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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// contentWithDataRefVSC builds a common SnapshotContent that published its volume data edge:
// status.dataRef.artifact -> VolumeSnapshotContent{name}. This is the durable edge the dual-path VSC
// wake-up reverse-resolves on (indexKeyDataRefArtifactName).
func contentWithDataRefVSC(name, vscName string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "SnapshotContent",
		"metadata":   map[string]interface{}{"name": name},
		"status": map[string]interface{}{
			"dataRef": map[string]interface{}{
				"artifact": map[string]interface{}{
					"apiVersion": volumeSnapshotContentAPIVersion,
					"kind":       kindVolumeSnapshotContent,
					"name":       vscName,
				},
			},
		},
	}}
	obj.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	return obj
}

func vscUnstructured(name string, refs []metav1.OwnerReference) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: kindVolumeSnapshotContent})
	u.SetName(name)
	if refs != nil {
		u.SetOwnerReferences(refs)
	}
	return u
}

// TestExtractDataRefArtifactNameIndex: projects status.dataRef.artifact.name only for a VolumeSnapshotContent
// artifact; a non-VSC kind, an empty name, or a non-unstructured object yields no index entry.
func TestExtractDataRefArtifactNameIndex(t *testing.T) {
	if got := extractDataRefArtifactNameIndex(contentWithDataRefVSC("c", "vsc-1")); len(got) != 1 || got[0] != "vsc-1" {
		t.Fatalf("VSC dataRef: got %v, want [vsc-1]", got)
	}
	// Non-VSC artifact kind -> no entry.
	other := contentWithDataRefVSC("c", "vsc-1")
	_ = unstructured.SetNestedField(other.Object, "ManifestCheckpoint", "status", "dataRef", "artifact", "kind")
	if got := extractDataRefArtifactNameIndex(other); got != nil {
		t.Fatalf("non-VSC artifact: got %v, want nil", got)
	}
	// Empty name -> no entry.
	if got := extractDataRefArtifactNameIndex(contentWithDataRefVSC("c", "")); got != nil {
		t.Fatalf("empty name: got %v, want nil", got)
	}
	// No dataRef at all -> no entry.
	if got := extractDataRefArtifactNameIndex(commonContentWithStatus("c", "mcp-1")); got != nil {
		t.Fatalf("no dataRef: got %v, want nil", got)
	}
	// Non-unstructured -> no entry.
	if got := extractDataRefArtifactNameIndex(&ssv1alpha1.ManifestCheckpoint{}); got != nil {
		t.Fatalf("non-unstructured: got %v, want nil", got)
	}
}

// TestMapVolumeSnapshotContentToContent_DualPath: ownerRef (path 1) wins when present; otherwise the
// pre-adoption reverse lookup by status.dataRef.artifact.name (path 2) resolves the owning content. An
// unknown VSC resolves to nothing (the 500ms self-requeue backstops).
func TestMapVolumeSnapshotContentToContent_DualPath(t *testing.T) {
	scheme := aggScheme(t)
	contentGVK := contentGVKForTest()
	indexObj := &unstructured.Unstructured{}
	indexObj.SetGroupVersionKind(contentGVK)

	content := contentWithDataRefVSC("owning-content", "vsc-1")
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(indexObj, indexKeyDataRefArtifactName, extractDataRefArtifactNameIndex).
		WithObjects(content).
		Build()
	r := &SnapshotContentController{
		Client:              cl,
		APIReader:           cl,
		GVKRegistry:         snapshot.NewGVKRegistry(),
		SnapshotContentGVKs: []schema.GroupVersionKind{contentGVK},
	}
	ctrlTrue := true

	// path 1: ownerRef wins (distinct name proves we did not fall through to reverse lookup).
	owned := vscUnstructured("vsc-1", []metav1.OwnerReference{{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "SnapshotContent",
		Name:       "owner-via-ref",
		UID:        "u",
		Controller: &ctrlTrue,
	}})
	if reqs := r.mapVolumeSnapshotContentToContent(context.Background(), owned); len(reqs) != 1 || reqs[0].Name != "owner-via-ref" {
		t.Fatalf("path1 (ownerRef): got %v, want one request for owner-via-ref", reqs)
	}

	// path 2: no ownerRef (pre-adoption) -> reverse lookup by published dataRef artifact name.
	if reqs := r.mapVolumeSnapshotContentToContent(context.Background(), vscUnstructured("vsc-1", nil)); len(reqs) != 1 || reqs[0].Name != "owning-content" {
		t.Fatalf("path2 (dataRef reverse lookup): got %v, want one request for owning-content", reqs)
	}

	// path 2 miss: unknown VSC name -> nil (self-requeue backstops).
	if reqs := r.mapVolumeSnapshotContentToContent(context.Background(), vscUnstructured("vsc-unknown", nil)); reqs != nil {
		t.Fatalf("path2 miss: got %v, want nil", reqs)
	}

	// nil object -> nil.
	if reqs := r.mapVolumeSnapshotContentToContent(context.Background(), nil); reqs != nil {
		t.Fatalf("nil obj: got %v, want nil", reqs)
	}
}

// contentWithLegConditions builds a common SnapshotContent at a given generation carrying the supplied
// ManifestsReady / ManifestsArchived conditions, for the manifestLegAlreadyLatched cost-cut guard.
func contentWithLegConditions(name string, gen int64, conds ...metav1.Condition) *unstructured.Unstructured { //nolint:unparam // test fixture keeps uniform signature
	obj := commonContentWithStatus(name, "mcp-1")
	obj.SetGeneration(gen)
	snapshot.SyncConditionsToUnstructured(obj, conds)
	return obj
}

func cond(t string, s metav1.ConditionStatus, gen int64) metav1.Condition {
	return metav1.Condition{Type: t, Status: s, Reason: snapshot.ReasonCompleted, ObservedGeneration: gen}
}

// TestManifestLegAlreadyLatched: the cost-cut guard fires ONLY when both ManifestsReady=True and
// ManifestsArchived=True are current for this generation. A stale observedGeneration or any missing/not-True
// leg forces the full manifest re-validation (returns false).
func TestManifestLegAlreadyLatched(t *testing.T) {
	const gen = int64(3)
	mr := snapshot.ConditionManifestsReady
	ma := snapshot.ConditionManifestsArchived

	if !manifestLegAlreadyLatched(contentWithLegConditions("c", gen,
		cond(mr, metav1.ConditionTrue, gen), cond(ma, metav1.ConditionTrue, gen))) {
		t.Fatal("both True + current generation must latch (skip re-validation)")
	}
	if manifestLegAlreadyLatched(contentWithLegConditions("c", gen,
		cond(mr, metav1.ConditionTrue, gen-1), cond(ma, metav1.ConditionTrue, gen-1))) {
		t.Fatal("stale observedGeneration must NOT latch (force re-validation)")
	}
	if manifestLegAlreadyLatched(contentWithLegConditions("c", gen,
		cond(mr, metav1.ConditionTrue, gen), cond(ma, metav1.ConditionFalse, gen))) {
		t.Fatal("ManifestsArchived not True must NOT latch")
	}
	if manifestLegAlreadyLatched(contentWithLegConditions("c", gen,
		cond(mr, metav1.ConditionFalse, gen), cond(ma, metav1.ConditionTrue, gen))) {
		t.Fatal("ManifestsReady not True must NOT latch")
	}
	if manifestLegAlreadyLatched(contentWithLegConditions("c", gen,
		cond(mr, metav1.ConditionTrue, gen))) {
		t.Fatal("missing ManifestsArchived must NOT latch")
	}
}
