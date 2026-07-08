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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// These tests cover the main-owned capture-leg lifecycle (reconcileOwnerCaptureLegs) that moved off the
// binder onto the SnapshotContentController aggregator (main-owned commonController, decision #10): the
// eager-init, the manifestCaptured/dataCaptured latch-and-reap (latch strictly before the delete), the
// native-CSI dataCaptured latch, and the subtreeManifestsPersisted snapshot-mirror. They reuse the
// projTest* fixtures + helpers from datarefs_projection_test.go (same package).

const (
	clOwnerName = "owner-snap"
	clMCRName   = "mcr-1"
	clMCPName   = "mcp-1"
)

var clSnapGVK = storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot")

func captureLegsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add ss scheme: %v", err)
	}
	return scheme
}

func captureLegsController(cl client.Client, domainGVK schema.GroupVersionKind, requiresData bool) *SnapshotContentController {
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}
	r.MarkDomainCaptureKind(domainGVK)
	if requiresData {
		r.MarkRequiresDataArtifact(domainGVK.Kind, true)
	}
	return r
}

// captureLegsOwner builds a domain snapshot at capture barrier 1 (phase Planned) bound to projTestContent,
// carrying the domain-written request names the aggregator reaps.
func captureLegsOwner(gvk schema.GroupVersionKind, boundContent, mcrName, vcrName, phase string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	o.SetNamespace(projTestNS)
	o.SetName(clOwnerName)
	if boundContent != "" {
		_ = unstructured.SetNestedField(o.Object, boundContent, "status", "boundSnapshotContentName")
	}
	if phase != "" {
		_ = unstructured.SetNestedField(o.Object, phase, "status", "captureState", "domainSpecificController", "phase")
	}
	if mcrName != "" {
		_ = unstructured.SetNestedField(o.Object, mcrName, "status", "captureState", "domainSpecificController", "manifestCaptureRequestName")
	}
	if vcrName != "" {
		_ = unstructured.SetNestedField(o.Object, vcrName, "status", "captureState", "domainSpecificController", "volumeCaptureRequestName")
	}
	return o
}

// captureLegsContentObj is the reconcile input: an unstructured SnapshotContent whose spec.snapshotRef
// points at the owning snapshot (the aggregator resolves the owner from it).
func captureLegsContentObj(ownerGVK schema.GroupVersionKind, subtreePersisted *bool) *unstructured.Unstructured {
	c := &unstructured.Unstructured{}
	c.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("SnapshotContent"))
	c.SetName(projTestContent)
	_ = unstructured.SetNestedMap(c.Object, map[string]interface{}{
		"apiVersion": ownerGVK.GroupVersion().String(),
		"kind":       ownerGVK.Kind,
		"name":       clOwnerName,
		"namespace":  projTestNS,
	}, "spec", "snapshotRef")
	if subtreePersisted != nil {
		_ = unstructured.SetNestedField(c.Object, *subtreePersisted, "status", "subtreeManifestsPersisted")
	}
	return c
}

func captureLegsData() *storagev1alpha1.SnapshotDataBinding {
	return &storagev1alpha1.SnapshotDataBinding{
		Source: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: projTestPVCName,
			Namespace: projTestNS, UID: types.UID(projTestPVCUID),
		},
		Artifact: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: projTestVSCName,
		},
		StorageClassName: "sc-a",
		VolumeMode:       string(corev1.PersistentVolumeFilesystem),
		AccessModes:      []string{string(corev1.ReadWriteOnce)},
	}
}

func captureLegsContentTyped(mcpName string, data *storagev1alpha1.SnapshotDataBinding) *storagev1alpha1.SnapshotContent {
	c := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: projTestContent, UID: types.UID(projTestConUID)}}
	c.Status.ManifestCheckpointName = mcpName
	c.Status.Data = data
	return c
}

func captureLegsReadyOwnedMCP() *ssv1alpha1.ManifestCheckpoint {
	ctrlTrue := true
	mcp := &ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: clMCPName}}
	mcp.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "SnapshotContent",
		Name: projTestContent, UID: types.UID(projTestConUID), Controller: &ctrlTrue,
	}}
	mcp.Status.Conditions = []metav1.Condition{{
		Type: ssv1alpha1.ManifestCheckpointConditionTypeReady, Status: metav1.ConditionTrue,
		Reason: "Ready", LastTransitionTime: metav1.Now(),
	}}
	return mcp
}

