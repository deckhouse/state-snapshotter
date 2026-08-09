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
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
	projTestVSName  = "user-vs"
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

// ---------------------------------------------------------------------------------------------------
// Bound-VSC branch: capture/import share it, so the import extras are gated on spec.mode: Import.
// ---------------------------------------------------------------------------------------------------

// projVSCWithRestoreSize is the bound VolumeSnapshotContent after the driver published restoreSize
// (500Mi = 524288000 bytes), i.e. the state in which the native-CSI leg is allowed to latch.
func projVSCWithRestoreSize() *unstructured.Unstructured {
	obj := projVSCUnowned()
	_ = unstructured.SetNestedField(obj.Object, int64(524288000), "status", "restoreSize")
	return obj
}

// projBoundVSCOwner builds a VolumeSnapshot owner already bound to the VSC and carrying the captured PVC as
// status.sourceRef. importMode stamps spec.mode: Import (the structural discriminator the projection gates
// the DataImport lookup on); a capture owner carries no spec.mode at all.
func projBoundVSCOwner(importMode bool) *unstructured.Unstructured {
	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(projVSGVK)
	owner.SetNamespace(projTestNS)
	owner.SetName(projTestVSName)
	if importMode {
		_ = unstructured.SetNestedField(owner.Object, string(storagev1alpha1.SnapshotModeImport), "spec", "mode")
	}
	_ = unstructured.SetNestedField(owner.Object, projTestVSCName, "status", "boundVolumeSnapshotContentName")
	_ = unstructured.SetNestedMap(owner.Object, map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"name":       projTestPVCName,
		"namespace":  projTestNS,
		"uid":        projTestPVCUID,
	}, "status", "sourceRef")
	return owner
}

// projDataImportForVS is a PopulateData DataImport whose spec.snapshotRef targets the VolumeSnapshot leaf
// and whose spec.storageParams carry the authoritative scratch StorageClass.
func projDataImportForVS(name string) *unstructured.Unstructured {
	di := &unstructured.Unstructured{}
	di.SetGroupVersionKind(schema.GroupVersionKind{Group: "storage-foundation.deckhouse.io", Version: "v1alpha1", Kind: "DataImport"})
	di.SetNamespace(projTestNS)
	di.SetName(name)
	_ = unstructured.SetNestedField(di.Object, snapshot.DataImportModePopulateData, "spec", "mode")
	_ = unstructured.SetNestedMap(di.Object, map[string]interface{}{
		"apiVersion": projVSGVK.GroupVersion().String(), "kind": projVSGVK.Kind, "name": projTestVSName,
	}, "spec", "snapshotRef")
	_ = unstructured.SetNestedMap(di.Object, map[string]interface{}{
		"storageClassName": importScratchStorageClass, "size": "10Gi", "volumeMode": string(corev1.PersistentVolumeFilesystem),
	}, "spec", "storageParams")
	return di
}

// projBoundVSCFixture wires the aggregator over the bound-VSC branch. The DataImport list GVK is ALWAYS
// registered and dataImports are ALWAYS visible when passed, so a capture test proves the lookup is skipped
// structurally rather than for lack of a listable type; dataImportLists counts every DataImport List that
// actually reaches the client.
func projBoundVSCFixture(t *testing.T, dataImportLists *int32, objs ...client.Object) (*SnapshotContentController, client.Client) {
	t.Helper()
	scheme := projScheme(t)
	scheme.AddKnownTypeWithName(dataImportListGVK, &unstructured.UnstructuredList{})
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if ul, ok := list.(*unstructured.UnstructuredList); ok && ul.GetObjectKind().GroupVersionKind() == dataImportListGVK {
					atomic.AddInt32(dataImportLists, 1)
				}
				return c.List(ctx, list, opts...)
			},
		}).
		Build()
	return &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}, cl
}

func projContentData(t *testing.T, cl client.Client) storagev1alpha1.SnapshotDataBinding {
	t.Helper()
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: projTestContent}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.Data == nil {
		t.Fatalf("expected status.data to be published")
	}
	return *got.Status.Data
}

func projContentResourceVersion(t *testing.T, cl client.Client) string {
	t.Helper()
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: projTestContent}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	return got.GetResourceVersion()
}

