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

package snapshotcontent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

const (
	importLeafKind     = "DemoVirtualDiskSnapshot"
	importLeafGroup    = "sds-unified-snapshots-poc.deckhouse.io"
	importLeafAPIVer   = importLeafGroup + "/v1alpha1"
	importLeafObjName  = "disk-snap"
	importDataImportNS = projTestNS
	// importScratchStorageClass is the class a PopulateData DataImport stages the imported bytes into
	// (spec.storageParams.storageClassName) — the authoritative import StorageClass mapping. It differs from
	// the capture fixture's PVC class ("sc-a") so a test cannot pass by picking up the wrong source.
	importScratchStorageClass = "sc-import"
	// importObservedFsType is the filesystem storage-foundation observed on the scratch volume
	// (DataImport.status.data.fsType). It differs from the capture fixture's PV filesystem so a test cannot
	// pass by picking up the wrong source.
	importObservedFsType = "ext4"
)

var dataImportListGVK = schema.GroupVersionKind{Group: "storage-foundation.deckhouse.io", Version: "v1alpha1", Kind: "DataImportList"}

// importOwnerLeaf builds the import-mode (spec.mode: Import) generic domain leaf owner. Whether its data
// leg runs is decided by the GVKRegistry (requiresDataArtifact), not by the kind, so one kind covers both
// the data-bearing and the structural cases.
func importOwnerLeaf() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": importLeafAPIVer,
		"kind":       importLeafKind,
		"metadata":   map[string]interface{}{"namespace": projTestNS, "name": importLeafObjName, "uid": "leaf-uid-1"},
		"spec":       map[string]interface{}{"mode": string(storagev1alpha1.SnapshotModeImport)},
	}}
}

// importDataImportForLeaf builds a PopulateData DataImport whose spec.snapshotRef targets the leaf, whose
// spec.storageParams carry the authoritative scratch StorageClass, and whose status.data.artifactRef points
// at the produced VolumeSnapshotContent.
func importDataImportForLeaf(vscName string) *unstructured.Unstructured {
	di := &unstructured.Unstructured{}
	di.SetGroupVersionKind(schema.GroupVersionKind{Group: "storage-foundation.deckhouse.io", Version: "v1alpha1", Kind: "DataImport"})
	di.SetNamespace(importDataImportNS)
	di.SetName("di-1")
	_ = unstructured.SetNestedField(di.Object, snapshot.DataImportModePopulateData, "spec", "mode")
	_ = unstructured.SetNestedMap(di.Object, map[string]interface{}{
		"apiVersion": importLeafAPIVer, "kind": importLeafKind, "name": importLeafObjName,
	}, "spec", "snapshotRef")
	_ = unstructured.SetNestedMap(di.Object, map[string]interface{}{
		"storageClassName": importScratchStorageClass, "size": "10Gi", "volumeMode": string(corev1.PersistentVolumeFilesystem),
	}, "spec", "storageParams")
	_ = unstructured.SetNestedMap(di.Object, map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshotContent", "name": vscName,
	}, "status", "data", "artifactRef")
	_ = unstructured.SetNestedField(di.Object, string(corev1.PersistentVolumeFilesystem), "status", "volumeMode")
	// The filesystem the imported bytes were actually written onto, observed by storage-foundation on the
	// scratch PersistentVolume before it was destroyed. Nothing else records it.
	_ = unstructured.SetNestedField(di.Object, importObservedFsType, "status", "data", "fsType")
	return di
}

// importContentStorageClassName returns the SnapshotContent's published status.data.storageClassName.
func importContentStorageClassName(t *testing.T, cl client.Client) string {
	t.Helper()
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: projTestContent}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.Data == nil {
		t.Fatalf("expected status.data to be published")
	}
	return got.Status.Data.StorageClassName
}

// importContentResourceVersion returns the SnapshotContent's resourceVersion, so a test can prove a pass
// wrote nothing at all (a latched pass must not re-patch status.data).
func importContentResourceVersion(t *testing.T, cl client.Client) string {
	t.Helper()
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: projTestContent}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	return got.GetResourceVersion()
}

