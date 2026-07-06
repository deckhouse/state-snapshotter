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

package genericbinder

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	vcctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

const (
	domainTestNS      = "ns1"
	domainTestSnap    = "disk-snap"
	domainTestSnapUID = "disk-snap-uid"
	domainTestPVCName = "pvc-a"
	domainTestPVCUID  = "pvc-a-uid"
	domainTestContent = "demo-content"
	domainTestConUID  = "demo-content-uid"
	domainTestVSCName = "vsc-1"
)

var demoDiskSnapshotGVK = demov1alpha1.SchemeGroupVersion.WithKind("DemoVirtualDiskSnapshot")
var volumeSnapshotContentGVK = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"}

func domainTestVCRName() string { return vcpkg.SnapshotOwnedVCRName(types.UID(domainTestSnapUID)) }

func domainTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	if err := demov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add demo scheme: %v", err)
	}
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add ss scheme: %v", err)
	}
	return scheme
}

var demoVMSnapshotGVK = demov1alpha1.SchemeGroupVersion.WithKind("DemoVirtualMachineSnapshot")

func domainTestPVC() *corev1.PersistentVolumeClaim {
	sc := "sc-a"
	mode := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: domainTestNS, Name: domainTestPVCName, UID: types.UID(domainTestPVCUID)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			VolumeMode:       &mode,
			// No VolumeName: enrichment skips the bound-PV fsType read (no PV installed in this test).
		},
	}
}

func domainTestSnapshotContent() *storagev1alpha1.SnapshotContent {
	return &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: domainTestContent, UID: types.UID(domainTestConUID)},
	}
}

func domainTestVCRTarget() vcpkg.Target {
	return vcpkg.Target{
		UID:        domainTestPVCUID,
		APIVersion: corev1.SchemeGroupVersion.String(),
		Kind:       "PersistentVolumeClaim",
		Name:       domainTestPVCName,
		Namespace:  domainTestNS,
	}
}

// domainTestReadyVCR builds a Ready VolumeCaptureRequest whose dataRefs bind the PVC target to the VSC.
func domainTestReadyVCR(withDataRefs bool) *unstructured.Unstructured {
	obj := vcctrl.NewVolumeCaptureRequestObject(domainTestNS, domainTestVCRName(), metav1.OwnerReference{}, []vcpkg.Target{domainTestVCRTarget()})
	if withDataRefs {
		_ = unstructured.SetNestedSlice(obj.Object, []interface{}{
			map[string]interface{}{
				"type":   vcpkg.ConditionTypeReady,
				"status": string(metav1.ConditionTrue),
				"reason": vcpkg.ConditionReasonCompleted,
			},
		}, "status", "conditions")
		// status.data carries only the artifact; the captured PVC identity comes from spec.target
		// (set by NewVolumeCaptureRequestObject above).
		_ = unstructured.SetNestedMap(obj.Object, map[string]interface{}{
			"artifact": map[string]interface{}{
				"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshotContent", "name": domainTestVSCName,
			},
		}, "status", "data")
	}
	return obj
}

// domainTestVSC builds a VolumeSnapshotContent at deletionPolicy=Delete with no owner (pre-handoff).
func domainTestVSC() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(volumeSnapshotContentGVK)
	obj.SetName(domainTestVSCName)
	_ = unstructured.SetNestedField(obj.Object, "Delete", "spec", "deletionPolicy")
	_ = unstructured.SetNestedField(obj.Object, true, "status", "readyToUse")
	return obj
}

func domainTestDemoSnapshotUnstructured(t *testing.T, vcrName string) *unstructured.Unstructured {
	t.Helper()
	snap := &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: domainTestNS, Name: domainTestSnap, UID: types.UID(domainTestSnapUID)},
		Status: demov1alpha1.DemoVirtualDiskSnapshotStatus{
			CaptureState: &storagev1alpha1.CaptureStateStatus{
				DomainSpecificController: &storagev1alpha1.DomainSpecificControllerCaptureState{
					VolumeCaptureRequestName: vcrName,
				},
			},
		},
	}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(snap)
	if err != nil {
		t.Fatalf("convert demo snapshot to unstructured: %v", err)
	}
	obj := &unstructured.Unstructured{Object: raw}
	obj.SetGroupVersionKind(demoDiskSnapshotGVK)
	return obj
}

