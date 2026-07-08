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
	importLeafGroup    = "demo.state-snapshotter.deckhouse.io"
	importLeafAPIVer   = importLeafGroup + "/v1alpha1"
	importLeafObjName  = "disk-snap"
	importDataImportNS = projTestNS
)

var dataImportListGVK = schema.GroupVersionKind{Group: "storage-foundation.deckhouse.io", Version: "v1alpha1", Kind: "DataImportList"}

// importOwnerLeaf builds an import-mode (spec.mode: Import) generic domain leaf owner of the given kind.
func importOwnerLeaf(kind string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": importLeafAPIVer,
		"kind":       kind,
		"metadata":   map[string]interface{}{"namespace": projTestNS, "name": importLeafObjName, "uid": "leaf-uid-1"},
		"spec":       map[string]interface{}{"mode": string(storagev1alpha1.SnapshotModeImport)},
	}}
}

// importDataImportForLeaf builds a DataImport whose spec.snapshotRef targets the leaf and whose
// status.data.artifact points at the produced VolumeSnapshotContent.
func importDataImportForLeaf(vscName string) *unstructured.Unstructured {
	di := &unstructured.Unstructured{}
	di.SetGroupVersionKind(schema.GroupVersionKind{Group: "storage-foundation.deckhouse.io", Version: "v1alpha1", Kind: "DataImport"})
	di.SetNamespace(importDataImportNS)
	di.SetName("di-1")
	_ = unstructured.SetNestedMap(di.Object, map[string]interface{}{
		"apiVersion": importLeafAPIVer, "kind": importLeafKind, "name": importLeafObjName,
	}, "spec", "snapshotRef")
	_ = unstructured.SetNestedMap(di.Object, map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshotContent", "name": vscName,
	}, "status", "data", "artifact")
	_ = unstructured.SetNestedField(di.Object, string(corev1.PersistentVolumeFilesystem), "status", "volumeMode")
	return di
}

func importLeafObject() *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "demo.state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "DemoVirtualDiskSnapshot",
		"metadata": map[string]interface{}{
			"name":      "disk-snap",
			"namespace": "project-a",
			"uid":       "leaf-uid-1",
		},
	}}
	return o
}

func dataImportWithArtifact(apiVersion, kind, name string) *unstructured.Unstructured {
	di := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "DataImport",
		"metadata": map[string]interface{}{
			"name":      "di-1",
			"namespace": "project-a",
		},
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
		_ = unstructured.SetNestedMap(di.Object, ref, "status", "data", "artifact")
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
	// DataImport fills the durable artifact uid best-effort (from the VCR artifact uid); it must flow
	// through into the published dataRef.artifact.uid.
	_ = unstructured.SetNestedField(di.Object, "8d7c6b5a-4e3f-4a2b-9c1d-0f1e2d3c4b5a", "status", "data", "artifact", "uid")
	leaf := importLeafObject()

	binding, ready, reason, _ := BuildImportDataBinding(di, leaf)
	if reason != "" {
		t.Fatalf("unexpected terminal reason: %q", reason)
	}
	if !ready || binding == nil {
		t.Fatalf("expected ready binding, got ready=%v binding=%v", ready, binding)
	}
	if binding.Artifact.Kind != snapshot.KindVolumeSnapshotContent || binding.Artifact.Name != "snapcontent-abc" {
		t.Fatalf("unexpected artifact: %#v", binding.Artifact)
	}
	if binding.Artifact.APIVersion != "snapshot.storage.k8s.io/v1" {
		t.Fatalf("unexpected artifact apiVersion: %q", binding.Artifact.APIVersion)
	}
	if binding.Artifact.UID != "8d7c6b5a-4e3f-4a2b-9c1d-0f1e2d3c4b5a" {
		t.Fatalf("expected artifact uid propagated from DataImport.status.data.artifact.uid, got %q", binding.Artifact.UID)
	}
	if string(binding.Source.UID) != "leaf-uid-1" {
		t.Fatalf("expected Source.UID from leaf UID, got %q", binding.Source.UID)
	}
	if binding.Source.Kind != "DemoVirtualDiskSnapshot" || binding.Source.Name != "disk-snap" || binding.Source.Namespace != "project-a" {
		t.Fatalf("unexpected source: %#v", binding.Source)
	}
	if binding.VolumeMode != "Block" {
		t.Fatalf("expected volumeMode propagated from DataImport.status.volumeMode, got %q", binding.VolumeMode)
	}
}

// Before the DataImport produces its artifact (no status.data.artifact), the binding is pending
// (not terminal) so the aggregator keeps requeuing rather than failing the import.
func TestBuildImportDataBinding_PendingWhenArtifactAbsent(t *testing.T) {
	di := dataImportWithArtifact("", "", "")
	binding, ready, reason, _ := BuildImportDataBinding(di, importLeafObject())
	if ready || binding != nil || reason != "" {
		t.Fatalf("expected pending (no binding, no terminal), got ready=%v binding=%v reason=%q", ready, binding, reason)
	}
}

// A partially-written status.data.artifact (missing name) is still treated as not-yet-produced (pending).
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

	requeue, err := r.reconcileDataLegProjection(ctx, projContentObj(nil), importOwnerLeaf(importLeafKind), projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection: %v", err)
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

	requeue, err := r.reconcileDataLegProjection(ctx, projContentObj(nil), importOwnerLeaf(importLeafKind), projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection: %v", err)
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
	if d.Artifact.Name != projTestVSCName || d.Artifact.Kind != snapshot.KindVolumeSnapshotContent {
		t.Fatalf("unexpected published artifact: %#v", d.Artifact)
	}
	if d.Source.Kind != importLeafKind || d.Source.Name != importLeafObjName {
		t.Fatalf("import data source must be the leaf identity, got %#v", d.Source)
	}
	if d.VolumeMode != string(corev1.PersistentVolumeFilesystem) {
		t.Fatalf("volumeMode must be projected from DataImport.status.volumeMode, got %q", d.VolumeMode)
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