// newImportProjectionFixture wires an aggregator over a data-bearing import leaf: the DataImport list GVK is
// registered (the reverse-lookup lists it cross-group), the produced VSC carries restoreSize, and the leaf
// kind is marked requiresDataArtifact so the data leg runs.
func newImportProjectionFixture(t *testing.T, content *storagev1alpha1.SnapshotContent, extra ...client.Object) (*SnapshotContentController, client.Client) {
	t.Helper()
	scheme := projScheme(t)
	scheme.AddKnownTypeWithName(dataImportListGVK, &unstructured.UnstructuredList{})

	objs := append([]client.Object{content, projVSCWithRestoreSize(), importDataImportForLeaf(projTestVSCName)}, extra...)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(objs...).
		Build()
	reg := snapshot.NewGVKRegistry()
	reg.MarkRequiresDataArtifact(importLeafKind, true)
	return &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: reg}, cl
}

func importLeafObject() *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "sds-unified-snapshots-poc.deckhouse.io/v1alpha1",
		"kind":       "DemoVirtualDiskSnapshot",
		"metadata": map[string]interface{}{
			"name":      "disk-snap",
			"namespace": "project-a",
			"uid":       "leaf-uid-1",
		},
	}}
	return o
}

// dataImportWithArtifact builds the PopulateData DataImport that materializes a snapshot leaf's data leg.
// spec.mode is part of the fixture, not decoration: the readers of its status volume metadata are gated on it,
// because a CreatePVC import publishes the same status fields for a PVC it creates AND KEEPS.
func dataImportWithArtifact(apiVersion, kind, name string) *unstructured.Unstructured {
	di := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "DataImport",
		"metadata": map[string]interface{}{
			"name":      "di-1",
			"namespace": "project-a",
		},
		"spec": map[string]interface{}{"mode": snapshot.DataImportModePopulateData},
	}}
	if name != "" || kind != "" || apiVersion != "" {
		ref := map[string]interface{}{}
		if apiVersion != "" {
			ref["apiVersion"] = apiVersion
		}
		if kind != "" {
			ref["kind"] = kind
		}
		if name != "" {
			ref["name"] = name
		}
		_ = unstructured.SetNestedMap(di.Object, ref, "status", "data", "artifactRef")
	}
	return di
}

// A produced VolumeSnapshotContent artifact yields a ready binding carrying the leaf identity as target
// and the VSC as the data artifact (size etc. are enriched downstream from VSC.status.restoreSize).
func TestBuildImportDataBinding_VSCReady(t *testing.T) {
	di := dataImportWithArtifact("snapshot.storage.k8s.io/v1", "VolumeSnapshotContent", "snapcontent-abc")
	// DataImport republishes the original captured volume mode into status.volumeMode; the binding must
	// carry it because the leaf-targeted dataRef cannot be enriched from a live PVC and downstream restore
	// fails closed on an empty volumeMode.
	_ = unstructured.SetNestedField(di.Object, "Block", "status", "volumeMode")
	// A Block import carries no filesystem, so this fixture leaves status.data.fsType unset — the binding must
	// then carry no fsType either, rather than a default.
	// DataImport fills the durable artifact uid best-effort (from the VCR artifact uid); it must flow
	// through into the published dataRef.artifactRef.uid.
	_ = unstructured.SetNestedField(di.Object, "8d7c6b5a-4e3f-4a2b-9c1d-0f1e2d3c4b5a", "status", "data", "artifactRef", "uid")
	leaf := importLeafObject()

	binding, ready, reason, _ := BuildImportDataBinding(di, leaf)
	if reason != "" {
		t.Fatalf("unexpected terminal reason: %q", reason)
	}
	if !ready || binding == nil {
		t.Fatalf("expected ready binding, got ready=%v binding=%v", ready, binding)
	}
	if binding.ArtifactRef.Kind != snapshot.KindVolumeSnapshotContent || binding.ArtifactRef.Name != "snapcontent-abc" {
		t.Fatalf("unexpected artifact: %#v", binding.ArtifactRef)
	}
	if binding.ArtifactRef.APIVersion != "snapshot.storage.k8s.io/v1" {
		t.Fatalf("unexpected artifact apiVersion: %q", binding.ArtifactRef.APIVersion)
	}
	if binding.ArtifactRef.UID != "8d7c6b5a-4e3f-4a2b-9c1d-0f1e2d3c4b5a" {
		t.Fatalf("expected artifact uid propagated from DataImport.status.data.artifactRef.uid, got %q", binding.ArtifactRef.UID)
	}
	if string(binding.SourceRef.UID) != "leaf-uid-1" {
		t.Fatalf("expected Source.UID from leaf UID, got %q", binding.SourceRef.UID)
	}
	if binding.SourceRef.Kind != "DemoVirtualDiskSnapshot" || binding.SourceRef.Name != "disk-snap" || binding.SourceRef.Namespace != "project-a" {
		t.Fatalf("unexpected source: %#v", binding.SourceRef)
	}
	if binding.VolumeMode != "Block" {
		t.Fatalf("expected volumeMode propagated from DataImport.status.volumeMode, got %q", binding.VolumeMode)
	}
	if binding.FsType != "" {
		t.Fatalf("a Block import has no filesystem; expected no fsType, got %q", binding.FsType)
	}
}

