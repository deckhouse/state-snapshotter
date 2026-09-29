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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func publishedDataBinding(uid types.UID, size string) *storagev1alpha1.SnapshotDataBinding {
	return &storagev1alpha1.SnapshotDataBinding{
		SourceRef: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: projTestPVCName,
			Namespace: projTestNS, UID: types.UID(projTestPVCUID),
		},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: volumeSnapshotContentAPIVersion, Kind: kindVolumeSnapshotContent,
			Name: projTestVSCName, UID: uid,
		},
		Size:             size,
		StorageClassName: "sc-a",
		VolumeMode:       string(corev1.PersistentVolumeFilesystem),
	}
}

func nativeCSIOwnerBoundTo(vscName string) *unstructured.Unstructured {
	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(projVSGVK)
	owner.SetNamespace(projTestNS)
	owner.SetName("user-vs")
	_ = unstructured.SetNestedField(owner.Object, vscName, "status", "boundVolumeSnapshotContentName")
	_ = unstructured.SetNestedMap(owner.Object, map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"name":       projTestPVCName,
		"namespace":  projTestNS,
		"uid":        projTestPVCUID,
	}, "status", "sourceRef")
	return owner
}

func TestReconcileDataLegProjection_NativeCSIHoldsIdentityOnNewVSC(t *testing.T) {
	ctx := context.Background()
	scheme := projScheme(t)
	const publishedUID = types.UID("vsc-1-uid")
	content := projContentTyped()
	content.Status.Data = publishedDataBinding(publishedUID, "500Mi")
	newVSC := projVSCUnowned()
	newVSC.SetName("vsc-2")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(projSourcePVC(), content, projVSCUnowned(), newVSC).
		Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	requeue, termReason, _, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), nativeCSIOwnerBoundTo("vsc-2"), projTestNS, true)
	if err != nil {
		t.Fatalf("divergence must not abort reconcile: %v", err)
	}
	if termReason != "" {
		t.Fatalf("holding identity must not be terminal, got %q", termReason)
	}
	if requeue {
		t.Fatalf("a complete published identity must not requeue")
	}
	got := projContentData(t, cl)
	if got.ArtifactRef.Name != projTestVSCName {
		t.Fatalf("published artifact identity must not move, got %q", got.ArtifactRef.Name)
	}
	if got.ArtifactRef.UID != publishedUID {
		t.Fatalf("published artifact uid must stay, got %q", got.ArtifactRef.UID)
	}
}

func TestReconcileDataLegProjection_NativeCSIBackfillsUIDWithoutNewIdentity(t *testing.T) {
	ctx := context.Background()
	scheme := projScheme(t)
	content := projContentTyped()
	content.Status.Data = publishedDataBinding("", "500Mi")
	oldVSC := projVSCUnowned()
	oldVSC.SetUID("vsc-uid-from-live")
	newVSC := projVSCUnowned()
	newVSC.SetName("vsc-2")
	newVSC.SetUID("vsc-2-uid")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(projSourcePVC(), content, oldVSC, newVSC).
		Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	requeue, termReason, _, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), nativeCSIOwnerBoundTo("vsc-2"), projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection: %v", err)
	}
	if termReason != "" {
		t.Fatalf("uid backfill must not be terminal, got %q", termReason)
	}
	if !requeue {
		t.Fatalf("a backfilling publish is a separate status patch, so it must requeue to be observed")
	}
	got := projContentData(t, cl)
	if got.ArtifactRef.Name != projTestVSCName {
		t.Fatalf("identity must stay %q, got %q", projTestVSCName, got.ArtifactRef.Name)
	}
	if got.ArtifactRef.UID != "vsc-uid-from-live" {
		t.Fatalf("missing uid must be backfilled from the published artifact, got %q", got.ArtifactRef.UID)
	}
	if got.Size != "500Mi" {
		t.Fatalf("size must stay, got %q", got.Size)
	}
}

func TestReconcileDataLegProjection_VCRFailedDoesNotMoveFrozenIdentity(t *testing.T) {
	ctx := context.Background()
	scheme := projScheme(t)
	content := projContentTyped()
	content.Status.Data = publishedDataBinding("vsc-uid", "500Mi")

	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(schema.GroupVersionKind{Group: "sds-unified-snapshots-poc.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualDiskSnapshot"})
	owner.SetNamespace(projTestNS)
	owner.SetName("disk-snap")
	_ = unstructured.SetNestedField(owner.Object, projTestVCRName, "status", "captureState", "domainSpecificController", "volumeCaptureRequestName")

	failedVCR := projReadyVCR()
	_ = unstructured.SetNestedSlice(failedVCR.Object, []interface{}{
		map[string]interface{}{
			"type":    "Ready",
			"status":  string(metav1.ConditionFalse),
			"reason":  "SnapshotCreationFailed",
			"message": "csi failed",
		},
	}, "status", "conditions")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(projSourcePVC(), content, failedVCR, projVSCUnowned()).
		Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	requeue, termReason, _, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true)
	if err != nil {
		t.Fatalf("frozen identity + failed leftover VCR must not abort: %v", err)
	}
	if termReason != "" {
		t.Fatalf("a leftover failed VCR must not terminal a content that already has data, got %q", termReason)
	}
	if requeue {
		t.Fatalf("complete frozen identity must not requeue")
	}
	got := projContentData(t, cl)
	if got.ArtifactRef.Name != projTestVSCName {
		t.Fatalf("identity must stay, got %q", got.ArtifactRef.Name)
	}
}