func domainTestVCRExists(t *testing.T, cl client.Client) bool {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: domainTestNS, Name: domainTestVCRName()}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return false
		}
		t.Fatalf("get VCR: %v", err)
	}
	return true
}

// A Ready data-leg VCR is fully handed off by the binder: dataRefs are enriched + published onto the
// SnapshotContent, the bound VolumeSnapshotContent is re-owned by the content at deletionPolicy=Retain,
// the domain status.dataCaptured marker is stamped (clearing volumeCaptureRequestName), and the transient
// VCR is deleted. This restores the data-leg handoff coverage that moved from demo to the common binder.
func TestEnsureDomainContentLinks_DataLegHandoff(t *testing.T) {
	ctx := context.Background()
	scheme := domainTestScheme(t)
	demoObj := domainTestDemoSnapshotUnstructured(t, domainTestVCRName())
	content := domainTestSnapshotContent()

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&demov1alpha1.DemoVirtualDiskSnapshot{}, &storagev1alpha1.SnapshotContent{}).
		WithObjects(domainTestPVC(), content, domainTestReadyVCR(true), domainTestVSC(), demoObj).
		Build()
	r := &GenericSnapshotBinderController{Client: cl, APIReader: cl, Scheme: scheme}

	requeue, treason, tmsg, err := r.ensureDomainContentLinks(ctx, demoObj, domainTestContent, "")
	if err != nil {
		t.Fatalf("ensureDomainContentLinks: %v", err)
	}
	if treason != "" {
		t.Fatalf("unexpected terminal reason %q (msg=%q)", treason, tmsg)
	}
	if requeue {
		t.Fatalf("handoff complete should not requeue")
	}

	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: domainTestContent}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.Data == nil {
		t.Fatalf("expected 1 published data binding, got none")
	}
	ref := *got.Status.Data
	if string(ref.Source.UID) != domainTestPVCUID || ref.Artifact.Name != domainTestVSCName {
		t.Fatalf("unexpected data binding: %#v", ref)
	}
	if ref.StorageClassName != "sc-a" || ref.VolumeMode != string(corev1.PersistentVolumeFilesystem) || len(ref.AccessModes) != 1 || ref.AccessModes[0] != string(corev1.ReadWriteOnce) {
		t.Fatalf("dataRef not enriched with volume metadata: %#v", ref)
	}

	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(volumeSnapshotContentGVK)
	if err := cl.Get(ctx, client.ObjectKey{Name: domainTestVSCName}, vsc); err != nil {
		t.Fatalf("get VSC: %v", err)
	}
	policy, _, _ := unstructured.NestedString(vsc.Object, "spec", "deletionPolicy")
	if policy != "Retain" {
		t.Fatalf("VSC deletionPolicy not forced to Retain, got %q", policy)
	}
	ownedByContent := false
	for _, o := range vsc.GetOwnerReferences() {
		if o.Kind == "SnapshotContent" && o.Name == domainTestContent && o.UID == types.UID(domainTestConUID) {
			ownedByContent = true
		}
	}
	if !ownedByContent {
		t.Fatalf("VSC not re-owned by content: %#v", vsc.GetOwnerReferences())
	}

	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(demoDiskSnapshotGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: domainTestNS, Name: domainTestSnap}, fresh); err != nil {
		t.Fatalf("get demo snapshot: %v", err)
	}
	if captured, _, _ := unstructured.NestedBool(fresh.Object, "status", "captureState", "commonController", "dataCaptured"); !captured {
		t.Fatalf("expected status.captureState.commonController.dataCaptured=true after durable handoff")
	}
	// Single-writer discipline: the binder owns commonController (the dataCaptured latch) but must NOT
	// clear the domain-owned domainSpecificController.volumeCaptureRequestName. Suppression of VCR
	// re-creation is driven by the latch, not by clearing the name; the domain owns that field.
	if name, _, _ := unstructured.NestedString(fresh.Object, "status", "captureState", "domainSpecificController", "volumeCaptureRequestName"); name != domainTestVCRName() {
		t.Fatalf("binder must not touch domain-owned volumeCaptureRequestName, got %q", name)
	}
	if domainTestVCRExists(t, cl) {
		t.Fatalf("expected the transient VCR to be deleted after durable handoff")
	}
}

