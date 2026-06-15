//go:build integration
// +build integration

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

package integration

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

const (
	exportImportReadyTimeout = 120 * time.Second
	exportImportPoll         = 300 * time.Millisecond
	// failClosedSettleWindow bounds the negative "no VRR ever created" assertion; it must exceed the
	// controller's requeueShort (5s) so at least one reconcile would have created a VRR if it intended to.
	failClosedSettleWindow = 6 * time.Second
)

var (
	vrrGVK            = schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "VolumeRestoreRequest"}
	vrrListGVK        = schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "VolumeRestoreRequestList"}
	dataExportGVK     = schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "DataExport"}
	dataExportListGVK = schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "DataExportList"}
)

// listUnstructuredInNamespace returns all objects of listGVK in ns.
func listUnstructuredInNamespace(ctx context.Context, listGVK schema.GroupVersionKind, ns string) []unstructured.Unstructured {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(listGVK)
	Expect(k8sClient.List(ctx, list, client.InNamespace(ns))).To(Succeed())
	return list.Items
}

// markUnstructuredReady sets status.conditions[Ready]=True (with reason) on an object that has a
// status subresource, simulating an external controller (VRR/DataExport/DataImport/VCR). It re-fetches
// under RetryOnConflict so it stays correct even if a controller ever also writes these CRs (today the
// export/import reconcilers only ever Get them, so the test is the sole status writer).
func markUnstructuredReady(ctx context.Context, obj *unstructured.Unstructured, reason string, extraStatus map[string]interface{}) {
	gvk := obj.GroupVersionKind()
	key := client.ObjectKeyFromObject(obj)
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &unstructured.Unstructured{}
		cur.SetGroupVersionKind(gvk)
		if err := k8sClient.Get(ctx, key, cur); err != nil {
			return err
		}
		cond := map[string]interface{}{
			"type":               "Ready",
			"status":             "True",
			"reason":             reason,
			"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
		}
		status, _, _ := unstructured.NestedMap(cur.Object, "status")
		if status == nil {
			status = map[string]interface{}{}
		}
		status["conditions"] = []interface{}{cond}
		for k, v := range extraStatus {
			status[k] = v
		}
		if err := unstructured.SetNestedMap(cur.Object, status, "status"); err != nil {
			return err
		}
		return k8sClient.Status().Update(ctx, cur)
	})).To(Succeed())
}

// newExportNamespace creates a throwaway namespace with deferred cleanup.
func newExportNamespace(ctx context.Context, prefix string) string {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	name := ns.Name
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}) })
	return name
}

// createReadySnapshotWithDataRef creates an empty Snapshot, waits for it to bind a Ready
// SnapshotContent, then patches that content's status.dataRefs with a single VolumeSnapshotContent
// data binding so the export resolver sees one data leaf. Returns the bound SnapshotContent name.
func createReadySnapshotWithDataRef(ctx context.Context, ns, snapName, vscName, volumeMode, storageClass string) string {
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: snapName, Namespace: ns},
		Spec:       storagev1alpha1.SnapshotSpec{},
	}
	Expect(k8sClient.Create(ctx, snap)).To(Succeed())

	var rootContentName string
	Eventually(func(g Gomega) {
		f := &storagev1alpha1.Snapshot{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: snapName}, f)).To(Succeed())
		rc := meta.FindStatusCondition(f.Status.Conditions, snapshot.ConditionReady)
		g.Expect(rc).NotTo(BeNil())
		g.Expect(rc.Status).To(Equal(metav1.ConditionTrue))
		rootContentName = f.Status.BoundSnapshotContentName
		g.Expect(rootContentName).NotTo(BeEmpty())
	}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())

	Eventually(func(g Gomega) {
		content := &storagev1alpha1.SnapshotContent{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: rootContentName}, content)).To(Succeed())
		base := content.DeepCopy()
		content.Status.DataRefs = []storagev1alpha1.SnapshotDataBinding{{
			TargetUID: "export-data-uid",
			Target: storagev1alpha1.SnapshotSubjectRef{
				APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "src-pvc", Namespace: ns,
			},
			Artifact: storagev1alpha1.SnapshotDataArtifactRef{
				APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: vscName,
			},
			VolumeMode:       volumeMode,
			AccessModes:      []string{"ReadWriteOnce"},
			StorageClassName: storageClass,
		}}
		g.Expect(k8sClient.Status().Patch(ctx, content, client.MergeFrom(base))).To(Succeed())
	}).WithTimeout(30 * time.Second).WithPolling(exportImportPoll).Should(Succeed())

	return rootContentName
}

