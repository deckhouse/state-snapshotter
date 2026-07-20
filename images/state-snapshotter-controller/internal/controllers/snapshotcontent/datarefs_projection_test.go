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
	vcctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

const (
	projTestNS      = "ns1"
	projTestPVCName = "pvc-a"
	projTestPVCUID  = "pvc-a-uid"
	projTestVSCName = "vsc-1"
	projTestVCRName = "vcr-1"
	projTestContent = "demo-content"
	projTestConUID  = "demo-content-uid"
)

var (
	projVSCGVK = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"}
	projVSGVK  = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: snapshot.KindVolumeSnapshot}
)

func projScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	return scheme
}

func projSourcePVC() *corev1.PersistentVolumeClaim {
	sc := "sc-a"
	mode := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: projTestNS, Name: projTestPVCName, UID: types.UID(projTestPVCUID)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			VolumeMode:       &mode,
		},
	}
}

// projReadyVCR builds a Ready VolumeCaptureRequest binding the PVC target to the VSC (status.data.artifactRef).
func projReadyVCR() *unstructured.Unstructured {
	target := vcpkg.Target{
		UID:        projTestPVCUID,
		APIVersion: corev1.SchemeGroupVersion.String(),
		Kind:       "PersistentVolumeClaim",
		Name:       projTestPVCName,
		Namespace:  projTestNS,
	}
	obj := vcctrl.NewVolumeCaptureRequestObject(projTestNS, projTestVCRName, metav1.OwnerReference{}, []vcpkg.Target{target})
	_ = unstructured.SetNestedSlice(obj.Object, []interface{}{
		map[string]interface{}{
			"type":   vcpkg.ConditionTypeReady,
			"status": string(metav1.ConditionTrue),
			"reason": vcpkg.ConditionReasonCompleted,
		},
	}, "status", "conditions")
	_ = unstructured.SetNestedMap(obj.Object, map[string]interface{}{
		"artifactRef": map[string]interface{}{
			"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshotContent", "name": projTestVSCName,
		},
	}, "status", "data")
	return obj
}

// projVSCUnowned builds a VolumeSnapshotContent at deletionPolicy=Delete with no owner (pre-handoff).
func projVSCUnowned() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(projVSCGVK)
	obj.SetName(projTestVSCName)
	_ = unstructured.SetNestedField(obj.Object, "Delete", "spec", "deletionPolicy")
	_ = unstructured.SetNestedField(obj.Object, true, "status", "readyToUse")
	return obj
}

func projContentTyped() *storagev1alpha1.SnapshotContent {
	return &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: projTestContent, UID: types.UID(projTestConUID)},
	}
}

// projContentObj is the unstructured SnapshotContent handed to the projection (name only).
func projContentObj() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "SnapshotContent",
		"metadata":   map[string]interface{}{"name": projTestContent},
	}}
}

func projAssertPublishedAndHandedOff(t *testing.T, cl client.Client) {
	t.Helper()
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: projTestContent}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.Data == nil {
		t.Fatalf("expected status.data to be published by the aggregator, got none")
	}
	d := *got.Status.Data
	if string(d.SourceRef.UID) != projTestPVCUID || d.ArtifactRef.Name != projTestVSCName {
		t.Fatalf("unexpected published data binding: %#v", d)
	}
	if d.StorageClassName != "sc-a" || d.VolumeMode != string(corev1.PersistentVolumeFilesystem) || len(d.AccessModes) != 1 {
		t.Fatalf("published dataRef not enriched with volume metadata: %#v", d)
	}

	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(projVSCGVK)
	if err := cl.Get(context.Background(), client.ObjectKey{Name: projTestVSCName}, vsc); err != nil {
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

// The aggregator is the single writer of SnapshotContent.status.data for a VCR domain owner: from a Ready
// VolumeCaptureRequest it enriches the binding, hands off the VolumeSnapshotContent (Retain + ownerRef),
// and publishes status.data — then requeues so the next pass re-reads the content WITH data and evaluates
// the data leg for real (the publish is a separate patch, invisible to the same-pass in-memory object).
func TestReconcileDataLegProjection_VCRDomainPublishesAndHandsOff(t *testing.T) {
	ctx := context.Background()
	scheme := projScheme(t)
	content := projContentTyped()

	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(schema.GroupVersionKind{Group: "sds-unified-snapshots-poc.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualDiskSnapshot"})
	owner.SetNamespace(projTestNS)
	owner.SetName("disk-snap")
	_ = unstructured.SetNestedField(owner.Object, projTestVCRName, "status", "captureState", "domainSpecificController", "volumeCaptureRequestName")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(projSourcePVC(), content, projReadyVCR(), projVSCUnowned()).
		Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	requeue, termReason, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection: %v", err)
	}
	if termReason != "" {
		t.Fatalf("a successful publish must not be terminal, got %q", termReason)
	}
	if !requeue {
		t.Fatalf("a fresh publish must requeue so the next pass re-reads the content with data")
	}
	projAssertPublishedAndHandedOff(t, cl)
}

