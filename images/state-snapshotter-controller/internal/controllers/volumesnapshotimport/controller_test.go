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

package volumesnapshotimport

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// importSnapshotSourceRef must target the orphan PVC (not the VolumeSnapshot handle): it is published as
// status.sourceRef and the aggregator builds the dataRef source from it. The restore compiler matches
// a captured PVC manifest to its dataRef by PVC identity/UID — a VolumeSnapshot-targeted source would never
// match and the PVC would be emitted data-less (contract violation).
func TestImportSnapshotSourceRef_TargetsPVC(t *testing.T) {
	pvc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":      "bk-pvc",
			"namespace": "source-ns",
			"uid":       "pvc-uid-123",
		},
	}}

	src := importSnapshotSourceRef(pvc)

	if string(src.UID) != "pvc-uid-123" {
		t.Fatalf("Source.UID must be the PVC uid, got %q", src.UID)
	}
	if src.Kind != "PersistentVolumeClaim" {
		t.Fatalf("Source.Kind must be PersistentVolumeClaim, got %q", src.Kind)
	}
	if src.APIVersion != "v1" {
		t.Fatalf("Source.APIVersion must be v1, got %q", src.APIVersion)
	}
	if src.Name != "bk-pvc" || src.Namespace != "source-ns" {
		t.Fatalf("Source identity mismatch: %s/%s", src.Namespace, src.Name)
	}
}

// isImportModeVolumeSnapshot keys solely on the unified enum spec.mode: Import (parity with every other
// snapshot kind); capture/pre-provisioned VS (mode absent or Capture) are not ours to bind.
func TestIsImportModeVolumeSnapshot(t *testing.T) {
	cases := []struct {
		name   string
		mode   string
		source map[string]interface{}
		want   bool
	}{
		{name: "mode Import (source omitted — canonical)", mode: "Import", source: nil, want: true},
		{name: "mode Import (empty source from a typed client)", mode: "Import", source: map[string]interface{}{}, want: true},
		{name: "mode Capture (persistentVolumeClaimName)", mode: "Capture", source: map[string]interface{}{"persistentVolumeClaimName": "pvc-1"}, want: false},
		{name: "mode absent (CRD default Capture)", source: map[string]interface{}{"persistentVolumeClaimName": "pvc-1"}, want: false},
		{name: "pre-provisioned (volumeSnapshotContentName)", mode: "Capture", source: map[string]interface{}{"volumeSnapshotContentName": "vsc-1"}, want: false},
		{name: "no source, no mode", source: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := map[string]interface{}{}
			if tc.mode != "" {
				spec["mode"] = tc.mode
			}
			if tc.source != nil {
				spec["source"] = tc.source
			}
			vs := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
			if got := isImportModeVolumeSnapshot(vs); got != tc.want {
				t.Fatalf("isImportModeVolumeSnapshot = %v, want %v", got, tc.want)
			}
		})
	}
}

// resolveDataImportArtifact distinguishes ready (VSC produced), pending (no artifact yet), and terminal
// (a non-VSC artifact the extended-VS legacy binding cannot represent).
func TestResolveDataImportArtifact(t *testing.T) {
	newDI := func(kind, name string) *unstructured.Unstructured {
		ref := map[string]interface{}{}
		if kind != "" {
			ref["kind"] = kind
		}
		if name != "" {
			ref["name"] = name
		}
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "di-1", "namespace": "ns1"},
			"status":   map[string]interface{}{"data": map[string]interface{}{"artifactRef": ref}},
		}}
	}
	r := &Controller{}

	t.Run("ready VSC artifact", func(t *testing.T) {
		vscName, ready, terminal := r.resolveDataImportArtifact(newDI(snapshotpkg.KindVolumeSnapshotContent, "vsc-7"))
		if !ready || vscName != "vsc-7" || terminal != "" {
			t.Fatalf("got vsc=%q ready=%v terminal=%q, want vsc-7/true/empty", vscName, ready, terminal)
		}
	})
	t.Run("pending (no artifact name)", func(t *testing.T) {
		vscName, ready, terminal := r.resolveDataImportArtifact(newDI(snapshotpkg.KindVolumeSnapshotContent, ""))
		if ready || vscName != "" || terminal != "" {
			t.Fatalf("got vsc=%q ready=%v terminal=%q, want empty/false/empty", vscName, ready, terminal)
		}
	})
	t.Run("terminal (non-VSC artifact)", func(t *testing.T) {
		vscName, ready, terminal := r.resolveDataImportArtifact(newDI("PersistentVolume", "pv-9"))
		if ready || vscName != "" || terminal == "" {
			t.Fatalf("got vsc=%q ready=%v terminal=%q, want empty/false/non-empty", vscName, ready, terminal)
		}
	})
}