var _ = Describe("Integration: SnapshotExport reconciler", func() {
	It("restores a data leaf to a PVC and publishes its DataExport endpoint", func() {
		ctx := context.Background()
		ns := newExportNamespace(ctx, "ss-export-")
		const snapName = "exp-snap"
		const vscName = "exp-src-vsc"

		contentName := createReadySnapshotWithDataRef(ctx, ns, snapName, vscName, "Block", "fast")

		// The export resolver fails closed on a non-Ready root. The injected dataRefs reference a
		// VolumeSnapshotContent (exp-src-vsc) that is never created; this stays Ready=True only because
		// the CSI VolumeSnapshotContent CRD is absent from envtest, so the SnapshotContent controller's
		// artifact-readiness Get errors (no-kind-match) and requeues without flipping Ready, rather than
		// resolving NotFound -> ArtifactMissing. Assert the invariant here so that if a CSI VSC CRD is
		// ever added to the envtest path this surfaces as a clear failure instead of a 120s export hang.
		Consistently(func(g Gomega) {
			content := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, content)).To(Succeed())
			g.Expect(meta.IsStatusConditionTrue(content.Status.Conditions, snapshot.ConditionReady)).To(BeTrue())
		}).WithTimeout(2 * time.Second).WithPolling(exportImportPoll).Should(Succeed())

		export := &storagev1alpha1.SnapshotExport{
			ObjectMeta: metav1.ObjectMeta{Name: "exp", Namespace: ns},
			Spec:       storagev1alpha1.SnapshotExportSpec{SnapshotRef: storagev1alpha1.LocalSnapshotRef{Name: snapName}},
		}
		Expect(k8sClient.Create(ctx, export)).To(Succeed())

		// 1. The controller creates exactly one VolumeRestoreRequest for the leaf. Simulate the
		//    external VRR controller: mark it Completed and create the restored PVC.
		var vrrName string
		Eventually(func(g Gomega) {
			items := listUnstructuredInNamespace(ctx, vrrListGVK, ns)
			g.Expect(items).To(HaveLen(1))
			vrrName = items[0].GetName()
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())

		vrr := &unstructured.Unstructured{}
		vrr.SetGroupVersionKind(vrrGVK)
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: vrrName}, vrr)).To(Succeed())
		markUnstructuredReady(ctx, vrr, "Completed", nil)

		restoredPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: vrrName, Namespace: ns},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, restoredPVC)).To(Succeed())

		// 2. Once the VRR is Completed and the PVC exists, the controller creates a DataExport.
		//    Simulate the external DataExport controller: mark it Ready and publish a URL.
		var deName string
		Eventually(func(g Gomega) {
			items := listUnstructuredInNamespace(ctx, dataExportListGVK, ns)
			g.Expect(items).To(HaveLen(1))
			deName = items[0].GetName()
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())

		de := &unstructured.Unstructured{}
		de.SetGroupVersionKind(dataExportGVK)
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: deName}, de)).To(Succeed())
		const dataURL = "https://data-pod.svc/api/v1/download"
		markUnstructuredReady(ctx, de, "Published", map[string]interface{}{"url": dataURL, "ca": "Zm9vYmFy"})

		// 3. The SnapshotExport becomes Ready with the published index/manifests/data endpoints.
		Eventually(func(g Gomega) {
			f := &storagev1alpha1.SnapshotExport{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "exp"}, f)).To(Succeed())
			ready := meta.FindStatusCondition(f.Status.Conditions, storagev1alpha1.SnapshotExportConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal(storagev1alpha1.SnapshotExportReasonPublished))
			g.Expect(f.Status.IndexURL).NotTo(BeEmpty())
			g.Expect(f.Status.ManifestsURL).NotTo(BeEmpty())
			g.Expect(f.Status.DataSnapshots).To(HaveLen(1))
			g.Expect(f.Status.DataSnapshots[0].DataURL).To(Equal(dataURL))
			g.Expect(f.Status.DataSnapshots[0].Ready).To(BeTrue())
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())

		// 4. The restored PVC carries the SnapshotExport as a non-controller hold owner (retention +
		//    GC anchor) so it survives the VRR TTL-scanner.
		Eventually(func(g Gomega) {
			pvc := &corev1.PersistentVolumeClaim{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: vrrName}, pvc)).To(Succeed())
			var held bool
			for _, o := range pvc.OwnerReferences {
				if o.Kind == "SnapshotExport" && o.Name == "exp" {
					held = true
					g.Expect(o.Controller == nil || !*o.Controller).To(BeTrue(), "hold owner must not be a controller ref")
				}
			}
			g.Expect(held).To(BeTrue())
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())
	})

	It("fails closed when a data leaf has no captured volumeMode", func() {
		ctx := context.Background()
		ns := newExportNamespace(ctx, "ss-export-novm-")
		const snapName = "exp-snap-novm"

		// Empty volumeMode: defaulting Block vs Filesystem risks a silent wrong-mode restore.
		_ = createReadySnapshotWithDataRef(ctx, ns, snapName, "exp-src-vsc-novm", "", "fast")

		export := &storagev1alpha1.SnapshotExport{
			ObjectMeta: metav1.ObjectMeta{Name: "exp-novm", Namespace: ns},
			Spec:       storagev1alpha1.SnapshotExportSpec{SnapshotRef: storagev1alpha1.LocalSnapshotRef{Name: snapName}},
		}
		Expect(k8sClient.Create(ctx, export)).To(Succeed())

		Eventually(func(g Gomega) {
			f := &storagev1alpha1.SnapshotExport{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "exp-novm"}, f)).To(Succeed())
			dr := meta.FindStatusCondition(f.Status.Conditions, storagev1alpha1.SnapshotExportConditionDataReady)
			g.Expect(dr).NotTo(BeNil())
			g.Expect(dr.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(dr.Reason).To(Equal(storagev1alpha1.SnapshotExportReasonDataExportFailed))
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())

		// No VolumeRestoreRequest must be created for a fail-closed leaf.
		Consistently(func(g Gomega) {
			g.Expect(listUnstructuredInNamespace(ctx, vrrListGVK, ns)).To(BeEmpty())
		}).WithTimeout(failClosedSettleWindow).WithPolling(exportImportPoll).Should(Succeed())
	})
})