func captureLegsMCR() *ssv1alpha1.ManifestCaptureRequest {
	mcr := &ssv1alpha1.ManifestCaptureRequest{ObjectMeta: metav1.ObjectMeta{Namespace: projTestNS, Name: clMCRName}}
	mcr.Status.CheckpointName = clMCPName
	return mcr
}

// captureLegsOwnedVSC returns a VSC already handed off (Retain + owned by the content).
func captureLegsOwnedVSC() *unstructured.Unstructured {
	ctrlTrue := true
	o := projVSCUnowned()
	_ = unstructured.SetNestedField(o.Object, "Retain", "spec", "deletionPolicy")
	o.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "SnapshotContent",
		Name: projTestContent, UID: types.UID(projTestConUID), Controller: &ctrlTrue,
	}})
	return o
}

func captureLegsOwnerLatch(t *testing.T, cl client.Client, gvk schema.GroupVersionKind, leg string) (bool, bool) {
	t.Helper()
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: projTestNS, Name: clOwnerName}, o); err != nil {
		t.Fatalf("get owner: %v", err)
	}
	v, found, err := unstructured.NestedBool(o.Object, "status", "captureState", "commonController", leg)
	if err != nil {
		t.Fatalf("read latch %q: %v", leg, err)
	}
	return v, found
}

func captureLegsMCRExists(t *testing.T, cl client.Client) bool {
	t.Helper()
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	err := cl.Get(context.Background(), client.ObjectKey{Namespace: projTestNS, Name: clMCRName}, mcr)
	return err == nil
}

func captureLegsVCRExists(t *testing.T, cl client.Client) bool {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(projReadyVCR().GroupVersionKind())
	err := cl.Get(context.Background(), client.ObjectKey{Namespace: projTestNS, Name: projTestVCRName}, obj)
	return err == nil
}

// Manifest leg: once the MCP handoff is durable (content.status.manifestCheckpointName points at a Ready
// MCP owned by the content, and the MCR references that MCP), the aggregator latches
// commonController.manifestCaptured on the owner AND reaps the MCR — latch strictly before the delete.
func TestReconcileOwnerCaptureLegs_ManifestLatchAndReapAfterHandoff(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)
	owner := captureLegsOwner(clSnapGVK, projTestContent, clMCRName, "", string(storagev1alpha1.SnapshotCapturePhasePlanned))

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped(clMCPName, nil), captureLegsReadyOwnedMCP(), captureLegsMCR()).
		Build()
	r := captureLegsController(cl, clSnapGVK, false)

	requeue, treason, _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil))
	if err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}
	if treason != "" {
		t.Fatalf("durable manifest handoff must not be terminal, got %q", treason)
	}
	if requeue {
		t.Fatalf("durable manifest handoff must not requeue")
	}
	if got, found := captureLegsOwnerLatch(t, cl, clSnapGVK, "manifestCaptured"); !found || !got {
		t.Fatalf("want commonController.manifestCaptured=true, got found=%v value=%v", found, got)
	}
	if captureLegsMCRExists(t, cl) {
		t.Fatalf("expected the transient MCR to be reaped after durable handoff")
	}
}

// Data leg (VCR domain): once the aggregator-published status.data covers the VCR targets AND the bound
// VSC is owned by the content, the aggregator latches commonController.dataCaptured and reaps the VCR.
func TestReconcileOwnerCaptureLegs_DataLegVCRLatchAndReapAfterHandoff(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)
	owner := captureLegsOwner(clSnapGVK, projTestContent, "", projTestVCRName, string(storagev1alpha1.SnapshotCapturePhasePlanned))

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped("", captureLegsData()), projReadyVCR(), captureLegsOwnedVSC(), projSourcePVC()).
		Build()
	r := captureLegsController(cl, clSnapGVK, true)

	requeue, treason, _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil))
	if err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}
	if treason != "" {
		t.Fatalf("durable data handoff must not be terminal, got %q", treason)
	}
	if requeue {
		t.Fatalf("durable data handoff must not requeue")
	}
	if got, found := captureLegsOwnerLatch(t, cl, clSnapGVK, "dataCaptured"); !found || !got {
		t.Fatalf("want commonController.dataCaptured=true, got found=%v value=%v", found, got)
	}
	if captureLegsVCRExists(t, cl) {
		t.Fatalf("expected the transient VCR to be reaped after durable handoff")
	}
}