// A not-yet-Ready data-leg VCR must NOT be handed off: the binder requeues and leaves the VCR, the marker,
// and the content dataRefs untouched (no premature deletion/marker that would mask an incomplete capture).
func TestEnsureDomainContentLinks_DataLegPendingRequeues(t *testing.T) {
	ctx := context.Background()
	scheme := domainTestScheme(t)
	demoObj := domainTestDemoSnapshotUnstructured(t, domainTestVCRName())

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&demov1alpha1.DemoVirtualDiskSnapshot{}, &storagev1alpha1.SnapshotContent{}).
		WithObjects(domainTestPVC(), domainTestSnapshotContent(), domainTestReadyVCR(false), demoObj).
		Build()
	r := &GenericSnapshotBinderController{Client: cl, APIReader: cl, Scheme: scheme}

	requeue, treason, _, err := r.ensureDomainContentLinks(ctx, demoObj, domainTestContent, "")
	if err != nil {
		t.Fatalf("ensureDomainContentLinks: %v", err)
	}
	if treason != "" {
		t.Fatalf("pending VCR must not be terminal, got reason %q", treason)
	}
	if !requeue {
		t.Fatalf("pending VCR should requeue")
	}

	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: domainTestContent}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.Data != nil {
		t.Fatalf("pending VCR must not publish data binding, got %#v", got.Status.Data)
	}
	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(demoDiskSnapshotGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: domainTestNS, Name: domainTestSnap}, fresh); err != nil {
		t.Fatalf("get demo snapshot: %v", err)
	}
	if captured, _, _ := unstructured.NestedBool(fresh.Object, "status", "captureState", "commonController", "dataCaptured"); captured {
		t.Fatalf("pending VCR must not set status.captureState.commonController.dataCaptured")
	}
	if !domainTestVCRExists(t, cl) {
		t.Fatalf("pending VCR must not be deleted")
	}
}

// A failed data-leg VCR surfaces an actionable terminal VolumeCaptureFailed condition (no marker, no
// deletion, no endless silent requeue).
func TestEnsureDomainContentLinks_DataLegFailedIsTerminal(t *testing.T) {
	ctx := context.Background()
	scheme := domainTestScheme(t)
	demoObj := domainTestDemoSnapshotUnstructured(t, domainTestVCRName())

	failedVCR := vcctrl.NewVolumeCaptureRequestObject(domainTestNS, domainTestVCRName(), metav1.OwnerReference{}, []vcpkg.Target{domainTestVCRTarget()})
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
		WithStatusSubresource(&demov1alpha1.DemoVirtualDiskSnapshot{}, &storagev1alpha1.SnapshotContent{}).
		WithObjects(domainTestPVC(), domainTestSnapshotContent(), failedVCR, demoObj).
		Build()
	r := &GenericSnapshotBinderController{Client: cl, APIReader: cl, Scheme: scheme}

	_, treason, _, err := r.ensureDomainContentLinks(ctx, demoObj, domainTestContent, "")
	if err != nil {
		t.Fatalf("ensureDomainContentLinks: %v", err)
	}
	if treason != snapshot.ReasonVolumeCaptureFailed {
		t.Fatalf("expected terminal VolumeCaptureFailed, got %q", treason)
	}
	if !domainTestVCRExists(t, cl) {
		t.Fatalf("failed VCR must not be deleted (operator needs to see it)")
	}
}

