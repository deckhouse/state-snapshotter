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

package restore

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func scopeFilterScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = ssv1alpha1.AddToScheme(scheme)
	_ = storagev1alpha1.AddToScheme(scheme)
	vsGVK := schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}
	scheme.AddKnownTypeWithName(vsGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: vsGVK.Group, Version: vsGVK.Version, Kind: "VolumeSnapshotList"}, &unstructured.UnstructuredList{})
	return scheme
}

// newRootNodeService seeds a Ready root Snapshot "snap"/source-ns whose own MCP holds objects, plus
// optional childRefs (which scope=node must ignore), and returns the restore Service. The children are
// intentionally NOT seeded, so a subtree walk would fail — proving scope=node never reads them.
func newRootNodeService(t *testing.T, objects []map[string]interface{}, childRefs []storagev1alpha1.SnapshotChildRef) *Service {
	t.Helper()
	return newRootNodeServiceReady(t, objects, childRefs, metav1.Condition{
		Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: snapshot.ReasonCompleted,
	})
}

// newRootNodeServiceReady is newRootNodeService with an explicit root Snapshot Ready condition, so
// degraded-root (Ready=False) scope=node cases can reuse the same MCP/content plumbing. The bound
// SnapshotContent stays Ready — only the root Snapshot's own Ready condition varies.
func newRootNodeServiceReady(t *testing.T, objects []map[string]interface{}, childRefs []storagev1alpha1.SnapshotChildRef, rootReady metav1.Condition) *Service {
	t.Helper()
	scheme := scopeFilterScheme()
	log, _ := logger.NewLogger("error")

	data, checksum := encodeChunk(objects)
	chunk := &ssv1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: "chunk-root-0"},
		Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: "mcp-root", Index: 0, Data: data, Checksum: checksum, ObjectsCount: len(objects),
		},
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-root", UID: types.UID("uid-mcp-root")},
		Status: ssv1alpha1.ManifestCheckpointStatus{
			Chunks:       []ssv1alpha1.ChunkInfo{{Name: "chunk-root-0", Index: 0, Checksum: checksum}},
			TotalObjects: len(objects),
		},
	}
	meta.SetStatusCondition(&mcp.Status.Conditions, metav1.Condition{
		Type: ssv1alpha1.ManifestCheckpointConditionTypeReady, Status: metav1.ConditionTrue,
		Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
	})

	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "Snapshot",
				Namespace:  "source-ns",
				Name:       "snap",
			},
		},
		Status: storagev1alpha1.SnapshotContentStatus{ManifestCheckpointName: "mcp-root"},
	}
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed"})

	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "source-ns"},
		Status:     storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: "root-content", ChildrenSnapshotRefs: childRefs},
	}
	meta.SetStatusCondition(&snap.Status.Conditions, rootReady)

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(chunk, mcp, content, snap).Build()
	arch := usecase.NewArchiveService(cl, cl, log)
	return NewService(cl, arch, nil, nil)
}

func cm(name string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": name, "namespace": "source-ns"},
		"data":     map[string]interface{}{"k": "v"},
	}
}

// gadget builds a namespaced custom object of kind "Gadget" in the given API group. The name is fixed to
// "g" on purpose: the ambiguity tests need the SAME kind+name present in two different groups.
func gadget(group string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": group + "/v1", "kind": "Gadget",
		"metadata": map[string]interface{}{"name": "g", "namespace": "source-ns"},
	}
}

// TestScopeNode_ReturnsOnlyOwnManifests proves scope=node compiles ONLY the root node's own manifests,
// never the children — even though the root declares a child ref that is not resolvable in the client.
func TestScopeNode_ReturnsOnlyOwnManifests(t *testing.T) {
	svc := newRootNodeService(t, []map[string]interface{}{cm("app-config"), cm("other")}, []storagev1alpha1.SnapshotChildRef{
		{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "vs-missing"},
	})

	out, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "snap", SnapshotNamespace: "source-ns", TargetNamespace: "restore-ns", Scope: ScopeNode,
	})
	if err != nil {
		t.Fatalf("scope=node build: %v", err)
	}
	objs := decodeObjects(t, out)
	if len(objs) != 2 {
		t.Fatalf("scope=node must return only the 2 own-node ConfigMaps, got %d", len(objs))
	}
	for _, o := range objs {
		u := unstructured.Unstructured{Object: o}
		if u.GetKind() != "ConfigMap" {
			t.Fatalf("unexpected kind %q in node-only output", u.GetKind())
		}
		if u.GetNamespace() != "restore-ns" {
			t.Fatalf("namespace = %q, want restore-ns", u.GetNamespace())
		}
	}
}