// A Ready data-leg VCR whose published status.data does not yet cover the targets must NOT be handed off:
// the aggregator requeues and leaves the VCR + latch untouched (no premature latch that would suppress
// re-creation before the data is durable).
func TestReconcileOwnerCaptureLegs_DataLegVCRPendingRequeues(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)
	owner := captureLegsOwner(clSnapGVK, projTestContent, "", projTestVCRName, string(storagev1alpha1.SnapshotCapturePhasePlanned))

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped("", nil), projReadyVCR(), projSourcePVC()).
		Build()
	r := captureLegsController(cl, clSnapGVK, true)

	requeue, treason, _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil))
	if err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}
	if treason != "" {
		t.Fatalf("pending data leg must not be terminal, got %q", treason)
	}
	if !requeue {
		t.Fatalf("pending data leg should requeue")
	}
	// eager-init declares dataCaptured=false (the leg exists); it must not be latched true yet.
	if got, _ := captureLegsOwnerLatch(t, cl, clSnapGVK, "dataCaptured"); got {
		t.Fatalf("pending data leg must not latch dataCaptured=true")
	}
	if !captureLegsVCRExists(t, cl) {
		t.Fatalf("pending data leg must not reap the VCR")
	}
}

// A failed data-leg VCR surfaces a terminal VolumeCaptureFailed reason (folded into the owner Ready mirror
// by the caller); no latch, no reap.
func TestReconcileOwnerCaptureLegs_DataLegVCRFailedIsTerminal(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)
	owner := captureLegsOwner(clSnapGVK, projTestContent, "", projTestVCRName, string(storagev1alpha1.SnapshotCapturePhasePlanned))

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
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped("", nil), failedVCR, projSourcePVC()).
		Build()
	r := captureLegsController(cl, clSnapGVK, true)

	_, treason, _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil))
	if err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}
	if treason != snapshot.ReasonVolumeCaptureFailed {
		t.Fatalf("expected terminal %q, got %q", snapshot.ReasonVolumeCaptureFailed, treason)
	}
	if got, _ := captureLegsOwnerLatch(t, cl, clSnapGVK, "dataCaptured"); got {
		t.Fatalf("failed data leg must not latch dataCaptured=true")
	}
	if !captureLegsVCRExists(t, cl) {
		t.Fatalf("failed VCR must not be reaped (operator needs to see it)")
	}
}

// Native-CSI data leg (§11.4): a VolumeSnapshot owner has no VCR — the aggregator latches dataCaptured
// once the content carries a published status.data (the projection performs the VSC handoff first). No
// request to reap.
func TestReconcileOwnerCaptureLegs_NativeCSIDataCaptured(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)
	owner := captureLegsOwner(projVSGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped("", captureLegsData())).
		Build()
	r := captureLegsController(cl, projVSGVK, true)

	requeue, _, _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(projVSGVK, nil))
	if err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}
	if requeue {
		t.Fatalf("published native-CSI data must not requeue")
	}
	if got, found := captureLegsOwnerLatch(t, cl, projVSGVK, "dataCaptured"); !found || !got {
		t.Fatalf("want commonController.dataCaptured=true, got found=%v value=%v", found, got)
	}
}

// Native-CSI without published data yet requeues and does not latch.
func TestReconcileOwnerCaptureLegs_NativeCSIPendingRequeues(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)
	owner := captureLegsOwner(projVSGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped("", nil)).
		Build()
	r := captureLegsController(cl, projVSGVK, true)

	requeue, _, _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(projVSGVK, nil))
	if err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}
	if !requeue {
		t.Fatalf("native-CSI without published data should requeue")
	}
	if got, _ := captureLegsOwnerLatch(t, cl, projVSGVK, "dataCaptured"); got {
		t.Fatalf("native-CSI without data must not latch dataCaptured=true")
	}
}

