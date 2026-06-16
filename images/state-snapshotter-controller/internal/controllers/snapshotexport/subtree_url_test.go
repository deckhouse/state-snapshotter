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

package snapshotexport

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

const subresPrefix = "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces"

// TestResolveRef_RootDefaulting proves an empty apiVersion/kind defaults to the namespaced root
// Snapshot and builds Snapshot-rooted (snapshots/<name>/...) subresource URLs.
func TestResolveRef_RootDefaulting(t *testing.T) {
	r := &SnapshotExportReconciler{}
	exp := export("e", types.UID("u1"))
	exp.Spec.SnapshotRef = storagev1alpha1.SnapshotReference{Name: "root"}

	rr, err := r.resolveRef(exp)
	if err != nil {
		t.Fatalf("resolveRef: %v", err)
	}
	if !rr.isRoot || rr.gvk.Kind != "Snapshot" {
		t.Fatalf("expected root Snapshot, got %+v", rr)
	}
	if got, want := rr.indexURL("ns"), subresPrefix+"/ns/snapshots/root/index"; got != want {
		t.Fatalf("indexURL = %q, want %q", got, want)
	}
	if got, want := rr.manifestsNodeURL("ns", "Snapshot--ns--root"), subresPrefix+"/ns/snapshots/root/manifests?node=Snapshot--ns--root"; got != want {
		t.Fatalf("manifestsNodeURL = %q, want %q", got, want)
	}
}

// TestResolveRef_DomainRoot proves a domain snapshot ref resolves its plural resource via the REST
// mapper and builds generic (<resource>/<name>/...) subresource URLs.
func TestResolveRef_DomainRoot(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualDiskSnapshot"}
	rm := meta.NewDefaultRESTMapper([]schema.GroupVersion{gvk.GroupVersion()})
	rm.Add(gvk, meta.RESTScopeNamespace)

	r := &SnapshotExportReconciler{RESTMapper: rm}
	exp := export("e", types.UID("u1"))
	exp.Spec.SnapshotRef = storagev1alpha1.SnapshotReference{APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind, Name: "disk-x"}

	rr, err := r.resolveRef(exp)
	if err != nil {
		t.Fatalf("resolveRef: %v", err)
	}
	if rr.isRoot {
		t.Fatal("domain ref must not be root")
	}
	if rr.resource != "demovirtualdisksnapshots" {
		t.Fatalf("resource = %q, want demovirtualdisksnapshots", rr.resource)
	}
	if got, want := rr.indexURL("ns"), subresPrefix+"/ns/demovirtualdisksnapshots/disk-x/index"; got != want {
		t.Fatalf("indexURL = %q, want %q", got, want)
	}
	if got, want := rr.manifestsNodeURL("ns", "DemoVirtualDiskSnapshot--ns--disk-x"),
		subresPrefix+"/ns/demovirtualdisksnapshots/disk-x/manifests?node=DemoVirtualDiskSnapshot--ns--disk-x"; got != want {
		t.Fatalf("manifestsNodeURL = %q, want %q", got, want)
	}
}

// TestResolveRef_DomainRootNoMapper proves a domain ref without a REST mapper is a transient (not
// InvalidSpec) failure so the export requeues until the CRD/mapper is available.
func TestResolveRef_DomainRootNoMapper(t *testing.T) {
	r := &SnapshotExportReconciler{}
	exp := export("e", types.UID("u1"))
	exp.Spec.SnapshotRef = storagev1alpha1.SnapshotReference{APIVersion: "demo.state-snapshotter.deckhouse.io/v1alpha1", Kind: "DemoVirtualDiskSnapshot", Name: "disk-x"}
	if _, err := r.resolveRef(exp); err == nil {
		t.Fatal("expected error resolving a domain ref without a REST mapper")
	}
}

// TestCollectNodes_AllNodesAndSize proves the walk yields every node (here the single root) with its
// data metadata and the volume size read from the source VolumeSnapshotContent restoreSize.
func TestCollectNodes_AllNodesAndSize(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	vscGVK := schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"}
	scheme.AddKnownTypeWithName(vscGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: vscGVK.Group, Version: vscGVK.Version, Kind: "VolumeSnapshotContentList"}, &unstructured.UnstructuredList{})

	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns"},
		Status:     storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: "c"},
	}
	meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{Type: snapshotpkg.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed"})

	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ManifestCheckpointName: "mcp",
			DataRefs: []storagev1alpha1.SnapshotDataBinding{{
				Artifact:         storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-1"},
				VolumeMode:       "Filesystem",
				FsType:           "ext4",
				StorageClassName: "fast",
			}},
		},
	}
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{Type: snapshotpkg.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed"})

	vsc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshotContent",
		"metadata":   map[string]interface{}{"name": "vsc-1"},
		"status":     map[string]interface{}{"restoreSize": int64(12345)},
	}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap, content, vsc).Build()
	r := &SnapshotExportReconciler{Client: cl, resolver: restore.NewResolver(cl)}

	rr, err := r.resolveRef(&storagev1alpha1.SnapshotExport{Spec: storagev1alpha1.SnapshotExportSpec{SnapshotRef: storagev1alpha1.SnapshotReference{Name: "root"}}})
	if err != nil {
		t.Fatalf("resolveRef: %v", err)
	}
	nodes, err := r.collectNodes(context.Background(), "ns", rr)
	if err != nil {
		t.Fatalf("collectNodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	n := nodes[0]
	if n.snapshotID != "Snapshot--ns--root" {
		t.Fatalf("snapshotID = %q", n.snapshotID)
	}
	if !n.hasData || n.volumeMode != "Filesystem" || n.fsType != "ext4" || n.storageClassName != "fast" {
		t.Fatalf("data metadata not propagated: %+v", n)
	}
	if n.size != 12345 {
		t.Fatalf("size = %d, want 12345 (from VSC restoreSize)", n.size)
	}
}
