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
	"strings"
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
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// Orchestration harness: out-of-process domains run outside core (the reference demo domain lives in the
// sds-unified-snapshots-poc module), so core compiles only GENERIC nodes in-process and DELEGATES every
// domain snapshot subtree to the domain's aggregated apiserver (DomainSubtreeRestorer). These tests
// validate that boundary against a fake client + a stub delegate: the resolver stops at domain nodes (it
// never reads their SnapshotContent), compileNode delegates them, and the spliced output stays apply-ready
// (post-order, deduped, fail-closed). Domain-internal restore logic (disk dataSource, covered PVCs) lives
// with the domain controller in its own repo, not here; the demo kind names below are synthetic stand-ins
// for the delegation predicate — core compiles no domain types.

const demoGroupV = "sds-unified-snapshots-poc.deckhouse.io/v1alpha1"

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
		gvk := schema.GroupVersionKind{Group: "sds-unified-snapshots-poc.deckhouse.io", Version: "v1alpha1", Kind: k}
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

	contentObj := rootBoundContent(content, mcp, rootName)
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
			SourceRef:   storagev1alpha1.SnapshotSubjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: o.pvcName, Namespace: "source-ns", UID: types.UID(o.pvcUID)},
			ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: o.vscName},
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
		if c.gvk.Group != "sds-unified-snapshots-poc.deckhouse.io" {
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

// ---------------------------------------------------------------------------
// The volume leg of a tree
// ---------------------------------------------------------------------------

// The cases below are about the node the delegation boundary moved to last: a volume node. It is a CSI
// VolumeSnapshot handle hanging off the root's childrenSnapshotRefs, and its restore is served by the
// module that owns the kind, not compiled here. They sit in this file rather than a new one because they
// belong to the SAME set of delegation cases as everything above — the delegate stub, the fixtures and the
// post-order assertions are shared, and splitting one set of cases across two test frameworks costs more
// than the local departure from Ginkgo.

// errDelegateUnavailable stands for "the owner did not answer". It is a sentinel rather than a fresh error so
// a case can assert that THIS error is what failed the request.
var errDelegateUnavailable = errors.New("volume node owner unavailable")

// missingDelegateMessage is the fragment compileDomainNode writes when no delegate is wired at all. It is
// matched as text because the refusal shares its wrapped sentinel (ErrContractViolation) with every other
// contract failure of the compiler, and it is exactly the neighbouring failures this case must not be
// satisfied by.
const missingDelegateMessage = "requires the domain restore delegate"

// isOutOfProcessRestoreKind is the predicate the running controller passes to the compiler, built over a
// registry as it looks with the CSI CRD installed and one CustomSnapshotDefinition domain registered.
//
// It is used instead of a hand-written closure on purpose: whether a volume node is delegated is decided
// by that predicate, and a local "kind == VolumeSnapshot" stand-in would keep these cases green after the
// predicate stopped saying so — which is the exact regression that would silently bring the compilation of
// a volume node back in-process.
func isOutOfProcessRestoreKind(t *testing.T) func(kind string) bool {
	t.Helper()

	reg := snapshot.NewGVKRegistry()
	for kind, groupVersion := range map[string]string{
		"Snapshot":                   "state-snapshotter.deckhouse.io/v1alpha1",
		"VolumeSnapshot":             "snapshot.storage.k8s.io/v1",
		"DemoVirtualMachineSnapshot": demoGroupV,
		"DemoVirtualDiskSnapshot":    demoGroupV,
	} {
		if err := reg.RegisterSnapshotGVK(kind, groupVersion); err != nil {
			t.Fatalf("register %s: %v", kind, err)
		}
	}
	return func(kind string) bool {
		return unifiedbootstrap.IsOutOfProcessDomainSnapshotKind(reg, kind)
	}
}

// materializeRootWithDelegatedVolumeNodes builds the generic root with childrenSnapshotRefs pointing at CSI
// VolumeSnapshot handles, and seeds NOTHING behind them — no VolumeSnapshot object, no child
// SnapshotContent, no checkpoint.
//
// That omission is the assertion: a delegated node is one core never reads. Every fixture the local
// orphan-PVC resolution needs is absent, so a regression that resolves the volume leg here instead of
// delegating it fails with a NotFound rather than quietly producing the same-looking output from the other
// implementation.
func materializeRootWithDelegatedVolumeNodes(t *testing.T, manifests []map[string]interface{}, vsNames []string) []client.Object {
	t.Helper()
	const rootName = "app"
	content := rootName + "-content"
	mcp := "mcp-" + rootName

	objs := materializeMCP(mcp, manifests)
	objs = append(objs, rootBoundContent(content, mcp, rootName))

	var childRefs []storagev1alpha1.SnapshotChildRef
	for _, vsName := range vsNames {
		childRefs = append(childRefs, storagev1alpha1.SnapshotChildRef{
			APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: vsName,
		})
	}
	objs = append(objs, rootSnapshotObj(rootName, content, childRefs))
	return objs
}

// TestBuildManifestsWithDataRestoration_DelegatesVolumeNodes proves the volume leg of a tree is answered by
// the module that owns the kind and spliced in, exactly like any other delegated subtree: the delegate is
// called with the CSI VolumeSnapshot identity, its objects reach the output in the target namespace, and
// they precede the root's own objects (post-order).
func TestBuildManifestsWithDataRestoration_DelegatesVolumeNodes(t *testing.T) {
	objs := materializeRootWithDelegatedVolumeNodes(t, []map[string]interface{}{cmManifest("config")}, []string{"vs-shared"})

	delegate := &recordingDelegate{byName: map[string][]map[string]interface{}{
		// What the owner returns is already apply-ready: the PVC bound to its VolumeSnapshot. The name
		// differs from anything the local resolution would produce, so the output identifies which side
		// compiled it.
		"vs-shared": {{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim",
			"metadata": map[string]interface{}{"name": "delegated-pvc"},
			"spec": map[string]interface{}{
				"dataSourceRef": map[string]interface{}{"apiGroup": "snapshot.storage.k8s.io", "kind": "VolumeSnapshot", "name": "vs-shared"},
			},
		}},
	}}

	cl := fake.NewClientBuilder().WithScheme(complexTreeScheme()).WithObjects(objs...).Build()
	log, _ := logger.NewLogger("error")
	arch := usecase.NewArchiveService(cl, cl, log)
	svc := NewService(cl, arch, delegate, isOutOfProcessRestoreKind(t))

	out, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "app", SnapshotNamespace: "source-ns", TargetNamespace: "restore-ns",
	})
	if err != nil {
		t.Fatalf("BuildManifestsWithDataRestoration: %v", err)
	}
	objects := decodeObjects(t, out)

	if len(delegate.calls) != 1 {
		t.Fatalf("expected the volume node to be delegated exactly once, got %d call(s)", len(delegate.calls))
	}
	call := delegate.calls[0]
	wantGVK := schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}
	if call.gvk != wantGVK {
		t.Fatalf("delegated identity = %v, want %v", call.gvk, wantGVK)
	}
	if call.namespace != "source-ns" || call.name != "vs-shared" || call.targetNamespace != "restore-ns" {
		t.Fatalf("delegated call = %s/%s -> %s, want source-ns/vs-shared -> restore-ns", call.namespace, call.name, call.targetNamespace)
	}

	if !hasObj(objects, "PersistentVolumeClaim", "delegated-pvc") {
		t.Fatalf("the owner's answer is missing from the output: %v", objects)
	}
	if !hasObj(objects, "ConfigMap", "config") {
		t.Fatal("root ConfigMap missing")
	}
	for _, o := range objects {
		u := unstructured.Unstructured{Object: o}
		if u.GetNamespace() != "restore-ns" {
			t.Fatalf("%s/%s namespace = %q, want restore-ns", u.GetKind(), u.GetName(), u.GetNamespace())
		}
	}
	// Post-order is unchanged by which side compiled the node: the volume node's objects come before the
	// objects of the parent that depends on them.
	if pvc, cm := idxOf(objects, "PersistentVolumeClaim", "delegated-pvc"), idxOf(objects, "ConfigMap", "config"); pvc >= cm {
		t.Fatalf("post-order violation: delegated PVC (idx %d) must precede the root's ConfigMap (idx %d)", pvc, cm)
	}
}

