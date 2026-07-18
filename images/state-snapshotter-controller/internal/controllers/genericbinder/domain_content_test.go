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

package genericbinder

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// The domain-capture request lifecycle (capture-leg eager-init, manifestCaptured/dataCaptured latches, the
// subtreeManifestsPersisted snapshot-mirror, and the MCR/VCR reap) moved to the SnapshotContentController
// aggregator (main-owned commonController, decision #10); its coverage lives in
// snapshotcontent/capture_legs_test.go. What remains on the binder is the leaf status.data export mirror
// (mirrorLeafDataFromContent) and the pure data-binding renderer — covered below.

const (
	domainTestNS      = "ns1"
	domainTestSnap    = "disk-snap"
	domainTestSnapUID = "disk-snap-uid"
	domainTestPVCName = "pvc-a"
	domainTestPVCUID  = "pvc-a-uid"
	domainTestContent = "domain-content"
	domainTestConUID  = "domain-content-uid"
	domainTestVSCName = "vsc-1"
)

// domainSnapshotGVK is a synthetic out-of-process domain snapshot kind. The binder handles domain leaves
// generically (unstructured, by status shape), so the test uses a placeholder GVK rather than any concrete
// domain's compiled types — the reference demo domain lives in the sds-unified-snapshots-poc repo.
var domainSnapshotGVK = schema.GroupVersionKind{Group: "example.domain.test", Version: "v1alpha1", Kind: "WidgetSnapshot"}

func domainTestVCRName() string { return vcpkg.SnapshotOwnedVCRName(types.UID(domainTestSnapUID)) }

// domainSnapshotStatusStub is an empty typed carrier used to register domainSnapshotGVK as a
// status-subresource kind on the fake client.
func domainSnapshotStatusStub() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(domainSnapshotGVK)
	return u
}

func domainTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add ss scheme: %v", err)
	}
	return scheme
}

func domainTestSnapshotContent() *storagev1alpha1.SnapshotContent {
	return &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: domainTestContent, UID: types.UID(domainTestConUID)},
	}
}

// domainTestDomainSnapshotUnstructured builds an out-of-process domain snapshot leaf as the binder sees it:
// an unstructured object whose status.captureState.domainSpecificController carries the VCR name.
func domainTestDomainSnapshotUnstructured(t *testing.T, vcrName string) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	obj.SetGroupVersionKind(domainSnapshotGVK)
	obj.SetNamespace(domainTestNS)
	obj.SetName(domainTestSnap)
	obj.SetUID(types.UID(domainTestSnapUID))
	if err := unstructured.SetNestedField(obj.Object, vcrName,
		"status", "captureState", "domainSpecificController", "volumeCaptureRequestName"); err != nil {
		t.Fatalf("set volumeCaptureRequestName: %v", err)
	}
	return obj
}

// mirrorLeafDataFromContent copies the bound SnapshotContent's self-contained data binding verbatim onto
// the namespaced data leaf's top-level status.data (source + artifact + volume metadata) and writes NO
// flat top-level storageClassName/size/volumeMode mirrors (folded into status.data in wave5).
func TestMirrorLeafDataFromContent_WritesTopLevelStatusData(t *testing.T) {
	ctx := context.Background()
	scheme := domainTestScheme(t)
	domainObj := domainTestDomainSnapshotUnstructured(t, domainTestVCRName())
	content := domainTestSnapshotContent()
	content.Status.Data = &storagev1alpha1.SnapshotDataBinding{
		SourceRef: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: domainTestPVCName,
			Namespace: domainTestNS, UID: types.UID(domainTestPVCUID),
		},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent",
			Name: domainTestVSCName, UID: types.UID("vsc-uid-1"),
		},
		VolumeMode:       string(corev1.PersistentVolumeFilesystem),
		AccessModes:      []string{string(corev1.ReadWriteOnce)},
		StorageClassName: "sc-a",
		Size:             "10Gi",
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(domainSnapshotStatusStub(), &storagev1alpha1.SnapshotContent{}).
		WithObjects(content, domainObj).
		Build()
	r := &GenericSnapshotBinderController{Client: cl, APIReader: cl, Scheme: scheme}

	if err := r.mirrorLeafDataFromContent(ctx, domainObj, domainTestContent, ""); err != nil {
		t.Fatalf("mirrorLeafDataFromContent: %v", err)
	}

	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(domainSnapshotGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: domainTestNS, Name: domainTestSnap}, fresh); err != nil {
		t.Fatalf("get domain snapshot: %v", err)
	}
	data, found, _ := unstructured.NestedMap(fresh.Object, "status", "data")
	if !found {
		t.Fatalf("expected status.data to be mirrored")
	}
	if srcUID, _, _ := unstructured.NestedString(data, "sourceRef", "uid"); srcUID != domainTestPVCUID {
		t.Fatalf("status.data.sourceRef.uid = %q, want %q", srcUID, domainTestPVCUID)
	}
	if artName, _, _ := unstructured.NestedString(data, "artifactRef", "name"); artName != domainTestVSCName {
		t.Fatalf("status.data.artifactRef.name = %q, want %q", artName, domainTestVSCName)
	}
	if sc, _, _ := unstructured.NestedString(data, "storageClassName"); sc != "sc-a" {
		t.Fatalf("status.data.storageClassName = %q, want sc-a", sc)
	}
	if size, _, _ := unstructured.NestedString(data, "size"); size != "10Gi" {
		t.Fatalf("status.data.size = %q, want 10Gi", size)
	}
	// The flat top-level mirrors must be gone (folded into status.data).
	if _, found, _ := unstructured.NestedString(fresh.Object, "status", "storageClassName"); found {
		t.Fatalf("flat status.storageClassName must not be written")
	}
	if _, found, _ := unstructured.NestedString(fresh.Object, "status", "volumeMode"); found {
		t.Fatalf("flat status.volumeMode must not be written")
	}
	if _, found, _ := unstructured.NestedString(fresh.Object, "status", "size"); found {
		t.Fatalf("flat status.size must not be written")
	}
}