// The filesystem the imported bytes were written onto (DataImport.status.data.fsType) must reach the binding:
// it is observed on the scratch volume before it is destroyed and exists nowhere else afterwards, so a binding
// that drops it leaves the restored PV with no filesystem type at all.
func TestBuildImportDataBinding_CarriesObservedFsType(t *testing.T) {
	di := dataImportWithArtifact("snapshot.storage.k8s.io/v1", "VolumeSnapshotContent", "snapcontent-abc")
	_ = unstructured.SetNestedField(di.Object, string(corev1.PersistentVolumeFilesystem), "status", "volumeMode")
	_ = unstructured.SetNestedField(di.Object, importObservedFsType, "status", "data", "fsType")

	binding, ready, reason, _ := BuildImportDataBinding(di, importLeafObject())
	if reason != "" || !ready || binding == nil {
		t.Fatalf("expected a ready binding, got ready=%v binding=%v reason=%q", ready, binding, reason)
	}
	if binding.FsType != importObservedFsType {
		t.Fatalf("expected fsType propagated from DataImport.status.data.fsType, got %q", binding.FsType)
	}
	if binding.VolumeMode != string(corev1.PersistentVolumeFilesystem) {
		t.Fatalf("volumeMode regressed while adding fsType, got %q", binding.VolumeMode)
	}
}

// Before the DataImport produces its artifact (no status.data.artifactRef), the binding is pending
// (not terminal) so the aggregator keeps requeuing rather than failing the import.
func TestBuildImportDataBinding_PendingWhenArtifactAbsent(t *testing.T) {
	di := dataImportWithArtifact("", "", "")
	binding, ready, reason, _ := BuildImportDataBinding(di, importLeafObject())
	if ready || binding != nil || reason != "" {
		t.Fatalf("expected pending (no binding, no terminal), got ready=%v binding=%v reason=%q", ready, binding, reason)
	}
}

// A partially-written status.data.artifactRef (missing name) is still treated as not-yet-produced (pending).
func TestBuildImportDataBinding_PendingWhenArtifactPartial(t *testing.T) {
	di := dataImportWithArtifact("snapshot.storage.k8s.io/v1", "VolumeSnapshotContent", "")
	binding, ready, reason, _ := BuildImportDataBinding(di, importLeafObject())
	if ready || binding != nil || reason != "" {
		t.Fatalf("expected pending for partial artifactRef, got ready=%v binding=%v reason=%q", ready, binding, reason)
	}
}

// A non-VSC artifact (e.g. PersistentVolume / Detach mode) is a terminal fault for the current import
// dataRef path (VSC-only); it must fail loud, not silently publish an unreadable dataRef.
func TestBuildImportDataBinding_TerminalForNonVSC(t *testing.T) {
	di := dataImportWithArtifact("v1", "PersistentVolume", "pv-xyz")
	binding, ready, reason, msg := BuildImportDataBinding(di, importLeafObject())
	if ready || binding != nil {
		t.Fatalf("expected no binding for non-VSC artifact, got ready=%v binding=%v", ready, binding)
	}
	if reason != snapshot.ReasonDataArtifactInvalid {
		t.Fatalf("expected terminal reason %q, got %q (msg=%q)", snapshot.ReasonDataArtifactInvalid, reason, msg)
	}
}