// An IMPORT VolumeSnapshot gets its StorageClass from the DataImport that staged the bytes, and that value
// wins over the live PVC the enricher can see: a PVC with the same name may reappear after a restore under a
// different class, and deriving the field from it would flap status.data between passes. The value latches
// on the next pass — no churn once it is published.
func TestReconcileDataLegProjection_BoundVSCImportPublishesStorageClassFromDataImport(t *testing.T) {
	ctx := context.Background()
	var lists int32
	r, cl := projBoundVSCFixture(t, &lists,
		projSourcePVC(), projContentTyped(), projVSCWithRestoreSize(), projDataImportForVS("di-1"))
	owner := projBoundVSCOwner(true)

	requeue, termReason, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection (pass 1): %v", err)
	}
	if termReason != "" || !requeue {
		t.Fatalf("a fresh import publish must requeue and not be terminal, got requeue=%v termReason=%q", requeue, termReason)
	}
	d := projContentData(t, cl)
	if d.StorageClassName != importScratchStorageClass {
		t.Fatalf("import storageClassName = %q, want %q (the live PVC class %q must not win)", d.StorageClassName, importScratchStorageClass, "sc-a")
	}
	if d.Size != "500Mi" || d.VolumeMode != string(corev1.PersistentVolumeFilesystem) {
		t.Fatalf("enriched metadata lost: %#v", d)
	}
	rv := projContentResourceVersion(t, cl)

	requeue, _, _, err = r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection (pass 2): %v", err)
	}
	if requeue {
		t.Fatalf("the second pass must latch on the published import storageClassName (no requeue)")
	}
	if got := projContentResourceVersion(t, cl); got != rv {
		t.Fatalf("a latched pass must not write the content: resourceVersion %q -> %q", rv, got)
	}
	if lists == 0 {
		t.Fatalf("an import owner must reverse-look-up its DataImport")
	}
}

// Self-heal on the shared branch: an import content published before the class was projected already matches
// artifactRef + size, so without the storageClassName term in the latch it would keep an empty class forever.
func TestReconcileDataLegProjection_BoundVSCImportBackfillsStorageClassOnStaleContent(t *testing.T) {
	ctx := context.Background()
	content := projContentTyped()
	content.Status.Data = &storagev1alpha1.SnapshotDataBinding{
		SourceRef: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: projTestPVCName,
			Namespace: projTestNS, UID: types.UID(projTestPVCUID),
		},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: "snapshot.storage.k8s.io/v1", Kind: snapshot.KindVolumeSnapshotContent, Name: projTestVSCName,
		},
		VolumeMode: string(corev1.PersistentVolumeFilesystem),
		Size:       "500Mi",
	}
	var lists int32
	r, cl := projBoundVSCFixture(t, &lists,
		content, projVSCWithRestoreSize(), projDataImportForVS("di-1"))

	requeue, termReason, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), projBoundVSCOwner(true), projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection: %v", err)
	}
	if termReason != "" {
		t.Fatalf("a self-heal publish must not be terminal, got %q", termReason)
	}
	if !requeue {
		t.Fatalf("a content published without the import storageClassName must NOT latch")
	}
	if sc := projContentData(t, cl).StorageClassName; sc != importScratchStorageClass {
		t.Fatalf("stale import content did not self-heal: storageClassName = %q, want %q", sc, importScratchStorageClass)
	}
}

