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
	"encoding/json"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// Orchestration harness: after the demo controllers moved to a separate binary, core compiles only
// GENERIC nodes in-process and DELEGATES every domain snapshot subtree to the domain controller's
// aggregated apiserver (DomainSubtreeRestorer). These tests validate that boundary against a fake
// client + a stub delegate: the resolver stops at domain nodes (it never reads their SnapshotContent),
// compileNode delegates them, and the spliced output stays apply-ready (post-order, deduped,
// fail-closed). Domain-internal restore logic (disk dataSource, covered PVCs) is tested in the domain
// apiserver package (internal/domainapi), not here.

const demoGroupV = "demo.state-snapshotter.deckhouse.io/v1alpha1"

func isDemoSnapshotKind(kind string) bool {
	return kind == "DemoVirtualMachineSnapshot" || kind == "DemoVirtualDiskSnapshot"
}

// orphanLeaf is a namespace-residual PVC captured at the root via a CSI VolumeSnapshot visibility leaf.
type orphanLeaf struct {
	pvcName string
	pvcUID  string
	vsName  string
	vscName string
}

// domainChild is a top-level domain snapshot hanging off the root Snapshot's childrenSnapshotRefs.
type domainChild struct {
	kind string
	name string
}

func complexTreeScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = ssv1alpha1.AddToScheme(scheme)
	_ = storagev1alpha1.AddToScheme(scheme)
	for _, k := range []string{"DemoVirtualMachineSnapshot", "DemoVirtualDiskSnapshot"} {
		gvk := schema.GroupVersionKind{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: k}
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: k + "List"}, &unstructured.UnstructuredList{})
	}
	vsGVK := schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}
	scheme.AddKnownTypeWithName(vsGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: vsGVK.Group, Version: vsGVK.Version, Kind: "VolumeSnapshotList"}, &unstructured.UnstructuredList{})
	return scheme
}

func cmManifest(name string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": name, "namespace": "source-ns"},
		"data":     map[string]interface{}{"k": "v"},
	}
}

func pvcManifestRaw(name, uid string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]interface{}{"name": name, "namespace": "source-ns", "uid": uid},
		"spec":     map[string]interface{}{"accessModes": []interface{}{"ReadWriteOnce"}},
	}
}

// materializeMCP builds a Ready ManifestCheckpoint + its single content chunk for the given manifests.
func materializeMCP(mcpName string, manifests []map[string]interface{}) []client.Object {
	data, checksum := encodeChunk(manifests)
	mcpObj := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: mcpName, UID: types.UID("uid-" + mcpName)},
		Spec:       ssv1alpha1.ManifestCheckpointSpec{},
		Status: ssv1alpha1.ManifestCheckpointStatus{
			Chunks:       []ssv1alpha1.ChunkInfo{{Name: mcpName + "-chunk-0", Index: 0, Checksum: checksum}},
			TotalObjects: len(manifests),
		},
	}
	meta.SetStatusCondition(&mcpObj.Status.Conditions, metav1.Condition{
		Type: ssv1alpha1.ManifestCheckpointConditionTypeReady, Status: metav1.ConditionTrue,
		Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
	})
	return []client.Object{
		&ssv1alpha1.ManifestCheckpointContentChunk{
			ObjectMeta: metav1.ObjectMeta{Name: mcpName + "-chunk-0"},
			Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
				CheckpointName: mcpName, Index: 0, Data: data, Checksum: checksum, ObjectsCount: len(manifests),
			},
		},
		mcpObj,
	}
}

// materializeRoot builds the GENERIC root: MCP + chunk + SnapshotContent + root Snapshot whose
// childrenSnapshotRefs point at orphan-PVC VolumeSnapshot handles and the given top-level domain
// snapshots. Variant A: the root content is an aggregator (no dataRef); each orphan PVC is its own
// standalone child volume node (own MCP holding that PVC's manifest + a single dataRef), reachable via
// the VolumeSnapshot handle's status.boundSnapshotContentName. The root MCP therefore holds only the
// non-orphan manifests. It deliberately does NOT seed the domain snapshot CRs: the resolver
// short-circuits domain kinds at the ref level before any Get, so core must restore the subtree
// without reading them (no demo RBAC). A regression to a post-Get short-circuit would fail with NotFound.
func materializeRoot(t *testing.T, manifests []map[string]interface{}, orphans []orphanLeaf, children []domainChild, rootNotReady bool) []client.Object {
	t.Helper()
	const rootName = "app"
	content := rootName + "-content"
	mcp := "mcp-" + rootName

	objs := materializeMCP(mcp, manifests)

	contentObj := readySnapshotContent(content, mcp, nil)
	if rootNotReady {
		meta.SetStatusCondition(&contentObj.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Pending"})
	}
	objs = append(objs, contentObj)

	var childRefs []storagev1alpha1.SnapshotChildRef
	for _, o := range orphans {
		// Standalone child volume node: own MCP carrying this orphan PVC's manifest + a single dataRef.
		childContentName := content + "-vol-" + o.vsName
		childMCP := "mcp-vol-" + o.vsName
		objs = append(objs, materializeMCP(childMCP, []map[string]interface{}{pvcManifestRaw(o.pvcName, o.pvcUID)})...)
		objs = append(objs, orphanChildContent(childContentName, childMCP, o.vsName, &storagev1alpha1.SnapshotDataBinding{
			Source:   storagev1alpha1.SnapshotSubjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: o.pvcName, Namespace: "source-ns", UID: types.UID(o.pvcUID)},
			Artifact: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: o.vscName},
		}))
		objs = append(objs, volumeSnapshotObj(o.vsName, o.vscName, childContentName))
		childRefs = append(childRefs, storagev1alpha1.SnapshotChildRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: o.vsName})
	}
	for _, c := range children {
		childRefs = append(childRefs, storagev1alpha1.SnapshotChildRef{APIVersion: demoGroupV, Kind: c.kind, Name: c.name})
	}
	objs = append(objs, rootSnapshotObj("app", content, childRefs))
	return objs
}