// subtreeManifestsPersisted mirror: a persisted content latch is mirrored (true-only, monotonic) onto the
// owner's commonController; a false/absent content latch leaves the mirror unset.
func TestReconcileOwnerCaptureLegs_SubtreeManifestsPersistedMirror(t *testing.T) {
	trueV := true
	falseV := false
	for _, tc := range []struct {
		name       string
		persisted  *bool
		wantMirror bool
	}{
		{"persisted true mirrors", &trueV, true},
		{"persisted false no-op", &falseV, false},
		{"absent no-op", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := captureLegsScheme(t)
			owner := captureLegsOwner(clSnapGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))

			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(owner).
				WithObjects(owner, captureLegsContentTyped("", nil)).
				Build()
			r := captureLegsController(cl, clSnapGVK, false)

			if _, _, _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, tc.persisted)); err != nil {
				t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
			}
			got, found := captureLegsOwnerLatch(t, cl, clSnapGVK, "subtreeManifestsPersisted")
			if tc.wantMirror && (!found || !got) {
				t.Fatalf("want subtreeManifestsPersisted mirror=true, got found=%v value=%v", found, got)
			}
			if !tc.wantMirror && found {
				t.Fatalf("subtreeManifestsPersisted mirror must stay unset for a %s content latch", tc.name)
			}
		})
	}
}

// eager-init declares the applicable core-owned capture legs on takeover: manifestCaptured=false always,
// dataCaptured=false only for data-artifact kinds (presence declares the leg for CoreCaptureOutcome).
func TestReconcileOwnerCaptureLegs_EagerInitDeclaresLegs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		requiresData bool
		wantData     bool
	}{
		{"manifest-only kind", false, false},
		{"data-artifact kind", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := captureLegsScheme(t)
			owner := captureLegsOwner(clSnapGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))

			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(owner).
				WithObjects(owner, captureLegsContentTyped("", nil)).
				Build()
			r := captureLegsController(cl, clSnapGVK, tc.requiresData)

			if _, _, _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil)); err != nil {
				t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
			}
			// manifestCaptured leg is always declared (init false, no MCR to latch -> stays false).
			mv, mfound := captureLegsOwnerLatch(t, cl, clSnapGVK, "manifestCaptured")
			if !mfound {
				t.Fatalf("manifestCaptured leg must be declared (present)")
			}
			if mv {
				t.Fatalf("manifestCaptured must be false with no durable handoff")
			}
			_, dfound := captureLegsOwnerLatch(t, cl, clSnapGVK, "dataCaptured")
			if dfound != tc.wantData {
				t.Fatalf("dataCaptured leg declared=%v, want %v", dfound, tc.wantData)
			}
		})
	}
}

func captureLegsChildRef(gvk schema.GroupVersionKind, name string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": gvk.GroupVersion().String(),
		"kind":       gvk.Kind,
		"name":       name,
	}
}

func captureLegsWithChildrenRefs(owner *unstructured.Unstructured, refs ...map[string]interface{}) *unstructured.Unstructured {
	items := make([]interface{}, 0, len(refs))
	for _, r := range refs {
		items = append(items, r)
	}
	_ = unstructured.SetNestedSlice(owner.Object, items, "status", "childrenSnapshotRefs")
	return owner
}

func captureLegsChildSnapshot(gvk schema.GroupVersionKind, name string, subtreePlanned bool) *unstructured.Unstructured {
	c := &unstructured.Unstructured{}
	c.SetGroupVersionKind(gvk)
	c.SetNamespace(projTestNS)
	c.SetName(name)
	if subtreePlanned {
		_ = unstructured.SetNestedField(c.Object, true, "status", "captureState", "commonController", "subtreePlanned")
	}
	return c
}

// subtreePlanned: a leaf owner (no declared children) latches subtreePlanned immediately once planned.
func TestReconcileOwnerCaptureLegs_SubtreePlannedLeafLatches(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)
	owner := captureLegsOwner(clSnapGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped("", nil)).
		Build()
	r := captureLegsController(cl, clSnapGVK, false)

	requeue, _, _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil))
	if err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}
	if requeue {
		t.Fatalf("a leaf owner must not requeue (subtree planned immediately)")
	}
	if got, found := captureLegsOwnerLatch(t, cl, clSnapGVK, "subtreePlanned"); !found || !got {
		t.Fatalf("leaf owner must latch subtreePlanned=true, got found=%v value=%v", found, got)
	}
}