// vcr-watch-core-terminal (decision D2): a FAILED data-leg VCR makes the CONTENT terminal — the projection
// returns termReason=VolumeCaptureFailed (not a requeue, no publish), which the aggregation folds into
// content.Ready and which then propagates up the content tree as ChildrenFailed.
func TestReconcileDataLegProjection_VCRFailedIsContentTerminal(t *testing.T) {
	ctx := context.Background()
	scheme := projScheme(t)
	content := projContentTyped()

	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(schema.GroupVersionKind{Group: "sds-unified-snapshots-poc.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualDiskSnapshot"})
	owner.SetNamespace(projTestNS)
	owner.SetName("disk-snap")
	_ = unstructured.SetNestedField(owner.Object, projTestVCRName, "status", "captureState", "domainSpecificController", "volumeCaptureRequestName")

	failedVCR := projReadyVCR()
	_ = unstructured.SetNestedSlice(failedVCR.Object, []interface{}{
		map[string]interface{}{
			"type":    vcpkg.ConditionTypeReady,
			"status":  string(metav1.ConditionFalse),
			"reason":  "SnapshotCreationFailed",
			"message": "csi failed",
		},
	}, "status", "conditions")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(projSourcePVC(), content, failedVCR).
		Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	requeue, termReason, termMsg, err := r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection: %v", err)
	}
	if termReason != snapshot.ReasonVolumeCaptureFailed {
		t.Fatalf("termReason = %q, want %q", termReason, snapshot.ReasonVolumeCaptureFailed)
	}
	if termMsg == "" {
		t.Fatalf("a terminal data-leg failure must carry a diagnostic message")
	}
	if requeue {
		t.Fatalf("a terminal data leg must not requeue (the content is already terminal)")
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: projTestContent}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.Data != nil {
		t.Fatalf("a failed VCR must not publish status.data, got %#v", *got.Status.Data)
	}
}

// Native-CSI data leg (§11.4): a VolumeSnapshot owner has no VCR — the fork binds it to a
// VolumeSnapshotContent (status.boundVolumeSnapshotContentName) and the domain reconciler publishes the
// captured PVC (status.sourceRef). The aggregator builds the {source PVC, VSC} binding and performs
// the same enrich + Retain/ownerRef handoff + publish.
func TestReconcileDataLegProjection_NativeCSIPublishesFromBoundVSC(t *testing.T) {
	ctx := context.Background()
	scheme := projScheme(t)
	content := projContentTyped()

	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(projVSGVK)
	owner.SetNamespace(projTestNS)
	owner.SetName("user-vs")
	_ = unstructured.SetNestedField(owner.Object, projTestVSCName, "status", "boundVolumeSnapshotContentName")
	_ = unstructured.SetNestedMap(owner.Object, map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"name":       projTestPVCName,
		"namespace":  projTestNS,
		"uid":        projTestPVCUID,
	}, "status", "sourceRef")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(projSourcePVC(), content, projVSCUnowned()).
		Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	requeue, termReason, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection: %v", err)
	}
	if termReason != "" {
		t.Fatalf("a successful native-CSI publish must not be terminal, got %q", termReason)
	}
	if !requeue {
		t.Fatalf("a fresh native-CSI publish must requeue so the next pass re-reads the content with data")
	}
	projAssertPublishedAndHandedOff(t, cl)
}