// The published artifact uid is set-once at the API boundary, so anything that would rewrite it does not
// merely lose a field — it wedges the reconcile on a rejection that never clears.
//
// The reachable way to rewrite it is not a divergent identity (that is held) but an EQUAL one. Identity is
// compared by name, deliberately: the incoming binding is built from the owner's bound content name and
// carries no uid at all, so comparing uids would report a divergence on every single pass. The uid arrives
// later, from enrichment reading the live artifact of that name — and a VolumeSnapshotContent deleted and
// recreated under the captured name is a different object with the same name. Enriching from it would hand
// the publish a uid that contradicts the one already published.
//
// So this pins that the published uid survives an enrichment pass over a live artifact that no longer is
// the one it was taken from: the name matches, the uid does not, and the published value wins.
func TestReconcileDataLegProjection_EnrichmentKeepsThePublishedArtifactUID(t *testing.T) {
	ctx := context.Background()
	scheme := projScheme(t)
	content := projContentTyped()
	// Published: the artifact this content is bound to, with the uid it had when it was captured, and no
	// Size — so the binding still needs enrichment and the enricher will read the live artifact.
	content.Status.Data = publishedDataBinding("vsc-uid-as-captured", "")

	// Live: same name, different object, and a restoreSize the enricher wants.
	recreated := projVSCUnowned()
	recreated.SetUID("vsc-uid-after-recreate")
	_ = unstructured.SetNestedField(recreated.Object, int64(524288000), "status", "restoreSize")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(projSourcePVC(), content, recreated).
		Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	if _, _, _, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), nativeCSIOwnerBoundTo(projTestVSCName), projTestNS, true); err != nil {
		t.Fatalf("reconcileDataLegProjection: %v", err)
	}

	got := projContentData(t, cl)
	if got.ArtifactRef.UID != "vsc-uid-as-captured" {
		t.Fatalf("published artifact uid = %q, want it untouched at %q: the uid is set-once at the API "+
			"boundary, so writing the recreated object's uid is a rejection this reconcile never gets past",
			got.ArtifactRef.UID, "vsc-uid-as-captured")
	}
	if got.ArtifactRef.Name != projTestVSCName {
		t.Fatalf("identity must stay %q, got %q", projTestVSCName, got.ArtifactRef.Name)
	}
}

// A PVC deleted and recreated under the captured name is a different volume, and it may be a differently
// shaped one: Block where the capture was Filesystem, another StorageClass. The enricher already refuses to
// read such a namesake — it compares the live PVC's uid against the binding's — but that comparison is only
// worth anything if the binding carries the PUBLISHED uid by the time the enricher runs. The projection
// assembles the binding from the owner's live status, which names the NEW uid, so the carry-forward has to
// land first; running it only in the writer leaves the enricher matching new against new and copying the
// stranger's volumeMode under the frozen identity.
//
// TURNS RED WHEN the carry-forward in publishDataBindings (datarefs_projection.go) is removed or moved back
// below EnrichDataBindingsWithVolumeMetadata: volumeMode becomes Block, read off the recreated claim.
func TestReconcileDataLegProjection_RecreatedSourceDoesNotEnrichFromTheStranger(t *testing.T) {
	ctx := context.Background()
	scheme := projScheme(t)
	const (
		capturedPVCUID = types.UID(projTestPVCUID)
		strangerUID    = types.UID("pvc-a-recreated-uid")
	)

	// Published at capture time: Filesystem, sc-a, and no Size yet — the window in which the projection
	// reassembles the binding on every pass.
	content := projContentTyped()
	content.Status.Data = publishedDataBinding("vsc-1-uid", "")
	content.Status.Data.SourceRef.UID = capturedPVCUID

	// The live claim now: same name and namespace, different object, different shape.
	block := corev1.PersistentVolumeBlock
	strangerSC := "sc-fast"
	stranger := projSourcePVC()
	stranger.UID = strangerUID
	stranger.Spec.VolumeMode = &block
	stranger.Spec.StorageClassName = &strangerSC

	// The owner publishes the uid of whatever claim is live, which is the stranger's.
	owner := nativeCSIOwnerBoundTo(projTestVSCName)
	_ = unstructured.SetNestedField(owner.Object, string(strangerUID), "status", "sourceRef", "uid")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(stranger, content, projVSCUnowned()).
		Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	if _, termReason, _, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true); err != nil {
		t.Fatalf("reconcileDataLegProjection: %v (terminal=%q)", err, termReason)
	}

	got := projContentData(t, cl)
	if got.SourceRef.UID != capturedPVCUID {
		t.Fatalf("published source uid must stay the captured one, got %q", got.SourceRef.UID)
	}
	if got.VolumeMode != string(corev1.PersistentVolumeFilesystem) {
		t.Fatalf("volumeMode must stay what the captured volume had; %q was read off the recreated namesake", got.VolumeMode)
	}
	if got.StorageClassName != "sc-a" {
		t.Fatalf("storageClassName must stay the captured one, got %q", got.StorageClassName)
	}
	// The two halves are asserted apart on purpose. Keeping the captured values could also come from the
	// published-metadata fallback, which fills what the enricher left empty — that fallback is the reason
	// the fields above are not blank, and it would be satisfied by an enrichment that never ran. What it
	// cannot do is UNDO a value the enricher wrote, so the stranger's shape appearing here is the signal
	// that the enricher was allowed to read it: the carry-forward had not landed when it compared uids.
	if got.VolumeMode == string(corev1.PersistentVolumeBlock) || got.StorageClassName == "sc-fast" {
		t.Fatalf("the recreated namesake's shape reached the published binding (volumeMode=%q storageClass=%q): the enricher compared the LIVE uid against itself, so the carry-forward has to run before it",
			got.VolumeMode, got.StorageClassName)
	}
}