// TestScopeNodeFilter_Hit proves the object filter returns exactly the matching object, sanitized and
// namespace-rewritten.
func TestScopeNodeFilter_Hit(t *testing.T) {
	svc := newRootNodeService(t, []map[string]interface{}{cm("app-config"), cm("other")}, nil)

	out, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "snap", SnapshotNamespace: "source-ns", TargetNamespace: "restore-ns",
		Scope: ScopeNode, FilterKind: "ConfigMap", FilterName: "app-config",
	})
	if err != nil {
		t.Fatalf("filter hit build: %v", err)
	}
	objs := decodeObjects(t, out)
	if len(objs) != 1 {
		t.Fatalf("filter must return exactly one object, got %d", len(objs))
	}
	u := unstructured.Unstructured{Object: objs[0]}
	if u.GetKind() != "ConfigMap" || u.GetName() != "app-config" {
		t.Fatalf("filter returned %s/%s, want ConfigMap/app-config", u.GetKind(), u.GetName())
	}
	if u.GetNamespace() != "restore-ns" {
		t.Fatalf("filtered object namespace = %q, want restore-ns", u.GetNamespace())
	}
}

// TestScopeNodeFilter_Miss proves a filter with no match yields ErrNotFound.
func TestScopeNodeFilter_Miss(t *testing.T) {
	svc := newRootNodeService(t, []map[string]interface{}{cm("app-config")}, nil)

	_, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "snap", SnapshotNamespace: "source-ns",
		Scope: ScopeNode, FilterKind: "ConfigMap", FilterName: "does-not-exist",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a filter miss, got %v", err)
	}
}

// TestScopeNodeFilter_SanitizedOutIsNotFound proves an object present in the MCP but removed by the
// restore-safe sanitizer (here a cluster-scoped ClusterRole, which is dropped) is a 404 — it is not
// restorable, so the filter must not find it.
func TestScopeNodeFilter_SanitizedOutIsNotFound(t *testing.T) {
	clusterRole := map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]interface{}{"name": "cr-x"}, // cluster-scoped: no namespace -> dropped
	}
	svc := newRootNodeService(t, []map[string]interface{}{cm("app-config"), clusterRole}, nil)

	_, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "snap", SnapshotNamespace: "source-ns",
		Scope: ScopeNode, FilterKind: "ClusterRole", FilterName: "cr-x",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a sanitizer-removed object, got %v", err)
	}
}

// TestScopeNodeFilter_AmbiguousWithoutAPIVersion proves that a kind+name matching two objects in
// different API groups, without an apiVersion, is a fail-closed ErrBadRequest.
func TestScopeNodeFilter_AmbiguousWithoutAPIVersion(t *testing.T) {
	svc := newRootNodeService(t, []map[string]interface{}{gadget("a.example.com"), gadget("b.example.com")}, nil)

	_, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "snap", SnapshotNamespace: "source-ns",
		Scope: ScopeNode, FilterKind: "Gadget", FilterName: "g",
	})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest for an ambiguous filter, got %v", err)
	}
}

// TestScopeNodeFilter_APIVersionFullDisambiguates proves a full "group/version" apiVersion resolves the
// ambiguity to a single object.
func TestScopeNodeFilter_APIVersionFullDisambiguates(t *testing.T) {
	svc := newRootNodeService(t, []map[string]interface{}{gadget("a.example.com"), gadget("b.example.com")}, nil)

	out, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "snap", SnapshotNamespace: "source-ns",
		Scope: ScopeNode, FilterKind: "Gadget", FilterName: "g", FilterAPIVersion: "a.example.com/v1",
	})
	if err != nil {
		t.Fatalf("full apiVersion filter: %v", err)
	}
	objs := decodeObjects(t, out)
	u := unstructured.Unstructured{Object: objs[0]}
	if len(objs) != 1 || u.GetAPIVersion() != "a.example.com/v1" {
		t.Fatalf("full apiVersion filter returned %#v, want the a.example.com/v1 Gadget", objs)
	}
}

// TestScopeNodeFilter_APIVersionBareGroupDisambiguates proves a bare "group" apiVersion (no version)
// resolves the ambiguity too.
func TestScopeNodeFilter_APIVersionBareGroupDisambiguates(t *testing.T) {
	svc := newRootNodeService(t, []map[string]interface{}{gadget("a.example.com"), gadget("b.example.com")}, nil)

	out, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "snap", SnapshotNamespace: "source-ns",
		Scope: ScopeNode, FilterKind: "Gadget", FilterName: "g", FilterAPIVersion: "b.example.com",
	})
	if err != nil {
		t.Fatalf("bare group filter: %v", err)
	}
	objs := decodeObjects(t, out)
	u := unstructured.Unstructured{Object: objs[0]}
	if len(objs) != 1 || u.GetAPIVersion() != "b.example.com/v1" {
		t.Fatalf("bare group filter returned %#v, want the b.example.com/v1 Gadget", objs)
	}
}

// Backward compatibility (no scope/filter -> whole subtree) is guarded by the unchanged
// TestBuildManifestsWithDataRestoration_NamespaceRootOrphanPVC in service_test.go, which builds with a
// zero Scope and expects the child-node PVC in the output. The resolver-level guard
// TestResolveRestoreNodeOnlyRoot_SkipsResolvableChildren additionally pins that only node-only skips the
// children.