// The export mirror copies the aggregator-published content.status.data onto the import VolumeSnapshot
// VERBATIM. storageClassName is the field that used to be re-derived here from a DataImport path that does
// not exist (spec.storageClassName), which silently produced an empty class on every imported leaf — and the
// leaf, not the SnapshotContent, is what d8 reads on export. The aggregator now fills it from
// DataImport.spec.storageParams.storageClassName, so this controller must add nothing of its own.
func TestMirrorDataToImportVolumeSnapshot_CopiesBindingVerbatim(t *testing.T) {
	ctx := context.Background()
	const (
		ns     = "team-a"
		vsName = "imported-vs"
	)

	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(csiVolumeSnapshotGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{
		Group: csiVolumeSnapshotGVK.Group, Version: csiVolumeSnapshotGVK.Version, Kind: csiVolumeSnapshotGVK.Kind + "List",
	}, &unstructured.UnstructuredList{})

	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
	vs.SetNamespace(ns)
	vs.SetName(vsName)

	statusStub := &unstructured.Unstructured{}
	statusStub.SetGroupVersionKind(csiVolumeSnapshotGVK)

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(statusStub).
		WithObjects(vs).
		Build()
	r := &Controller{Client: cl, APIReader: cl}

	binding := storagev1alpha1.SnapshotDataBinding{
		SourceRef: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: "v1", Kind: kindPersistentVolumeClaim, Name: "bk-pvc", Namespace: ns, UID: types.UID("pvc-uid-123"),
		},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: snapshotpkg.CSISnapshotAPIVersion, Kind: snapshotpkg.KindVolumeSnapshotContent, Name: "snapcontent-abc",
		},
		VolumeMode:       "Filesystem",
		StorageClassName: "sc-import",
		Size:             "10Gi",
	}
	if err := r.mirrorDataToImportVolumeSnapshot(ctx, client.ObjectKey{Namespace: ns, Name: vsName}, binding); err != nil {
		t.Fatalf("mirrorDataToImportVolumeSnapshot: %v", err)
	}

	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(csiVolumeSnapshotGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: vsName}, fresh); err != nil {
		t.Fatalf("get VolumeSnapshot: %v", err)
	}
	if sc, _, _ := unstructured.NestedString(fresh.Object, "status", "data", "storageClassName"); sc != "sc-import" {
		t.Fatalf("status.data.storageClassName = %q, want sc-import", sc)
	}
	if size, _, _ := unstructured.NestedString(fresh.Object, "status", "data", "size"); size != "10Gi" {
		t.Fatalf("status.data.size = %q, want 10Gi", size)
	}
	if name, _, _ := unstructured.NestedString(fresh.Object, "status", "data", "artifactRef", "name"); name != "snapcontent-abc" {
		t.Fatalf("status.data.artifactRef.name = %q, want snapcontent-abc", name)
	}
	if uid, _, _ := unstructured.NestedString(fresh.Object, "status", "data", "sourceRef", "uid"); uid != "pvc-uid-123" {
		t.Fatalf("status.data.sourceRef.uid = %q, want pvc-uid-123", uid)
	}
}

// A DataImport lives only as long as the import needs it: storage-foundation reaps it on its own idle TTL.
// For an already-bound import VolumeSnapshot "no DataImport" is therefore the PERMANENT end state, not a
// pre-creation window, and the two are told apart by what the leaf already has: a bound VSC plus a content
// carrying its published data leg. Treating the end state as pending kept the leaf at ~12 reconciles/min
// forever (each listing DataImports) with the status.data export mirror — the only thing that carries a
// later content-side correction onto the VS that d8 reads — never running again.
//
// The readiness surface of this controller is the one-shot legacy CSI readyToUse written at bind time; it is
// deliberately not asserted below as an output of these paths.