// Regression: once the manifest leg is captured (commonController.manifestCaptured=true), the binder has
// already published the checkpoint onto the content and intentionally deleted the MCR. The domain-owned
// manifestCaptureRequestName still points at the now-absent MCR, so a NotFound MCR lookup MUST NOT set
// requeue=true — otherwise ensureSnapshotContentLinks returns before the Step 5 Ready mirror on every
// reconcile and the (manifest-only) snapshot wedges at Ready=False/ContentMissing while its bound content
// is already Ready=True, cascading the parent into ChildrenPending forever.
func TestEnsureSnapshotContentLinks_ManifestCapturedMCRDeletedDoesNotRequeue(t *testing.T) {
	ctx := context.Background()
	scheme := domainTestScheme(t)

	// Manifest-only demo VM snapshot at phase>=Planned with the manifest leg already latched captured and
	// its MCR gone (deleted by the binder after the durable checkpoint handoff). No children, no data leg.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(demoVMSnapshotGVK)
	obj.SetNamespace(domainTestNS)
	obj.SetName("vm-snap")
	obj.SetUID("vm-snap-uid")
	if err := unstructured.SetNestedField(obj.Object, "Planned", "status", "captureState", "domainSpecificController", "phase"); err != nil {
		t.Fatalf("set phase: %v", err)
	}
	if err := unstructured.SetNestedField(obj.Object, "nss-mcr-gone", "status", "captureState", "domainSpecificController", "manifestCaptureRequestName"); err != nil {
		t.Fatalf("set mcr name: %v", err)
	}
	if err := unstructured.SetNestedField(obj.Object, true, "status", "captureState", "commonController", "manifestCaptured"); err != nil {
		t.Fatalf("set manifestCaptured: %v", err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&demov1alpha1.DemoVirtualMachineSnapshot{}, &storagev1alpha1.SnapshotContent{}).
		WithObjects(domainTestSnapshotContent(), obj).
		Build()
	r := &GenericSnapshotBinderController{Client: cl, APIReader: cl, Scheme: scheme}
	r.MarkDomainCaptureKind(demoVMSnapshotGVK)

	requeue, treason, tmsg, err := r.ensureSnapshotContentLinks(ctx, nil, obj, domainTestContent)
	if err != nil {
		t.Fatalf("ensureSnapshotContentLinks: %v", err)
	}
	if treason != "" {
		t.Fatalf("captured manifest leg must not be terminal, got reason %q (msg=%q)", treason, tmsg)
	}
	if requeue {
		t.Fatalf("captured manifest leg with a GC'd MCR must not requeue (would starve the Ready mirror)")
	}
}

// mirrorLeafDataFromContent copies the bound SnapshotContent's self-contained data binding verbatim onto
// the namespaced data leaf's top-level status.data (source + artifact + volume metadata) and writes NO
// flat top-level storageClassName/size/volumeMode mirrors (folded into status.data in wave5).
func TestMirrorLeafDataFromContent_WritesTopLevelStatusData(t *testing.T) {
	ctx := context.Background()
	scheme := domainTestScheme(t)
	demoObj := domainTestDemoSnapshotUnstructured(t, domainTestVCRName())
	content := domainTestSnapshotContent()
	content.Status.Data = &storagev1alpha1.SnapshotDataBinding{
		Source: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: domainTestPVCName,
			Namespace: domainTestNS, UID: types.UID(domainTestPVCUID),
		},
		Artifact: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent",
			Name: domainTestVSCName, UID: types.UID("vsc-uid-1"),
		},
		VolumeMode:       string(corev1.PersistentVolumeFilesystem),
		AccessModes:      []string{string(corev1.ReadWriteOnce)},
		StorageClassName: "sc-a",
		Size:             "10Gi",
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&demov1alpha1.DemoVirtualDiskSnapshot{}, &storagev1alpha1.SnapshotContent{}).
		WithObjects(content, demoObj).
		Build()
	r := &GenericSnapshotBinderController{Client: cl, APIReader: cl, Scheme: scheme}

	if err := r.mirrorLeafDataFromContent(ctx, demoObj, domainTestContent, ""); err != nil {
		t.Fatalf("mirrorLeafDataFromContent: %v", err)
	}

	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(demoDiskSnapshotGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: domainTestNS, Name: domainTestSnap}, fresh); err != nil {
		t.Fatalf("get demo snapshot: %v", err)
	}
	data, found, _ := unstructured.NestedMap(fresh.Object, "status", "data")
	if !found {
		t.Fatalf("expected status.data to be mirrored")
	}
	if srcUID, _, _ := unstructured.NestedString(data, "source", "uid"); srcUID != domainTestPVCUID {
		t.Fatalf("status.data.source.uid = %q, want %q", srcUID, domainTestPVCUID)
	}
	if artName, _, _ := unstructured.NestedString(data, "artifact", "name"); artName != domainTestVSCName {
		t.Fatalf("status.data.artifact.name = %q, want %q", artName, domainTestVSCName)
	}
	if sc, _, _ := unstructured.NestedString(data, "storageClassName"); sc != "sc-a" {
		t.Fatalf("status.data.storageClassName = %q, want sc-a", sc)
	}
	if size, _, _ := unstructured.NestedString(data, "size"); size != "10Gi" {
		t.Fatalf("status.data.size = %q, want 10Gi", size)
	}
	// The flat top-level mirrors must be gone (folded into status.data).
	if _, found, _ := unstructured.NestedString(fresh.Object, "status", "storageClassName"); found {
		t.Fatalf("flat status.storageClassName must not be written")
	}
	if _, found, _ := unstructured.NestedString(fresh.Object, "status", "volumeMode"); found {
		t.Fatalf("flat status.volumeMode must not be written")
	}
	if _, found, _ := unstructured.NestedString(fresh.Object, "status", "size"); found {
		t.Fatalf("flat status.size must not be written")
	}
}