// On import the content data carries no storageClassName; the caller passes it from
// DataImport.spec.storageClassName as scOverride, which must land in the mirrored status.data.
func TestMirrorLeafDataFromContent_ScOverride(t *testing.T) {
	ctx := context.Background()
	scheme := domainTestScheme(t)
	domainObj := domainTestDomainSnapshotUnstructured(t, domainTestVCRName())
	content := domainTestSnapshotContent()
	content.Status.Data = &storagev1alpha1.SnapshotDataBinding{
		SourceRef:   storagev1alpha1.SnapshotSubjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: domainTestPVCName, Namespace: domainTestNS, UID: types.UID(domainTestPVCUID)},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: domainTestVSCName},
		Size:        "5Gi",
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(domainSnapshotStatusStub(), &storagev1alpha1.SnapshotContent{}).
		WithObjects(content, domainObj).
		Build()
	r := &GenericSnapshotBinderController{Client: cl, APIReader: cl, Scheme: scheme}
	if err := r.mirrorLeafDataFromContent(ctx, domainObj, domainTestContent, "sc-import"); err != nil {
		t.Fatalf("mirrorLeafDataFromContent: %v", err)
	}
	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(domainSnapshotGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: domainTestNS, Name: domainTestSnap}, fresh); err != nil {
		t.Fatalf("get domain snapshot: %v", err)
	}
	if sc, _, _ := unstructured.NestedString(fresh.Object, "status", "data", "storageClassName"); sc != "sc-import" {
		t.Fatalf("scOverride not applied: status.data.storageClassName = %q, want sc-import", sc)
	}
}

// SnapshotDataBindingToUnstructuredMap renders sourceRef/artifactRef always, omits empty optionals, and
// converts AccessModes to a JSON-typed []interface{} (required by unstructured.SetNestedMap).
func TestSnapshotDataBindingToMap(t *testing.T) {
	m := snapshotcontent.SnapshotDataBindingToUnstructuredMap(&storagev1alpha1.SnapshotDataBinding{
		SourceRef:   storagev1alpha1.SnapshotSubjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc", UID: types.UID("u1")},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc"},
		AccessModes: []string{"ReadWriteOnce"},
	})
	if _, ok := m["sourceRef"].(map[string]interface{}); !ok {
		t.Fatalf("sourceRef must be a map, got %#v", m["sourceRef"])
	}
	if _, ok := m["artifactRef"].(map[string]interface{}); !ok {
		t.Fatalf("artifactRef must be a map, got %#v", m["artifactRef"])
	}
	am, ok := m["accessModes"].([]interface{})
	if !ok || len(am) != 1 || am[0] != "ReadWriteOnce" {
		t.Fatalf("accessModes must be []interface{}{\"ReadWriteOnce\"}, got %#v", m["accessModes"])
	}
	// Empty optionals are omitted.
	if _, ok := m["storageClassName"]; ok {
		t.Fatalf("empty storageClassName must be omitted")
	}
	if _, ok := m["size"]; ok {
		t.Fatalf("empty size must be omitted")
	}
	// The Namespace on sourceRef was empty -> omitted.
	if src := m["sourceRef"].(map[string]interface{}); func() bool { _, ok := src["namespace"]; return ok }() {
		t.Fatalf("empty sourceRef.namespace must be omitted")
	}
}