// TestBuildManifestsWithDataRestoration_VolumeNodeDelegateErrorFailsClosed proves the volume leg fails the
// WHOLE request when its owner cannot answer, with the same error as any other delegated subtree. A
// partial tree would be the worst possible outcome here: the objects that survived would apply, and the
// volume they need would silently not be there.
func TestBuildManifestsWithDataRestoration_VolumeNodeDelegateErrorFailsClosed(t *testing.T) {
	objs := materializeRootWithDelegatedVolumeNodes(t, []map[string]interface{}{cmManifest("config")}, []string{"vs-shared"})

	delegate := &recordingDelegate{err: errDelegateUnavailable}
	cl := fake.NewClientBuilder().WithScheme(complexTreeScheme()).WithObjects(objs...).Build()
	log, _ := logger.NewLogger("error")
	arch := usecase.NewArchiveService(cl, cl, log)
	svc := NewService(cl, arch, delegate, isOutOfProcessRestoreKind(t))

	out, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "app", SnapshotNamespace: "source-ns", TargetNamespace: "restore-ns",
	})
	if err == nil {
		t.Fatalf("expected the unavailable owner to fail the whole restore, got output %s", out)
	}
	if out != nil {
		t.Fatalf("expected no output alongside the error, got %s", out)
	}
	// The owner's own message must be what failed the request. Merely asserting "an error" would also be
	// satisfied by a compiler that never delegated and tripped over the fixtures this tree deliberately
	// omits — the failure would then be the local resolution's, and this case would prove nothing.
	if !errors.Is(err, errDelegateUnavailable) {
		t.Fatalf("expected the owner's error to fail the request, got %v", err)
	}
	if len(delegate.calls) != 1 {
		t.Fatalf("expected the volume node to be delegated once before failing, got %d call(s)", len(delegate.calls))
	}
}