// A structural import node (kind not marked requiresDataArtifact — e.g. a VM snapshot or the root) has only
// manifests + children, so the import data-leg projection short-circuits: no publish, no requeue. Guards
// against polling forever for a DataImport that will never exist for a manifest-only node.
func TestReconcileDataLegProjection_GenericImportStructuralNodeSkips(t *testing.T) {
	ctx := context.Background()
	scheme := projScheme(t)
	content := projContentTyped()
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(content).
		Build()
	// GVKRegistry with the leaf kind NOT marked as data-bearing (default reads false).
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	requeue, termReason, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), importOwnerLeaf(), projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection: %v", err)
	}
	if termReason != "" {
		t.Fatalf("a manifest-only import node must not be terminal, got %q", termReason)
	}
	if requeue {
		t.Fatalf("a manifest-only import node must not requeue for a data leg")
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: projTestContent}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.Data != nil {
		t.Fatalf("a structural import node must not publish status.data, got %#v", *got.Status.Data)
	}
}

// The aggregator is the single writer of SnapshotContent.status.data for a GENERIC import leaf: it
// reverse-looks-up the DataImport (spec.snapshotRef -> leaf), reads its produced VolumeSnapshotContent,
// performs the same enrich + Retain/ownerRef handoff + publish as the capture path — but with the LEAF
// identity as the binding source (an imported leaf has no live source PVC) and the DataImport-republished
// volumeMode.
func TestReconcileDataLegProjection_GenericImportPublishesFromDataImport(t *testing.T) {
	ctx := context.Background()
	scheme := projScheme(t)
	// The DataImport reverse-lookup lists an unstructured DataImportList cross-group; register the list GVK.
	scheme.AddKnownTypeWithName(dataImportListGVK, &unstructured.UnstructuredList{})
	content := projContentTyped()

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(content, projVSCUnowned(), importDataImportForLeaf(projTestVSCName)).
		Build()
	// Mark the leaf kind as data-bearing so the import branch runs the data leg.
	reg := snapshot.NewGVKRegistry()
	reg.MarkRequiresDataArtifact(importLeafKind, true)
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: reg}

	requeue, termReason, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), importOwnerLeaf(), projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection: %v", err)
	}
	if termReason != "" {
		t.Fatalf("a successful import publish must not be terminal, got %q", termReason)
	}
	if !requeue {
		t.Fatalf("a fresh import publish must requeue so the next pass re-reads the content with data")
	}

	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: projTestContent}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.Data == nil {
		t.Fatalf("expected status.data published by the aggregator from the DataImport artifact, got none")
	}
	d := *got.Status.Data
	if d.ArtifactRef.Name != projTestVSCName || d.ArtifactRef.Kind != snapshot.KindVolumeSnapshotContent {
		t.Fatalf("unexpected published artifact: %#v", d.ArtifactRef)
	}
	if d.SourceRef.Kind != importLeafKind || d.SourceRef.Name != importLeafObjName {
		t.Fatalf("import data source must be the leaf identity, got %#v", d.SourceRef)
	}
	if d.VolumeMode != string(corev1.PersistentVolumeFilesystem) {
		t.Fatalf("volumeMode must be projected from DataImport.status.volumeMode, got %q", d.VolumeMode)
	}
	if d.StorageClassName != importScratchStorageClass {
		t.Fatalf("storageClassName must be projected from DataImport.spec.storageParams.storageClassName, got %q", d.StorageClassName)
	}
	if d.FsType != importObservedFsType {
		t.Fatalf("fsType must be projected from DataImport.status.data.fsType, got %q", d.FsType)
	}

	// The produced VSC is handed off to the content (forced Retain + content ownerRef) exactly like capture.
	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(projVSCGVK)
	if err := cl.Get(ctx, client.ObjectKey{Name: projTestVSCName}, vsc); err != nil {
		t.Fatalf("get VSC: %v", err)
	}
	if policy, _, _ := unstructured.NestedString(vsc.Object, "spec", "deletionPolicy"); policy != "Retain" {
		t.Fatalf("VSC deletionPolicy not forced to Retain, got %q", policy)
	}
	owned := false
	for _, o := range vsc.GetOwnerReferences() {
		if o.Kind == "SnapshotContent" && o.Name == projTestContent && o.UID == types.UID(projTestConUID) {
			owned = true
		}
	}
	if !owned {
		t.Fatalf("VSC not re-owned by content: %#v", vsc.GetOwnerReferences())
	}
}