// subtreePlanned: an owner whose every DIRECT child already carries subtreePlanned=true latches its own.
func TestReconcileOwnerCaptureLegs_SubtreePlannedLatchesWhenChildrenPlanned(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)
	owner := captureLegsWithChildrenRefs(
		captureLegsOwner(clSnapGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned)),
		captureLegsChildRef(clSnapGVK, "child-a"),
	)

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped("", nil), captureLegsChildSnapshot(clSnapGVK, "child-a", true)).
		Build()
	r := captureLegsController(cl, clSnapGVK, false)

	requeue, _, _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil))
	if err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}
	if requeue {
		t.Fatalf("owner with all children planned must not requeue")
	}
	if got, found := captureLegsOwnerLatch(t, cl, clSnapGVK, "subtreePlanned"); !found || !got {
		t.Fatalf("owner must latch subtreePlanned=true when all children are planned")
	}
}

// subtreePlanned: while a DIRECT child's subtree is not planned (latch absent), or a declared child is not
// created yet, the owner does not latch and requeues (the 500 ms self-requeue re-evaluates bottom-up).
func TestReconcileOwnerCaptureLegs_SubtreePlannedRequeuesWhilePending(t *testing.T) {
	for _, tc := range []struct {
		name      string
		childObjs []client.Object
	}{
		{"child not planned yet", []client.Object{captureLegsChildSnapshot(clSnapGVK, "child-a", false)}},
		{"child not created yet", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := captureLegsScheme(t)
			owner := captureLegsWithChildrenRefs(
				captureLegsOwner(clSnapGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned)),
				captureLegsChildRef(clSnapGVK, "child-a"),
			)
			objs := []client.Object{owner, captureLegsContentTyped("", nil)}
			objs = append(objs, tc.childObjs...)

			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(owner).
				WithObjects(objs...).
				Build()
			r := captureLegsController(cl, clSnapGVK, false)

			requeue, _, _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil))
			if err != nil {
				t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
			}
			if !requeue {
				t.Fatalf("owner with a pending child subtree must requeue")
			}
			if _, found := captureLegsOwnerLatch(t, cl, clSnapGVK, "subtreePlanned"); found {
				t.Fatalf("owner must not latch subtreePlanned while a child subtree is pending")
			}
		})
	}
}

// Writer-switch + barrier guards: the aggregator does not touch the owner's commonController before the
// owner has adopted THIS content, before capture barrier 1 (phase>=Planned), for a non-domain owner, or
// for ownerless (bucket) content.
func TestReconcileOwnerCaptureLegs_Guards(t *testing.T) {
	for _, tc := range []struct {
		name          string
		bound         string
		phase         string
		markDomain    bool
		ownerlessRef  bool
		wantNoInitLeg bool
	}{
		{name: "not bound to this content", bound: "other", phase: string(storagev1alpha1.SnapshotCapturePhasePlanned), markDomain: true, wantNoInitLeg: true},
		{name: "before Planned", bound: projTestContent, phase: string(storagev1alpha1.SnapshotCapturePhasePlanning), markDomain: true, wantNoInitLeg: true},
		{name: "non-domain owner", bound: projTestContent, phase: string(storagev1alpha1.SnapshotCapturePhasePlanned), markDomain: false, wantNoInitLeg: true},
		{name: "ownerless content", bound: projTestContent, phase: string(storagev1alpha1.SnapshotCapturePhasePlanned), markDomain: true, ownerlessRef: true, wantNoInitLeg: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := captureLegsScheme(t)
			owner := captureLegsOwner(clSnapGVK, tc.bound, "", "", tc.phase)

			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(owner).
				WithObjects(owner, captureLegsContentTyped("", nil)).
				Build()
			r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}
			if tc.markDomain {
				r.MarkDomainCaptureKind(clSnapGVK)
			}

			contentObj := captureLegsContentObj(clSnapGVK, nil)
			if tc.ownerlessRef {
				unstructured.RemoveNestedField(contentObj.Object, "spec", "snapshotRef")
			}

			requeue, treason, _, err := r.reconcileOwnerCaptureLegs(ctx, contentObj)
			if err != nil {
				t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
			}
			if requeue || treason != "" {
				t.Fatalf("guard %q must be a no-op, got requeue=%v treason=%q", tc.name, requeue, treason)
			}
			if _, found := captureLegsOwnerLatch(t, cl, clSnapGVK, "manifestCaptured"); found == tc.wantNoInitLeg && found {
				t.Fatalf("guard %q must not eager-init any capture leg", tc.name)
			}
		})
	}
}