// TestBuildManifestsWithDataRestoration_VolumeNodeNoDelegateFailsClosed proves a volume leg with no
// delegate wired is refused as a contract violation rather than dropped — the same answer the compiler
// gives for any other node whose owner it cannot reach.
func TestBuildManifestsWithDataRestoration_VolumeNodeNoDelegateFailsClosed(t *testing.T) {
	objs := materializeRootWithDelegatedVolumeNodes(t, []map[string]interface{}{cmManifest("config")}, []string{"vs-shared"})

	cl := fake.NewClientBuilder().WithScheme(complexTreeScheme()).WithObjects(objs...).Build()
	log, _ := logger.NewLogger("error")
	arch := usecase.NewArchiveService(cl, cl, log)
	svc := NewService(cl, arch, nil, isOutOfProcessRestoreKind(t))

	_, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "app", SnapshotNamespace: "source-ns", TargetNamespace: "restore-ns",
	})
	if err == nil {
		t.Fatal("expected fail-closed when the volume node has no delegate configured")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
	// Which refusal it is matters: this tree omits the fixtures the local resolution needs, so a compiler
	// that stopped delegating the volume leg would also fail here — with a contract violation of its own.
	// The message is what tells the two apart.
	if !strings.Contains(err.Error(), missingDelegateMessage) {
		t.Fatalf("expected the refusal to name the missing delegate (%q), got %v", missingDelegateMessage, err)
	}
}