// recordingDelegate is a stub DomainSubtreeRestorer: it records every delegated call and returns
// canned objects keyed by snapshot name, namespaced into the requested targetNamespace.
type recordingDelegate struct {
	byName map[string][]map[string]interface{}
	err    error
	calls  []delegateCall
}

type delegateCall struct {
	gvk             schema.GroupVersionKind
	namespace       string
	name            string
	targetNamespace string
}

func (d *recordingDelegate) RestoreDomainSubtree(_ context.Context, gvk schema.GroupVersionKind, namespace, name, targetNamespace string) ([]unstructured.Unstructured, error) {
	d.calls = append(d.calls, delegateCall{gvk: gvk, namespace: namespace, name: name, targetNamespace: targetNamespace})
	if d.err != nil {
		return nil, d.err
	}
	raw := d.byName[name]
	out := make([]unstructured.Unstructured, 0, len(raw))
	for _, o := range raw {
		u := unstructured.Unstructured{Object: deepCopyMap(o)}
		u.SetNamespace(targetNamespace)
		out = append(out, u)
	}
	return out, nil
}

func deepCopyMap(in map[string]interface{}) map[string]interface{} {
	return (&unstructured.Unstructured{Object: in}).DeepCopy().Object
}

func newServiceWithDelegate(t *testing.T, objs []client.Object, delegate DomainSubtreeRestorer) *Service {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(complexTreeScheme()).WithObjects(objs...).Build()
	log, _ := logger.NewLogger("error")
	arch := usecase.NewArchiveService(cl, cl, log)
	return NewService(cl, arch, delegate, isDemoSnapshotKind)
}

func decodeObjects(t *testing.T, raw []byte) []map[string]interface{} {
	t.Helper()
	var objects []map[string]interface{}
	if err := json.Unmarshal(raw, &objects); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	return objects
}

func idxOf(objs []map[string]interface{}, kind, name string) int {
	for i, o := range objs {
		u := unstructured.Unstructured{Object: o}
		if u.GetKind() == kind && u.GetName() == name {
			return i
		}
	}
	return -1
}

func hasObj(objs []map[string]interface{}, kind, name string) bool {
	return idxOf(objs, kind, name) >= 0
}