const (
	vsImportNS          = "team-a"
	vsImportName        = "imported-vs"
	vsImportUID         = "imported-vs-uid"
	vsImportParentSnap  = "root-snap"
	vsImportParentUID   = "root-snap-uid"
	vsImportParentCont  = "parent-content"
	vsImportParentCUID  = "parent-content-uid"
	vsImportContentName = "leaf-content"
	vsImportContentUID  = "leaf-content-uid"
	vsImportVSCName     = "snapcontent-abc"
	vsImportScratchSC   = "sc-import"
)

// vsImportScheme registers the extended VolumeSnapshot (unstructured) plus the SVDM DataImport/DataImportList
// the reverse-lookup lists, so the fake client can serve every read Reconcile performs.
func vsImportScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add state-snapshotter scheme: %v", err)
	}
	scheme.AddKnownTypeWithName(csiVolumeSnapshotGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(csiVolumeSnapshotGVK.GroupVersion().WithKind(csiVolumeSnapshotGVK.Kind+"List"), &unstructured.UnstructuredList{})
	diGVK := schema.GroupVersionKind{Group: "storage-foundation.deckhouse.io", Version: "v1alpha1", Kind: "DataImport"}
	scheme.AddKnownTypeWithName(diGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(diGVK.GroupVersion().WithKind("DataImportList"), &unstructured.UnstructuredList{})
	return scheme
}

// vsImportLeaf builds an import-mode extended VolumeSnapshot already bound to its SnapshotContent, with a
// child->parent ownerRef. boundVSCName != "" marks the leaf whose data leg the DataImport already produced.
func vsImportLeaf(t *testing.T, boundVSCName string) *unstructured.Unstructured {
	t.Helper()
	vs := &unstructured.Unstructured{Object: map[string]interface{}{}}
	vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
	vs.SetNamespace(vsImportNS)
	vs.SetName(vsImportName)
	vs.SetUID(types.UID(vsImportUID))
	vs.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "Snapshot",
		Name:       vsImportParentSnap,
		UID:        types.UID(vsImportParentUID),
	}})
	if err := unstructured.SetNestedField(vs.Object, string(storagev1alpha1.SnapshotModeImport), "spec", "mode"); err != nil {
		t.Fatalf("set spec.mode Import: %v", err)
	}
	if err := unstructured.SetNestedField(vs.Object, vsImportContentName, "status", "boundSnapshotContentName"); err != nil {
		t.Fatalf("set boundSnapshotContentName: %v", err)
	}
	if boundVSCName != "" {
		if err := unstructured.SetNestedField(vs.Object, boundVSCName, "status", "boundVolumeSnapshotContentName"); err != nil {
			t.Fatalf("set boundVolumeSnapshotContentName: %v", err)
		}
	}
	return vs
}

// vsImportLeafContent is the leaf's SnapshotContent, already owned by the parent content so that the
// EnsureLifecycleOwnerRef step is a no-op. data == nil models a content the aggregator has not published yet.
func vsImportLeafContent(data *storagev1alpha1.SnapshotDataBinding) *storagev1alpha1.SnapshotContent {
	controller := true
	c := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: vsImportContentName,
			UID:  types.UID(vsImportContentUID),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "SnapshotContent",
				Name:       vsImportParentCont,
				UID:        types.UID(vsImportParentCUID),
				Controller: &controller,
			}},
		},
	}
	c.Status.Data = data
	return c
}

// vsImportPublishedData is the descriptor the aggregator publishes for an imported orphan-PVC leaf.
func vsImportPublishedData() *storagev1alpha1.SnapshotDataBinding {
	return &storagev1alpha1.SnapshotDataBinding{
		SourceRef: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: corePVCAPIVersion, Kind: kindPersistentVolumeClaim,
			Name: "bk-pvc", Namespace: vsImportNS, UID: types.UID("pvc-uid-123"),
		},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: snapshotpkg.CSISnapshotAPIVersion, Kind: snapshotpkg.KindVolumeSnapshotContent, Name: vsImportVSCName,
		},
		StorageClassName: vsImportScratchSC,
		Size:             "10Gi",
	}
}