// The published import StorageClass latches: once status.data carries the class from the DataImport, the
// next pass short-circuits without requeueing and without touching the object. Idempotence is asserted on
// the resourceVersion, so a re-publish that happens to write the same bytes would still be caught.
func TestReconcileDataLegProjection_GenericImportStorageClassLatches(t *testing.T) {
	ctx := context.Background()
	r, cl := newImportProjectionFixture(t, projContentTyped())
	owner := importOwnerLeaf()

	requeue, termReason, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection (pass 1): %v", err)
	}
	if termReason != "" || !requeue {
		t.Fatalf("a fresh import publish must requeue and not be terminal, got requeue=%v termReason=%q", requeue, termReason)
	}
	if sc := importContentStorageClassName(t, cl); sc != importScratchStorageClass {
		t.Fatalf("published storageClassName = %q, want %q", sc, importScratchStorageClass)
	}
	rv := importContentResourceVersion(t, cl)

	requeue, termReason, _, err = r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection (pass 2): %v", err)
	}
	if termReason != "" || requeue {
		t.Fatalf("the second pass must latch (no requeue, not terminal), got requeue=%v termReason=%q", requeue, termReason)
	}
	if got := importContentResourceVersion(t, cl); got != rv {
		t.Fatalf("a latched pass must not write the content: resourceVersion %q -> %q", rv, got)
	}
	if sc := importContentStorageClassName(t, cl); sc != importScratchStorageClass {
		t.Fatalf("latched storageClassName = %q, want %q", sc, importScratchStorageClass)
	}
}

// Self-heal (mandatory part of the fix): a content published BEFORE the aggregator projected the import
// StorageClass already matches the artifactRef and the volumeMode, so a latch keyed on those two alone would
// short-circuit it forever and it would keep an empty storageClassName for the rest of its life — the class
// is unrecoverable once the DataImport is reaped by its idle TTL. The latch must therefore also compare the
// class and let the stale content catch up.
func TestReconcileDataLegProjection_GenericImportBackfillsStorageClassOnStaleContent(t *testing.T) {
	ctx := context.Background()
	content := projContentTyped()
	// Exactly what the pre-fix code published: source + artifact + volumeMode + size, no storageClassName.
	content.Status.Data = &storagev1alpha1.SnapshotDataBinding{
		SourceRef: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: importLeafAPIVer, Kind: importLeafKind,
			Namespace: projTestNS, Name: importLeafObjName, UID: types.UID("leaf-uid-1"),
		},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: "snapshot.storage.k8s.io/v1", Kind: snapshot.KindVolumeSnapshotContent, Name: projTestVSCName,
		},
		VolumeMode: string(corev1.PersistentVolumeFilesystem),
		Size:       "500Mi",
	}
	r, cl := newImportProjectionFixture(t, content)
	owner := importOwnerLeaf()

	requeue, termReason, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection: %v", err)
	}
	if termReason != "" {
		t.Fatalf("a self-heal publish must not be terminal, got %q", termReason)
	}
	if !requeue {
		t.Fatalf("a content published without storageClassName must NOT latch; it has to be re-published")
	}
	if sc := importContentStorageClassName(t, cl); sc != importScratchStorageClass {
		t.Fatalf("stale content did not self-heal: storageClassName = %q, want %q", sc, importScratchStorageClass)
	}
}