// TestBuildManifestsWithDataRestoration_DelegatesDomainSubtrees compiles a generic root (ConfigMap +
// orphan PVC) with two top-level domain children (a VM snapshot and a standalone disk snapshot). Core
// compiles the root in-process and delegates each domain subtree; the spliced output is apply-ready,
// post-order, and in the target namespace.
func TestBuildManifestsWithDataRestoration_DelegatesDomainSubtrees(t *testing.T) {
	// Variant A: the orphan PVC manifest now lives on its own child volume node's MCP (materializeRoot
	// seeds it from the orphanLeaf), so the root MCP carries only the ConfigMap.
	objs := materializeRoot(t,
		[]map[string]interface{}{cmManifest("config")},
		[]orphanLeaf{{pvcName: "shared-pvc", pvcUID: "uid-shared", vsName: "vs-shared", vscName: "vsc-shared"}},
		[]domainChild{
			{kind: "DemoVirtualMachineSnapshot", name: "vm-a-snap"},
			{kind: "DemoVirtualDiskSnapshot", name: "disk-standalone-snap"},
		}, false)

	delegate := &recordingDelegate{byName: map[string][]map[string]interface{}{
		// The domain apiserver returns already-restored objects for the whole subtree.
		"vm-a-snap": {
			{"apiVersion": demoGroupV, "kind": "DemoVirtualDisk", "metadata": map[string]interface{}{"name": "disk-a1"}, "spec": map[string]interface{}{"dataSource": map[string]interface{}{"name": "disk-a1-snap"}}},
			{"apiVersion": demoGroupV, "kind": "DemoVirtualMachine", "metadata": map[string]interface{}{"name": "vm-a"}},
		},
		"disk-standalone-snap": {
			{"apiVersion": demoGroupV, "kind": "DemoVirtualDisk", "metadata": map[string]interface{}{"name": "disk-standalone"}, "spec": map[string]interface{}{"dataSource": map[string]interface{}{"name": "disk-standalone-snap"}}},
		},
	}}

	svc := newServiceWithDelegate(t, objs, delegate)
	out, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "app", SnapshotNamespace: "source-ns", TargetNamespace: "restore-ns",
	})
	if err != nil {
		t.Fatalf("BuildManifestsWithDataRestoration: %v", err)
	}
	objects := decodeObjects(t, out)

	// Generic root compiled in-process: ConfigMap + orphan PVC with a dataSourceRef.
	if !hasObj(objects, "ConfigMap", "config") {
		t.Fatal("root ConfigMap missing")
	}
	pi := idxOf(objects, "PersistentVolumeClaim", "shared-pvc")
	if pi < 0 {
		t.Fatal("orphan PVC missing from output")
	}
	if name, _, _ := unstructured.NestedString(objects[pi], "spec", "dataSourceRef", "name"); name != "vs-shared" {
		t.Fatalf("orphan PVC dataSourceRef.name = %q, want vs-shared", name)
	}

	// Delegated domain objects spliced in.
	for _, kn := range []struct{ kind, name string }{
		{"DemoVirtualMachine", "vm-a"}, {"DemoVirtualDisk", "disk-a1"}, {"DemoVirtualDisk", "disk-standalone"},
	} {
		if !hasObj(objects, kn.kind, kn.name) {
			t.Fatalf("delegated object %s/%s missing", kn.kind, kn.name)
		}
	}

	// Everything is in the target namespace and carries no control-plane kinds.
	for _, o := range objects {
		u := unstructured.Unstructured{Object: o}
		switch u.GetKind() {
		case "VolumeSnapshot", "VolumeSnapshotContent", "Snapshot", "SnapshotContent":
			t.Fatalf("control-plane kind %s must not be emitted", u.GetKind())
		}
		if u.GetNamespace() != "restore-ns" {
			t.Fatalf("%s/%s namespace = %q, want restore-ns", u.GetKind(), u.GetName(), u.GetNamespace())
		}
	}

	// Each top-level domain child was delegated exactly once with the right identity + targetNamespace.
	if len(delegate.calls) != 2 {
		t.Fatalf("expected 2 delegate calls, got %d", len(delegate.calls))
	}
	for _, c := range delegate.calls {
		if c.namespace != "source-ns" || c.targetNamespace != "restore-ns" {
			t.Fatalf("delegate call %s/%s: namespace=%q targetNamespace=%q", c.gvk.Kind, c.name, c.namespace, c.targetNamespace)
		}
		if c.gvk.Group != "demo.state-snapshotter.deckhouse.io" {
			t.Fatalf("delegate call gvk group = %q, want demo group", c.gvk.Group)
		}
	}

	// Post-order: delegated subtree objects precede the root's own objects.
	mustBefore := func(childKind, childName, parentKind, parentName string) {
		ci, pj := idxOf(objects, childKind, childName), idxOf(objects, parentKind, parentName)
		if ci < 0 || pj < 0 || ci >= pj {
			t.Fatalf("post-order violation: %s/%s (idx %d) must precede %s/%s (idx %d)", childKind, childName, ci, parentKind, parentName, pj)
		}
	}
	mustBefore("DemoVirtualMachine", "vm-a", "ConfigMap", "config")
	mustBefore("DemoVirtualDisk", "disk-standalone", "ConfigMap", "config")
}