// No wedge when the DataImport is not resolved. Two shapes reach this state and both must publish exactly as
// before the fix — without a class, without blocking, and latching on the next pass:
//   - none: d8 has not created it yet, or its idle TTL already reaped it after a completed import (which is
//     the STEADY state of every imported leaf, so blocking here would freeze them all);
//   - two: the fail-closed cardinality fault, which the import binder surfaces on VolumeSnapshot.status.error
//     — the aggregator must not turn it into a second, silent block in a branch shared with capture.
func TestReconcileDataLegProjection_BoundVSCImportPublishesWithUnresolvedDataImport(t *testing.T) {
	for _, tt := range []struct {
		name        string
		dataImports []client.Object
	}{
		{name: "no DataImport (never created, or reaped by TTL)"},
		{name: "two DataImports (ambiguous, fail-closed)", dataImports: []client.Object{projDataImportForVS("di-1"), projDataImportForVS("di-2")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			var lists int32
			// No live source PVC: the enricher then leaves the class empty, so "no class was invented" is
			// observable instead of being masked by a PVC-derived value.
			objs := append([]client.Object{projContentTyped(), projVSCWithRestoreSize()}, tt.dataImports...)
			r, cl := projBoundVSCFixture(t, &lists, objs...)
			owner := projBoundVSCOwner(true)

			requeue, termReason, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true)
			if err != nil {
				t.Fatalf("reconcileDataLegProjection (pass 1): %v", err)
			}
			if termReason != "" {
				t.Fatalf("an unresolved DataImport must not be terminal here, got %q", termReason)
			}
			if !requeue {
				t.Fatalf("the first publish must requeue so the next pass re-reads the content")
			}
			d := projContentData(t, cl)
			if d.ArtifactRef.Name != projTestVSCName {
				t.Fatalf("the data leg must be published even without a DataImport, got %#v", d.ArtifactRef)
			}
			if d.StorageClassName != "" {
				t.Fatalf("no class is known here; the projection must not invent one, got %q", d.StorageClassName)
			}
			rv := projContentResourceVersion(t, cl)

			requeue, _, _, err = r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true)
			if err != nil {
				t.Fatalf("reconcileDataLegProjection (pass 2): %v", err)
			}
			if requeue {
				t.Fatalf("with the class unresolved the latch must stay exactly the pre-fix one (no requeue)")
			}
			if got := projContentResourceVersion(t, cl); got != rv {
				t.Fatalf("a latched pass must not write the content: resourceVersion %q -> %q", rv, got)
			}
		})
	}
}

// An import leaf whose DataImport is already reaped keeps the class it published while the DataImport was
// alive: the latch must not compare against the (now unknown) expected class and must not re-publish, or the
// re-publish would rebuild the binding from scratch and drop the durable metadata.
func TestReconcileDataLegProjection_BoundVSCImportKeepsPublishedClassAfterDataImportReaped(t *testing.T) {
	ctx := context.Background()
	content := projContentTyped()
	content.Status.Data = &storagev1alpha1.SnapshotDataBinding{
		SourceRef: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: projTestPVCName,
			Namespace: projTestNS, UID: types.UID(projTestPVCUID),
		},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: "snapshot.storage.k8s.io/v1", Kind: snapshot.KindVolumeSnapshotContent, Name: projTestVSCName,
		},
		VolumeMode:       string(corev1.PersistentVolumeFilesystem),
		AccessModes:      []string{string(corev1.ReadWriteOnce)},
		StorageClassName: importScratchStorageClass,
		Size:             "500Mi",
	}
	var lists int32
	// The DataImport is gone (TTL) and so is the imported leaf's source PVC — nothing left to re-derive from.
	r, cl := projBoundVSCFixture(t, &lists, content, projVSCWithRestoreSize())

	requeue, _, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), projBoundVSCOwner(true), projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection: %v", err)
	}
	if requeue {
		t.Fatalf("a reaped DataImport must not un-latch an already published import content")
	}
	d := projContentData(t, cl)
	if d.StorageClassName != importScratchStorageClass || d.VolumeMode != string(corev1.PersistentVolumeFilesystem) ||
		len(d.AccessModes) != 1 || d.Size != "500Mi" {
		t.Fatalf("durable metadata must survive a reaped DataImport: %#v", d)
	}
}

// Capture regression guard (structural gate): a capture VolumeSnapshot must not perform the DataImport
// reverse-lookup AT ALL — not even to discover there is nothing to apply. A matching DataImport is present in
// the namespace and the list type is registered, so the only thing keeping it out is spec.mode. The published
// class therefore stays the live PVC's, and the leg latches on the second pass.
func TestReconcileDataLegProjection_BoundVSCCaptureSkipsDataImportLookupAndLatches(t *testing.T) {
	ctx := context.Background()
	var lists int32
	r, cl := projBoundVSCFixture(t, &lists,
		projSourcePVC(), projContentTyped(), projVSCWithRestoreSize(), projDataImportForVS("di-1"))
	owner := projBoundVSCOwner(false)

	requeue, termReason, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection (pass 1): %v", err)
	}
	if termReason != "" || !requeue {
		t.Fatalf("a fresh capture publish must requeue and not be terminal, got requeue=%v termReason=%q", requeue, termReason)
	}
	if sc := projContentData(t, cl).StorageClassName; sc != "sc-a" {
		t.Fatalf("capture storageClassName must come from the live PVC, got %q", sc)
	}
	rv := projContentResourceVersion(t, cl)

	requeue, _, _, err = r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection (pass 2): %v", err)
	}
	if requeue {
		t.Fatalf("a capture leg with size captured must latch on the second pass (no requeue)")
	}
	if got := projContentResourceVersion(t, cl); got != rv {
		t.Fatalf("a latched capture pass must not write the content: resourceVersion %q -> %q", rv, got)
	}
	if n := atomic.LoadInt32(&lists); n != 0 {
		t.Fatalf("a capture owner must not list DataImports at all, got %d List calls", n)
	}
}