// Self-heal on the generic import branch: a content published before the filesystem was projected already
// matches artifactRef, volumeMode and class, so without an fsType term in the latch it would keep the gap for
// the rest of its life — and the value is unrecoverable once the scratch volume and the DataImport are gone.
func TestReconcileDataLegProjection_GenericImportBackfillsFsTypeOnStaleContent(t *testing.T) {
	ctx := context.Background()
	content := projContentTyped()
	content.Status.Data = &storagev1alpha1.SnapshotDataBinding{
		SourceRef: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: importLeafAPIVer, Kind: importLeafKind,
			Namespace: projTestNS, Name: importLeafObjName, UID: types.UID("leaf-uid-1"),
		},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: "snapshot.storage.k8s.io/v1", Kind: snapshot.KindVolumeSnapshotContent, Name: projTestVSCName,
		},
		VolumeMode:       string(corev1.PersistentVolumeFilesystem),
		StorageClassName: importScratchStorageClass,
		Size:             "500Mi",
	}
	r, cl := newImportProjectionFixture(t, content)

	requeue, termReason, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), importOwnerLeaf(), projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection: %v", err)
	}
	if termReason != "" {
		t.Fatalf("a self-heal publish must not be terminal, got %q", termReason)
	}
	if !requeue {
		t.Fatalf("a content published without fsType must NOT latch; it has to be re-published")
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: projTestContent}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.Data == nil || got.Status.Data.FsType != importObservedFsType {
		t.Fatalf("stale content did not self-heal its fsType: %#v", got.Status.Data)
	}
}

// A field the DataImport does not attest must not put the leg into a churn loop: with an unconditional
// comparison a published value the DataImport cannot confirm would mismatch on every pass, re-publishing the
// leg forever while the field itself never changes. The leg must latch on what is published instead.
func TestReconcileDataLegProjection_GenericImportLatchesWhenDataImportAttestsNothingNew(t *testing.T) {
	ctx := context.Background()
	content := projContentTyped()
	content.Status.Data = &storagev1alpha1.SnapshotDataBinding{
		SourceRef: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: importLeafAPIVer, Kind: importLeafKind,
			Namespace: projTestNS, Name: importLeafObjName, UID: types.UID("leaf-uid-1"),
		},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: "snapshot.storage.k8s.io/v1", Kind: snapshot.KindVolumeSnapshotContent, Name: projTestVSCName,
		},
		VolumeMode:       string(corev1.PersistentVolumeFilesystem),
		FsType:           importObservedFsType,
		StorageClassName: importScratchStorageClass,
		Size:             "500Mi",
	}
	// A DataImport whose status volume metadata is empty: storage-foundation has not published it (yet), or the
	// import predates the field.
	silent := importDataImportForLeaf(projTestVSCName)
	unstructured.RemoveNestedField(silent.Object, "status", "volumeMode")
	unstructured.RemoveNestedField(silent.Object, "status", "data", "fsType")

	scheme := projScheme(t)
	scheme.AddKnownTypeWithName(dataImportListGVK, &unstructured.UnstructuredList{})
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(content, projVSCWithRestoreSize(), silent).
		Build()
	reg := snapshot.NewGVKRegistry()
	reg.MarkRequiresDataArtifact(importLeafKind, true)
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: reg}
	rv := importContentResourceVersion(t, cl)

	requeue, termReason, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), importOwnerLeaf(), projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection: %v", err)
	}
	if termReason != "" || requeue {
		t.Fatalf("a silent DataImport must latch on what is already published, got requeue=%v termReason=%q", requeue, termReason)
	}
	if got := importContentResourceVersion(t, cl); got != rv {
		t.Fatalf("nothing must be written when the DataImport attests nothing new: resourceVersion %q -> %q", rv, got)
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: projTestContent}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.Data == nil || got.Status.Data.FsType != importObservedFsType ||
		got.Status.Data.VolumeMode != string(corev1.PersistentVolumeFilesystem) ||
		got.Status.Data.StorageClassName != importScratchStorageClass {
		t.Fatalf("a latched pass must leave the published metadata exactly as it was: %#v", got.Status.Data)
	}
}