// Native-CSI size backfill (regression): the fork binds the VolumeSnapshot to its VSC
// (status.boundVolumeSnapshotContentName) BEFORE the CSI driver publishes status.restoreSize, so the first
// projection publishes status.data without size. The projection MUST NOT latch on the VSC name alone: it
// keeps re-enriching (requeue) while size is empty, backfills size once restoreSize appears, and only then
// latches. Without this the durable restore size (needed to recreate the volume on restore/export) is lost
// forever — the exact defect the e2e caught on sds-local-volume (late restoreSize).
func TestReconcileDataLegProjection_NativeCSIBackfillsSizeThenLatches(t *testing.T) {
	ctx := context.Background()
	scheme := projScheme(t)
	content := projContentTyped()

	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(projVSGVK)
	owner.SetNamespace(projTestNS)
	owner.SetName("user-vs")
	_ = unstructured.SetNestedField(owner.Object, projTestVSCName, "status", "boundVolumeSnapshotContentName")
	_ = unstructured.SetNestedMap(owner.Object, map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"name":       projTestPVCName,
		"namespace":  projTestNS,
		"uid":        projTestPVCUID,
	}, "status", "sourceRef")

	// The VSC is bound and readyToUse but has NOT published restoreSize yet (projVSCUnowned sets neither).
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(projSourcePVC(), content, projVSCUnowned()).
		Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	reconcile := func() bool {
		t.Helper()
		requeue, termReason, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true)
		if err != nil {
			t.Fatalf("reconcileDataLegProjection: %v", err)
		}
		if termReason != "" {
			t.Fatalf("a native-CSI publish must not be terminal, got %q", termReason)
		}
		return requeue
	}
	contentSize := func() string {
		t.Helper()
		got := &storagev1alpha1.SnapshotContent{}
		if err := cl.Get(ctx, client.ObjectKey{Name: projTestContent}, got); err != nil {
			t.Fatalf("get content: %v", err)
		}
		if got.Status.Data == nil {
			t.Fatalf("expected status.data to be published")
		}
		return got.Status.Data.Size
	}

	// Pass 1: publishes the binding but restoreSize is absent, so size stays empty; must requeue.
	if !reconcile() {
		t.Fatalf("first publish must requeue")
	}
	if s := contentSize(); s != "" {
		t.Fatalf("size must not be fabricated before restoreSize is published, got %q", s)
	}

	// Pass 2: restoreSize STILL absent. The projection must NOT latch on the VSC name — it keeps requeueing
	// so a later restoreSize can be backfilled.
	if !reconcile() {
		t.Fatalf("projection must keep requeueing (not latch) while status.data.size is empty")
	}
	if s := contentSize(); s != "" {
		t.Fatalf("size must still be empty while restoreSize is absent, got %q", s)
	}

	// The fork now publishes restoreSize (500Mi = 524288000 bytes) on the VSC.
	liveVSC := &unstructured.Unstructured{}
	liveVSC.SetGroupVersionKind(projVSCGVK)
	if err := cl.Get(ctx, client.ObjectKey{Name: projTestVSCName}, liveVSC); err != nil {
		t.Fatalf("get VSC: %v", err)
	}
	if err := unstructured.SetNestedField(liveVSC.Object, int64(524288000), "status", "restoreSize"); err != nil {
		t.Fatalf("set restoreSize: %v", err)
	}
	if err := cl.Update(ctx, liveVSC); err != nil {
		t.Fatalf("update VSC restoreSize: %v", err)
	}

	// Pass 3: re-enriches and backfills size (the name-only latch would have skipped this); must requeue.
	if !reconcile() {
		t.Fatalf("the size-backfill publish must requeue so the next pass re-reads the content")
	}
	if s := contentSize(); s != "500Mi" {
		t.Fatalf("size must be backfilled from restoreSize, got %q", s)
	}

	// Pass 4: size is now captured, so the projection latches — no more requeue, no churn.
	if reconcile() {
		t.Fatalf("once size is captured the projection must latch (no requeue)")
	}
	if s := contentSize(); s != "500Mi" {
		t.Fatalf("latched size must remain 500Mi, got %q", s)
	}
}