// vsImportReconcile assembles the parent chain + reconstructed checkpoint every import reconcile walks
// through and runs one Reconcile against it. No DataImport object is ever created: these tests are about
// the leaf that outlived its own.
func vsImportReconcile(t *testing.T, vs *unstructured.Unstructured, content *storagev1alpha1.SnapshotContent) (ctrl.Result, client.Client, error) {
	t.Helper()
	scheme := vsImportScheme(t)
	parent := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: vsImportNS, Name: vsImportParentSnap, UID: types.UID(vsImportParentUID)},
	}
	parent.Status.BoundSnapshotContentName = vsImportParentCont
	parentContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: vsImportParentCont, UID: types.UID(vsImportParentCUID)},
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: usecase.ReconstructedManifestCheckpointName(types.UID(vsImportUID), "")},
	}

	statusStub := &unstructured.Unstructured{}
	statusStub.SetGroupVersionKind(csiVolumeSnapshotGVK)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(statusStub, &storagev1alpha1.SnapshotContent{}, &storagev1alpha1.Snapshot{}).
		WithObjects(vs, parent, parentContent, content, mcp).
		Build()
	r := &Controller{Client: cl, APIReader: cl}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: vsImportNS, Name: vsImportName}})
	return res, cl, err
}

// Steady state of a finished import: the DataImport is gone, the VSC is bound and the content carries its
// data leg. Reconcile must stop polling and still refresh the status.data export mirror.
func TestReconcile_SteadyStateWithoutDataImportMirrorsAndStopsPolling(t *testing.T) {
	res, cl, err := vsImportReconcile(t, vsImportLeaf(t, vsImportVSCName), vsImportLeafContent(vsImportPublishedData()))
	if err != nil {
		t.Fatalf("Reconcile must not fail in the DataImport-less steady state: %v", err)
	}
	if res.RequeueAfter != 0 || res.Requeue {
		t.Fatalf("a finished import must stop polling, got %+v", res)
	}

	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(csiVolumeSnapshotGVK)
	if gErr := cl.Get(context.Background(), client.ObjectKey{Namespace: vsImportNS, Name: vsImportName}, fresh); gErr != nil {
		t.Fatalf("get VolumeSnapshot: %v", gErr)
	}
	if sc, _, _ := unstructured.NestedString(fresh.Object, "status", "data", "storageClassName"); sc != vsImportScratchSC {
		t.Fatalf("status.data.storageClassName = %q, want %q (the export mirror did not run)", sc, vsImportScratchSC)
	}
	if art, _, _ := unstructured.NestedString(fresh.Object, "status", "data", "artifactRef", "name"); art != vsImportVSCName {
		t.Fatalf("status.data.artifactRef.name = %q, want %q", art, vsImportVSCName)
	}
}

// Genuinely pending import: no DataImport and the VSC is not bound yet, so the data leg is still ahead of
// this leaf. Nothing wakes the controller (no DataImport watch), so the poll must survive. Regression guard.
func TestReconcile_PendingWithoutDataImportKeepsPolling(t *testing.T) {
	res, cl, err := vsImportReconcile(t, vsImportLeaf(t, ""), vsImportLeafContent(nil))
	if err != nil {
		t.Fatalf("Reconcile on a pending import must not fail: %v", err)
	}
	if res.RequeueAfter != importPollInterval {
		t.Fatalf("a pending import must keep polling, got RequeueAfter=%v want %v", res.RequeueAfter, importPollInterval)
	}

	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(csiVolumeSnapshotGVK)
	if gErr := cl.Get(context.Background(), client.ObjectKey{Namespace: vsImportNS, Name: vsImportName}, fresh); gErr != nil {
		t.Fatalf("get VolumeSnapshot: %v", gErr)
	}
	if _, found, _ := unstructured.NestedMap(fresh.Object, "status", "data"); found {
		t.Fatalf("status.data must not be written while the content has published none")
	}
}

// A bound VSC alone does NOT make the steady state: a leaf whose VSC binding raced ahead of the aggregator's
// publish is still converging, and the aggregator's write produces no event for this controller, so the poll
// must survive until the content actually carries its data leg.
func TestReconcile_BoundVSCWithoutPublishedContentDataKeepsPolling(t *testing.T) {
	res, _, err := vsImportReconcile(t, vsImportLeaf(t, vsImportVSCName), vsImportLeafContent(nil))
	if err != nil {
		t.Fatalf("Reconcile must not fail while the content is unpublished: %v", err)
	}
	if res.RequeueAfter != importPollInterval {
		t.Fatalf("an unpublished content must keep polling, got RequeueAfter=%v want %v", res.RequeueAfter, importPollInterval)
	}
}
