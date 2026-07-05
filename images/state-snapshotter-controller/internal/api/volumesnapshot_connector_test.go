/*
Copyright 2026 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package api //nolint:revive // package name matches internal/api directory

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func vsConnectorScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = ssv1alpha1.AddToScheme(scheme)
	_ = storagev1alpha1.AddToScheme(scheme)
	vsGVK := schema.GroupVersionKind{Group: snapshot.CSISnapshotGroup, Version: snapshot.CSISnapshotVersion, Kind: snapshot.KindVolumeSnapshot}
	scheme.AddKnownTypeWithName(vsGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: vsGVK.Group, Version: vsGVK.Version, Kind: "VolumeSnapshotList"}, &unstructured.UnstructuredList{})
	return scheme
}

// vsConnectorVolumeSnapshot builds an extended CSI VolumeSnapshot. importMode marks it as an import-mode
// target via the unified empty marker spec.source.import: {}; ready sets status.readyToUse for the
// restore path.
func vsConnectorVolumeSnapshot(name, ns, boundVSC, boundContent string, importMode, ready bool) *unstructured.Unstructured { //nolint:unparam // test fixture keeps uniform signature
	status := map[string]interface{}{}
	if boundVSC != "" {
		status["boundVolumeSnapshotContentName"] = boundVSC
	}
	if boundContent != "" {
		status["boundSnapshotContentName"] = boundContent
	}
	if ready {
		status["readyToUse"] = true
	}
	obj := map[string]interface{}{
		"apiVersion": snapshot.CSISnapshotAPIVersion,
		"kind":       snapshot.KindVolumeSnapshot,
		"metadata":   map[string]interface{}{"name": name, "namespace": ns, "uid": "uid-" + name},
		"status":     status,
	}
	if importMode {
		obj["spec"] = map[string]interface{}{"source": map[string]interface{}{"import": map[string]interface{}{}}}
	}
	return &unstructured.Unstructured{Object: obj}
}

func newVSConnectorServer(t *testing.T, cl client.Client) *httptest.Server {
	t.Helper()
	log, _ := logger.NewLogger("error")
	arch := usecase.NewArchiveService(cl, cl, log)
	agg := usecase.NewAggregatedNamespaceManifests(cl, arch, nil)
	rs := restore.NewService(cl, arch, nil, nil)
	importUpload := usecase.NewImportUploadService(cl)
	rh := NewRestoreHandler(cl, rs, log, agg, importUpload)
	mux := http.NewServeMux()
	rh.SetupVolumeSnapshotConnectorRoutes(mux)
	return httptest.NewServer(mux)
}

func TestVolumeSnapshotConnector_Discovery(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(vsConnectorScheme()).Build()
	srv := newVSConnectorServer(t, cl)
	defer srv.Close()

	groupBody := getRawResponse(t, srv.URL+"/apis/subresources.snapshot.storage.k8s.io", http.StatusOK)
	var group metav1.APIGroup
	if err := json.Unmarshal(groupBody, &group); err != nil {
		t.Fatalf("decode APIGroup: %v", err)
	}
	if group.Name != "subresources.snapshot.storage.k8s.io" {
		t.Fatalf("group name = %q", group.Name)
	}
	if group.PreferredVersion.Version != "v1" {
		t.Fatalf("preferred version = %q, want v1", group.PreferredVersion.Version)
	}

	listBody := getRawResponse(t, srv.URL+"/apis/subresources.snapshot.storage.k8s.io/v1", http.StatusOK)
	var list metav1.APIResourceList
	if err := json.Unmarshal(listBody, &list); err != nil {
		t.Fatalf("decode APIResourceList: %v", err)
	}
	if list.GroupVersion != "subresources.snapshot.storage.k8s.io/v1" {
		t.Fatalf("groupVersion = %q", list.GroupVersion)
	}
	want := map[string]bool{
		"volumesnapshots/manifests-download":                 false,
		"volumesnapshots/manifests-with-data-restoration":    false,
		"volumesnapshots/manifests-and-children-refs-upload": false,
	}
	for _, res := range list.APIResources {
		if _, ok := want[res.Name]; ok {
			want[res.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("APIResourceList missing %q", name)
		}
	}
}

// seedVolumeSnapshotLeaf creates the child volume node behind an extended VolumeSnapshot: an MCP holding
// the PVC manifest, a Ready child SnapshotContent with the single dataRef (-> VSC), and the VS handle.
func seedVolumeSnapshotLeaf(t *testing.T, cl client.Client, vsName string, ready bool) {
	t.Helper()
	pvc := map[string]interface{}{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]interface{}{"name": "orphan-pvc", "namespace": "ns1", "uid": "uid-pvc"},
		"spec":     map[string]interface{}{"accessModes": []interface{}{"ReadWriteOnce"}},
	}
	createReadyMCPForAPI(t, cl, "mcp-vol", []map[string]interface{}{pvc})

	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "vol-content"},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: "snapshot.storage.k8s.io/v1",
				Kind:       "VolumeSnapshot",
				Namespace:  "ns1",
				Name:       vsName,
			},
		},
		Status: storagev1alpha1.SnapshotContentStatus{
			ManifestCheckpointName: "mcp-vol",
			Data: &storagev1alpha1.SnapshotDataBinding{
				Source:   storagev1alpha1.SnapshotSubjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "orphan-pvc", Namespace: "ns1", UID: "uid-pvc"},
				Artifact: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-1"},
			},
		},
	}
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed"})
	if err := cl.Create(context.Background(), content); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(context.Background(), vsConnectorVolumeSnapshot(vsName, "ns1", "vsc-1", "vol-content", false, ready)); err != nil {
		t.Fatal(err)
	}
}

func TestVolumeSnapshotConnector_ManifestsDownload(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(vsConnectorScheme()).Build()
	seedVolumeSnapshotLeaf(t, cl, "vs-1", true)
	srv := newVSConnectorServer(t, cl)
	defer srv.Close()

	objects := getAggregatedObjects(t, srv.URL+"/apis/subresources.snapshot.storage.k8s.io/v1/namespaces/ns1/volumesnapshots/vs-1/manifests-download", http.StatusOK)
	if len(objects) != 1 || !containsKindName(objects, "PersistentVolumeClaim", "orphan-pvc") {
		t.Fatalf("manifests-download should return the single own-node PVC, got %#v", objects)
	}
}

func TestVolumeSnapshotConnector_ManifestsWithDataRestoration(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(vsConnectorScheme()).Build()
	seedVolumeSnapshotLeaf(t, cl, "vs-1", true)
	srv := newVSConnectorServer(t, cl)
	defer srv.Close()

	objects := getAggregatedObjects(t, srv.URL+"/apis/subresources.snapshot.storage.k8s.io/v1/namespaces/ns1/volumesnapshots/vs-1/manifests-with-data-restoration?targetNamespace=restore-ns", http.StatusOK)
	pi := -1
	for i, o := range objects {
		u := unstructured.Unstructured{Object: o}
		if u.GetKind() == "PersistentVolumeClaim" && u.GetName() == "orphan-pvc" {
			pi = i
		}
	}
	if pi < 0 {
		t.Fatalf("restore output missing PVC: %#v", objects)
	}
	if name, _, _ := unstructured.NestedString(objects[pi], "spec", "dataSourceRef", "name"); name != "vs-1" {
		t.Fatalf("PVC dataSourceRef.name = %q, want vs-1", name)
	}
	if ns := (&unstructured.Unstructured{Object: objects[pi]}).GetNamespace(); ns != "restore-ns" {
		t.Fatalf("PVC namespace = %q, want restore-ns", ns)
	}
}

func TestVolumeSnapshotConnector_Upload(t *testing.T) {
	vsStatusStub := &unstructured.Unstructured{}
	vsStatusStub.SetGroupVersionKind(volumeSnapshotGVK())
	cl := fake.NewClientBuilder().
		WithScheme(vsConnectorScheme()).
		WithStatusSubresource(&ssv1alpha1.ManifestCheckpoint{}, vsStatusStub).
		Build()
	if err := cl.Create(context.Background(), vsConnectorVolumeSnapshot("vs-import", "ns1", "", "", true, false)); err != nil {
		t.Fatal(err)
	}
	srv := newVSConnectorServer(t, cl)
	defer srv.Close()

	payload := `{"manifests":[{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"orphan-pvc","namespace":"ns1"}}],"childRefs":[]}`
	resp, err := http.Post(srv.URL+"/apis/subresources.snapshot.storage.k8s.io/v1/namespaces/ns1/volumesnapshots/vs-import/manifests-and-children-refs-upload", "application/json", bytes.NewReader([]byte(payload)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload status = %d", resp.StatusCode)
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["status"] != "Success" {
		t.Fatalf("upload response status = %v, want Success", out["status"])
	}
	if out["manifestCheckpointName"] == "" || out["manifestCheckpointName"] == nil {
		t.Fatalf("upload response missing manifestCheckpointName: %#v", out)
	}
}

// TestVolumeSnapshotConnector_UploadRejectsNonImportMode proves the connector reuses the import-mode
// guard: a VS without an import source must not have its manifests clobbered.
func TestVolumeSnapshotConnector_UploadRejectsNonImportMode(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(vsConnectorScheme()).Build()
	if err := cl.Create(context.Background(), vsConnectorVolumeSnapshot("vs-capture", "ns1", "vsc-1", "vol-content", false, true)); err != nil {
		t.Fatal(err)
	}
	srv := newVSConnectorServer(t, cl)
	defer srv.Close()

	payload := `{"manifests":[{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"x","namespace":"ns1"}}],"childRefs":[]}`
	resp, err := http.Post(srv.URL+"/apis/subresources.snapshot.storage.k8s.io/v1/namespaces/ns1/volumesnapshots/vs-capture/manifests-and-children-refs-upload", "application/json", bytes.NewReader([]byte(payload)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("upload of non-import VS status = %d, want 409", resp.StatusCode)
	}
}

// TestVolumeSnapshotConnector_ErrorPaths pins the status-code contract for the GET subresources:
// a missing VolumeSnapshot is 404 (both download and restore agree), and an existing-but-not-ready VS
// is a 409 on restore (fail-closed: no apply-ready PVC for a snapshot that is not usable yet).
func TestVolumeSnapshotConnector_ErrorPaths(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(vsConnectorScheme()).Build()
	// An existing-but-not-ready VS: status has no readyToUse, so the restore leaf check fails closed.
	if err := cl.Create(context.Background(), vsConnectorVolumeSnapshot("vs-notready", "ns1", "vsc-1", "vol-content", false, false)); err != nil {
		t.Fatal(err)
	}
	srv := newVSConnectorServer(t, cl)
	defer srv.Close()

	getStatus := func(url string) int {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	base := srv.URL + "/apis/subresources.snapshot.storage.k8s.io/v1/namespaces/ns1/volumesnapshots/"
	if got := getStatus(base + "missing/manifests-download"); got != http.StatusNotFound {
		t.Fatalf("manifests-download missing VS = %d, want 404", got)
	}
	if got := getStatus(base + "missing/manifests-with-data-restoration"); got != http.StatusNotFound {
		t.Fatalf("manifests-with-data-restoration missing VS = %d, want 404", got)
	}
	if got := getStatus(base + "vs-notready/manifests-with-data-restoration"); got != http.StatusConflict {
		t.Fatalf("manifests-with-data-restoration not-ready VS = %d, want 409", got)
	}
}

func TestVolumeSnapshotConnector_Routing(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(vsConnectorScheme()).Build()
	srv := newVSConnectorServer(t, cl)
	defer srv.Close()

	// Unknown subresource -> 404.
	resp, err := http.Get(srv.URL + "/apis/subresources.snapshot.storage.k8s.io/v1/namespaces/ns1/volumesnapshots/vs-1/bogus")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown subresource = %d, want 404", resp.StatusCode)
	}

	// Wrong method on a GET subresource -> 405.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/apis/subresources.snapshot.storage.k8s.io/v1/namespaces/ns1/volumesnapshots/vs-1/manifests-download", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST manifests-download = %d, want 405", resp.StatusCode)
	}

	// Wrong method on the upload (POST-only) subresource -> 405.
	resp, err = http.Get(srv.URL + "/apis/subresources.snapshot.storage.k8s.io/v1/namespaces/ns1/volumesnapshots/vs-1/manifests-and-children-refs-upload")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET upload = %d, want 405", resp.StatusCode)
	}
}