// On import the content data carries no storageClassName; the caller passes it from
// DataImport.spec.storageClassName as scOverride, which must land in the mirrored status.data.
func TestMirrorLeafDataFromContent_ScOverride(t *testing.T) {
	ctx := context.Background()
	scheme := domainTestScheme(t)
	demoObj := domainTestDemoSnapshotUnstructured(t, domainTestVCRName())
	content := domainTestSnapshotContent()
	content.Status.Data = &storagev1alpha1.SnapshotDataBinding{
		Source:   storagev1alpha1.SnapshotSubjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: domainTestPVCName, Namespace: domainTestNS, UID: types.UID(domainTestPVCUID)},
		Artifact: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: domainTestVSCName},
		Size:     "5Gi",
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&demov1alpha1.DemoVirtualDiskSnapshot{}, &storagev1alpha1.SnapshotContent{}).
		WithObjects(content, demoObj).
		Build()
	r := &GenericSnapshotBinderController{Client: cl, APIReader: cl, Scheme: scheme}
	if err := r.mirrorLeafDataFromContent(ctx, demoObj, domainTestContent, "sc-import"); err != nil {
		t.Fatalf("mirrorLeafDataFromContent: %v", err)
	}
	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(demoDiskSnapshotGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: domainTestNS, Name: domainTestSnap}, fresh); err != nil {
		t.Fatalf("get demo snapshot: %v", err)
	}
	if sc, _, _ := unstructured.NestedString(fresh.Object, "status", "data", "storageClassName"); sc != "sc-import" {
		t.Fatalf("scOverride not applied: status.data.storageClassName = %q, want sc-import", sc)
	}
}

// SnapshotDataBindingToUnstructuredMap renders source/artifact always, omits empty optionals, and
// converts AccessModes to a JSON-typed []interface{} (required by unstructured.SetNestedMap).
func TestSnapshotDataBindingToMap(t *testing.T) {
	m := snapshotcontent.SnapshotDataBindingToUnstructuredMap(&storagev1alpha1.SnapshotDataBinding{
		Source:      storagev1alpha1.SnapshotSubjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc", UID: types.UID("u1")},
		Artifact:    storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc"},
		AccessModes: []string{"ReadWriteOnce"},
	})
	if _, ok := m["source"].(map[string]interface{}); !ok {
		t.Fatalf("source must be a map, got %#v", m["source"])
	}
	if _, ok := m["artifact"].(map[string]interface{}); !ok {
		t.Fatalf("artifact must be a map, got %#v", m["artifact"])
	}
	am, ok := m["accessModes"].([]interface{})
	if !ok || len(am) != 1 || am[0] != "ReadWriteOnce" {
		t.Fatalf("accessModes must be []interface{}{\"ReadWriteOnce\"}, got %#v", m["accessModes"])
	}
	// Empty optionals are omitted.
	if _, ok := m["storageClassName"]; ok {
		t.Fatalf("empty storageClassName must be omitted")
	}
	if _, ok := m["size"]; ok {
		t.Fatalf("empty size must be omitted")
	}
	// The Namespace on source was empty -> omitted.
	if src := m["source"].(map[string]interface{}); func() bool { _, ok := src["namespace"]; return ok }() {
		t.Fatalf("empty source.namespace must be omitted")
	}
}