// TestBuildManifestsWithDataRestorationForNode_DomainKindFullyDelegated proves the per-node endpoint
// for a domain kind delegates the whole subtree to the domain apiserver (core compiles nothing for it).
func TestBuildManifestsWithDataRestorationForNode_DomainKindFullyDelegated(t *testing.T) {
	objs := materializeRoot(t,
		[]map[string]interface{}{cmManifest("config")},
		nil,
		[]domainChild{{kind: "DemoVirtualMachineSnapshot", name: "vm-a-snap"}}, false)

	delegate := &recordingDelegate{byName: map[string][]map[string]interface{}{
		"vm-a-snap": {{"apiVersion": demoGroupV, "kind": "DemoVirtualMachine", "metadata": map[string]interface{}{"name": "vm-a"}}},
	}}
	svc := newServiceWithDelegate(t, objs, delegate)

	gvk := schema.GroupVersionKind{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualMachineSnapshot"}
	out, err := svc.BuildManifestsWithDataRestorationForNode(context.Background(), gvk, Options{
		SnapshotName: "vm-a-snap", SnapshotNamespace: "source-ns", TargetNamespace: "restore-ns",
	})
	if err != nil {
		t.Fatalf("BuildManifestsWithDataRestorationForNode: %v", err)
	}
	objects := decodeObjects(t, out)
	if !hasObj(objects, "DemoVirtualMachine", "vm-a") {
		t.Fatal("delegated VM subtree must contain vm-a")
	}
	if hasObj(objects, "ConfigMap", "config") {
		t.Fatal("root namespace objects must not be compiled for a per-node domain restore")
	}
	if len(delegate.calls) != 1 || delegate.calls[0].name != "vm-a-snap" {
		t.Fatalf("expected exactly one delegate call for vm-a-snap, got %#v", delegate.calls)
	}
}

// TestBuildManifestsWithDataRestoration_DuplicateAcrossDelegatedFailsClosed proves the final dedup
// rejects a delegated object whose identity collides with a root object.
func TestBuildManifestsWithDataRestoration_DuplicateAcrossDelegatedFailsClosed(t *testing.T) {
	objs := materializeRoot(t,
		[]map[string]interface{}{cmManifest("dup")},
		nil,
		[]domainChild{{kind: "DemoVirtualDiskSnapshot", name: "disk-x-snap"}}, false)

	delegate := &recordingDelegate{byName: map[string][]map[string]interface{}{
		// Same identity (v1/ConfigMap/restore-ns/dup) as the root ConfigMap after namespacing.
		"disk-x-snap": {cmManifest("dup")},
	}}
	svc := newServiceWithDelegate(t, objs, delegate)
	_, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "app", SnapshotNamespace: "source-ns", TargetNamespace: "restore-ns",
	})
	if err == nil {
		t.Fatal("expected duplicate object across delegated + root to fail closed")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}

// TestBuildManifestsWithDataRestoration_DelegateErrorFailsClosed proves a domain apiserver error
// aborts the whole restore (no partial apply-ready output).
func TestBuildManifestsWithDataRestoration_DelegateErrorFailsClosed(t *testing.T) {
	objs := materializeRoot(t,
		[]map[string]interface{}{cmManifest("config")},
		nil,
		[]domainChild{{kind: "DemoVirtualDiskSnapshot", name: "disk-x-snap"}}, false)

	delegate := &recordingDelegate{err: errors.New("domain apiserver unavailable")}
	svc := newServiceWithDelegate(t, objs, delegate)
	_, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "app", SnapshotNamespace: "source-ns", TargetNamespace: "restore-ns",
	})
	if err == nil {
		t.Fatal("expected delegate error to fail the whole restore")
	}
}

// TestBuildManifestsWithDataRestoration_NoDelegateFailsClosed proves that a domain node with no
// delegate configured fails closed rather than silently dropping the subtree.
func TestBuildManifestsWithDataRestoration_NoDelegateFailsClosed(t *testing.T) {
	objs := materializeRoot(t,
		[]map[string]interface{}{cmManifest("config")},
		nil,
		[]domainChild{{kind: "DemoVirtualDiskSnapshot", name: "disk-x-snap"}}, false)

	cl := fake.NewClientBuilder().WithScheme(complexTreeScheme()).WithObjects(objs...).Build()
	log, _ := logger.NewLogger("error")
	arch := usecase.NewArchiveService(cl, cl, log)
	// isDomainKind set, but no delegate wired.
	svc := NewService(cl, arch, nil, isDemoSnapshotKind)
	_, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "app", SnapshotNamespace: "source-ns", TargetNamespace: "restore-ns",
	})
	if err == nil {
		t.Fatal("expected fail-closed when a domain node has no delegate configured")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}

// TestBuildManifestsWithDataRestoration_RootNotReadyFailsClosed proves a not-Ready root content aborts
// the whole compilation.
func TestBuildManifestsWithDataRestoration_RootNotReadyFailsClosed(t *testing.T) {
	objs := materializeRoot(t,
		[]map[string]interface{}{cmManifest("config")},
		nil,
		[]domainChild{{kind: "DemoVirtualDiskSnapshot", name: "disk-x-snap"}}, true)

	delegate := &recordingDelegate{byName: map[string][]map[string]interface{}{"disk-x-snap": {cmManifest("d")}}}
	svc := newServiceWithDelegate(t, objs, delegate)
	_, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "app", SnapshotNamespace: "source-ns", TargetNamespace: "restore-ns",
	})
	if err == nil {
		t.Fatal("expected not-Ready root content to fail closed")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected ErrNotReady, got %v", err)
	}
}