// Capture regression guard (durability): after the source PVC is deleted the leg must stay latched. A
// re-publish rebuilds the binding from {sourceRef, artifactRef} only, and the enricher silently skips a gone
// PVC — so a latch that failed here would wipe volumeMode/accessModes/storageClassName from an already
// durable content, and an empty volumeMode fail-closes restore.
func TestReconcileDataLegProjection_BoundVSCCaptureKeepsMetadataAfterSourcePVCDeleted(t *testing.T) {
	ctx := context.Background()
	var lists int32
	r, cl := projBoundVSCFixture(t, &lists, projSourcePVC(), projContentTyped(), projVSCWithRestoreSize())
	owner := projBoundVSCOwner(false)

	if requeue, _, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true); err != nil || !requeue {
		t.Fatalf("first publish: requeue=%v err=%v", requeue, err)
	}
	before := projContentData(t, cl)
	if before.StorageClassName != "sc-a" || before.VolumeMode == "" || len(before.AccessModes) != 1 {
		t.Fatalf("fixture did not enrich the capture binding: %#v", before)
	}
	rv := projContentResourceVersion(t, cl)

	if err := cl.Delete(ctx, projSourcePVC()); err != nil {
		t.Fatalf("delete source PVC: %v", err)
	}

	requeue, _, _, err := r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true)
	if err != nil {
		t.Fatalf("reconcileDataLegProjection after PVC deletion: %v", err)
	}
	if requeue {
		t.Fatalf("a deleted source PVC must not un-latch a published capture content")
	}
	if got := projContentResourceVersion(t, cl); got != rv {
		t.Fatalf("nothing must be written after the source PVC is gone: resourceVersion %q -> %q", rv, got)
	}
	after := projContentData(t, cl)
	if after.StorageClassName != before.StorageClassName || after.VolumeMode != before.VolumeMode ||
		len(after.AccessModes) != len(before.AccessModes) || after.Size != before.Size {
		t.Fatalf("durable metadata changed after the source PVC was deleted: %#v -> %#v", before, after)
	}
}

// publishDataBindings applies the import class AFTER enrichment, so it overrides whatever the enricher read
// off a live PVC (anti-flap: a same-named PVC may reappear after a restore under another class). An empty
// value means "nothing to apply" and must leave the enriched result untouched — never clear the field.
func TestPublishDataBindings_ImportStorageClassOverridesEnricher(t *testing.T) {
	for _, tt := range []struct {
		name                   string
		importStorageClassName string
		want                   string
	}{
		{name: "non-empty overrides the live PVC class", importStorageClassName: importScratchStorageClass, want: importScratchStorageClass},
		{name: "empty keeps the enriched class", importStorageClassName: "", want: "sc-a"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := projScheme(t)
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
				WithObjects(projSourcePVC(), projContentTyped(), projVSCWithRestoreSize()).
				Build()
			r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

			binding := storagev1alpha1.SnapshotDataBinding{
				SourceRef: storagev1alpha1.SnapshotSubjectRef{
					APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: projTestPVCName,
					Namespace: projTestNS, UID: types.UID(projTestPVCUID),
				},
				ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
					APIVersion: volumeSnapshotContentAPIVersion, Kind: kindVolumeSnapshotContent, Name: projTestVSCName,
				},
			}
			if _, err := r.publishDataBindings(ctx, projTestContent, []storagev1alpha1.SnapshotDataBinding{binding}, tt.importStorageClassName); err != nil {
				t.Fatalf("publishDataBindings: %v", err)
			}
			d := projContentData(t, cl)
			if d.StorageClassName != tt.want {
				t.Fatalf("published storageClassName = %q, want %q", d.StorageClassName, tt.want)
			}
			// The rest of the enrichment must be unaffected either way.
			if d.VolumeMode != string(corev1.PersistentVolumeFilesystem) || len(d.AccessModes) != 1 || d.Size != "500Mi" {
				t.Fatalf("enrichment damaged by the import class stamp: %#v", d)
			}
		})
	}
}
