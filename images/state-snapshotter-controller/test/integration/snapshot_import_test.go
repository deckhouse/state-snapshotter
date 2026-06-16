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
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

var (
	dataImportGVK     = schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "DataImport"}
	dataImportListGVK = schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "DataImportList"}
	vcrGVK            = schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "VolumeCaptureRequest"}
	vcrListGVK        = schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "VolumeCaptureRequestList"}
)

type condSpec struct{ condType, status, reason string }

// setUnstructuredStatus replaces status.conditions with conds and merges extra status fields, then
// persists via the status subresource. Used to simulate the external DataImport/VCR controllers. It
// re-fetches under RetryOnConflict so it stays correct even if a controller ever also writes these CRs
// (today the import reconciler only ever Gets them, so the test is the sole status writer).
func setUnstructuredStatus(ctx context.Context, obj *unstructured.Unstructured, conds []condSpec, extra map[string]interface{}) {
	gvk := obj.GroupVersionKind()
	key := client.ObjectKeyFromObject(obj)
	condList := make([]interface{}, 0, len(conds))
	for _, c := range conds {
		condList = append(condList, map[string]interface{}{
			"type":               c.condType,
			"status":             c.status,
			"reason":             c.reason,
			"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
		})
	}
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &unstructured.Unstructured{}
		cur.SetGroupVersionKind(gvk)
		if err := k8sClient.Get(ctx, key, cur); err != nil {
			return err
		}
		status, _, _ := unstructured.NestedMap(cur.Object, "status")
		if status == nil {
			status = map[string]interface{}{}
		}
		status["conditions"] = condList
		for k, v := range extra {
			status[k] = v
		}
		if err := unstructured.SetNestedMap(cur.Object, status, "status"); err != nil {
			return err
		}
		return k8sClient.Status().Update(ctx, cur)
	})).To(Succeed())
}

// setImportCondition patches one condition on a SnapshotImport (simulates the upload handler flipping
// IndexReceived / ManifestsReceived).
func setImportCondition(ctx context.Context, ns, name, condType, status, reason string) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.SnapshotImport{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cur); err != nil {
			return err
		}
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:    condType,
			Status:  metav1.ConditionStatus(status),
			Reason:  reason,
			Message: "simulated by integration test",
		})
		return k8sClient.Status().Update(ctx, cur)
	})).To(Succeed())
}

var _ = Describe("Integration: SnapshotImport reconciler", func() {
	It("pre-provisions a snapshot tree from an uploaded index and cleans up the populated PVC", func() {
		ctx := context.Background()
		ns := newExportNamespace(ctx, "ss-import-")
		const impName = "imp"
		const snapName = "imported-snap"
		const capturedVSC = "imp-captured-vsc"
		// Namespace-suffixed so this cluster-scoped name never collides if the spec is duplicated.
		srcSC := "imp-src-sc-" + ns

		// Target StorageClass resolved by identity (no spec.storageClassMapping).
		sc := &storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: srcSC},
			Provisioner: "kubernetes.io/no-provisioner",
		}
		Expect(k8sClient.Create(ctx, sc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: srcSC}}) })

		// Single-node index with one Block data volume.
		rootID := "Snapshot--" + ns + "--orig"
		idx := restore.Index{
			Version: restore.IndexVersion,
			RootSnapshot: restore.IndexSnapshotID{
				ID: rootID, APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "Snapshot", Namespace: ns, Name: "orig",
			},
			Snapshots: []restore.IndexSnapshot{{
				ID: rootID, APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "Snapshot", Namespace: ns, Name: "orig",
				HasData: true,
				Data: &restore.IndexData{
					StorageClassName: srcSC,
					VolumeMode:       "Block",
					AccessModes:      []string{"ReadWriteOnce"},
					Size:             1 << 20,
					ArtifactName:     "orig-vsc",
				},
			}},
		}
		idxJSON, err := json.Marshal(&idx)
		Expect(err).NotTo(HaveOccurred())

		// Pre-seed the index and per-node manifests blobs through a direct (cache-bypassing) client,
		// exactly as the aggregated upload handler would have persisted them.
		blobLog, err := logger.NewLogger("error")
		Expect(err).NotTo(HaveOccurred())
		directClient, err := client.New(cfg, client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred())
		store := usecase.NewImportBlobStore(directClient, blobLog)
		_, err = store.Append(ctx, usecase.ImportBlobKey(ns, impName, usecase.ImportBlobKindIndex), 0, idxJSON)
		Expect(err).NotTo(HaveOccurred())
		manifests := []byte(`[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"imp-cm","namespace":"` + ns + `"},"data":{"k":"v"}}]`)
		_, err = store.Append(ctx, usecase.ImportManifestsBlobKey(ns, impName, rootID), 0, manifests)
		Expect(err).NotTo(HaveOccurred())

		imp := &storagev1alpha1.SnapshotImport{
			ObjectMeta: metav1.ObjectMeta{Name: impName, Namespace: ns},
			Spec:       storagev1alpha1.SnapshotImportSpec{TargetName: snapName, TTL: exportTTL},
		}
		Expect(k8sClient.Create(ctx, imp)).To(Succeed())

		// Cluster-scoped objects (pre-provisioned SnapshotContent + reconstructed ManifestCheckpoint and
		// its chunk) are not reclaimed by namespace deletion in envtest. Deleting the SnapshotImport also
		// exercises reconcileDelete's per-node upload-blob GC. Names are captured by the assertions below.
		var contentName, mcpName string
		DeferCleanup(func() {
			bg := context.Background()
			imp := &storagev1alpha1.SnapshotImport{}
			if err := k8sClient.Get(bg, client.ObjectKey{Namespace: ns, Name: impName}, imp); err == nil {
				_ = k8sClient.Delete(bg, imp)
				Eventually(func() bool {
					return apierrors.IsNotFound(k8sClient.Get(bg, client.ObjectKey{Namespace: ns, Name: impName}, &storagev1alpha1.SnapshotImport{}))
				}).WithTimeout(30 * time.Second).WithPolling(exportImportPoll).Should(BeTrue())
			}
			if contentName != "" {
				_ = k8sClient.Delete(bg, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: contentName}})
			}
			if mcpName != "" {
				_ = k8sClient.Delete(bg, &ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: mcpName}})
				_ = k8sClient.Delete(bg, &ssv1alpha1.ManifestCheckpointContentChunk{ObjectMeta: metav1.ObjectMeta{Name: mcpName + "-0"}})
			}
		})

		// Stage 0/1: the controller publishes the upload endpoints; simulate the upload handler having
		// received the index and manifests.
		Eventually(func(g Gomega) {
			f := &storagev1alpha1.SnapshotImport{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: impName}, f)).To(Succeed())
			g.Expect(f.Status.IndexUploadURL).NotTo(BeEmpty())
			g.Expect(f.Status.ManifestsUploadURL).NotTo(BeEmpty())
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())
		setImportCondition(ctx, ns, impName, storagev1alpha1.SnapshotImportConditionIndexReceived, "True", "Simulated")
		setImportCondition(ctx, ns, impName, storagev1alpha1.SnapshotImportConditionManifestsReceived, "True", "Simulated")

		// Stage 4: the controller creates one DataImport. Simulate SVDM: importer Ready + upload
		// finished + published URL, and create the populated PVC the importer would have provisioned.
		var diName string
		Eventually(func(g Gomega) {
			items := listUnstructuredInNamespace(ctx, dataImportListGVK, ns)
			g.Expect(items).To(HaveLen(1))
			diName = items[0].GetName()
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())

		di := &unstructured.Unstructured{}
		di.SetGroupVersionKind(dataImportGVK)
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: diName}, di)).To(Succeed())
		// spec.ttl must be propagated verbatim from the SnapshotImport into the child DataImport.
		gotTTL, _, _ := unstructured.NestedString(di.Object, "spec", "ttl")
		Expect(gotTTL).To(Equal(exportTTL))
		setUnstructuredStatus(ctx, di, []condSpec{
			{"Ready", "True", "Ready"},
			{"UploadFinished", "True", "Finished"},
		}, map[string]interface{}{"url": "https://importer.svc/api/v1/upload"})

		populatedPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: diName, Namespace: ns},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Mi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, populatedPVC)).To(Succeed())

		// Stage 6: once the PVC is populated the controller creates a VolumeCaptureRequest. Simulate
		// the capture controller: Ready + a published dataRefs entry naming the captured VSC.
		var vcrName string
		Eventually(func(g Gomega) {
			items := listUnstructuredInNamespace(ctx, vcrListGVK, ns)
			g.Expect(items).To(HaveLen(1))
			vcrName = items[0].GetName()
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())

		vcr := &unstructured.Unstructured{}
		vcr.SetGroupVersionKind(vcrGVK)
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: vcrName}, vcr)).To(Succeed())
		setUnstructuredStatus(ctx, vcr, []condSpec{{"Ready", "True", "Completed"}}, map[string]interface{}{
			"dataRefs": []interface{}{
				map[string]interface{}{
					"targetUID": "imp-target-uid",
					"target": map[string]interface{}{
						"apiVersion": "v1", "kind": "PersistentVolumeClaim", "name": diName, "namespace": ns, "uid": "imp-target-uid",
					},
					"artifact": map[string]interface{}{
						"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshotContent", "name": capturedVSC,
					},
				},
			},
		})

		// The import reaches Ready: the snapshot tree is pre-provisioned.
		Eventually(func(g Gomega) {
			f := &storagev1alpha1.SnapshotImport{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: impName}, f)).To(Succeed())
			ready := meta.FindStatusCondition(f.Status.Conditions, storagev1alpha1.SnapshotImportConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal(storagev1alpha1.SnapshotImportReasonImported))
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())

		// The recreated root Snapshot is statically bound to a pre-provisioned SnapshotContent that
		// carries the reconstructed manifest checkpoint and the captured VSC in its dataRefs. The
		// controller wrote the content status via a cache-bypassing client, so poll until the cached
		// reader catches up rather than reading once.
		Eventually(func(g Gomega) {
			recreated := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: snapName}, recreated)).To(Succeed())
			g.Expect(recreated.Spec.Source).NotTo(BeNil())
			contentName = recreated.Spec.Source.SnapshotContentName
			g.Expect(contentName).NotTo(BeEmpty())

			content := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, content)).To(Succeed())
			mcpName = content.Status.ManifestCheckpointName
			g.Expect(mcpName).NotTo(BeEmpty())
			g.Expect(content.Status.DataRefs).To(HaveLen(1))
			g.Expect(content.Status.DataRefs[0].Artifact.Name).To(Equal(capturedVSC))
			g.Expect(content.Status.DataRefs[0].VolumeMode).To(Equal("Block"))
			g.Expect(content.Spec.SnapshotRef).NotTo(BeNil())
			g.Expect(content.Spec.SnapshotRef.Name).To(Equal(snapName))
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())

		// Stage 8 cleanup: the populating DataImport and PVC are deleted once capture completes. In
		// envtest the StorageObjectInUseProtection admission stamps the pvc-protection finalizer and no
		// kube-controller-manager runs to clear it, so a deleted PVC lingers in Terminating rather than
		// vanishing; assert the controller issued the delete (DeletionTimestamp set) or it is already
		// gone. The DataImport is a finalizer-free CR, so it disappears outright.
		Eventually(func(g Gomega) {
			pvc := &corev1.PersistentVolumeClaim{}
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: diName}, pvc)
			if err == nil {
				g.Expect(pvc.DeletionTimestamp).NotTo(BeNil(), "populated PVC should be deleting after capture")
			} else {
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "populated PVC should be deleted after capture")
			}
			gone := &unstructured.Unstructured{}
			gone.SetGroupVersionKind(dataImportGVK)
			derr := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: diName}, gone)
			g.Expect(apierrors.IsNotFound(derr)).To(BeTrue(), "populating DataImport should be deleted after capture")
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())
	})

	It("latches Expired and frees children when a node's upload endpoint idles out before upload finishes", func() {
		ctx := context.Background()
		ns := newExportNamespace(ctx, "ss-import-exp-")
		const impName = "imp-expired"
		const snapName = "imported-snap-expired"
		srcSC := "imp-exp-sc-" + ns

		sc := &storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: srcSC},
			Provisioner: "kubernetes.io/no-provisioner",
		}
		Expect(k8sClient.Create(ctx, sc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: srcSC}}) })

		rootID := "Snapshot--" + ns + "--orig"
		idx := restore.Index{
			Version: restore.IndexVersion,
			RootSnapshot: restore.IndexSnapshotID{
				ID: rootID, APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "Snapshot", Namespace: ns, Name: "orig",
			},
			Snapshots: []restore.IndexSnapshot{{
				ID: rootID, APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "Snapshot", Namespace: ns, Name: "orig",
				HasData: true,
				Data: &restore.IndexData{
					StorageClassName: srcSC,
					VolumeMode:       "Block",
					AccessModes:      []string{"ReadWriteOnce"},
					Size:             1 << 20,
					ArtifactName:     "orig-vsc",
				},
			}},
		}
		idxJSON, err := json.Marshal(&idx)
		Expect(err).NotTo(HaveOccurred())

		blobLog, err := logger.NewLogger("error")
		Expect(err).NotTo(HaveOccurred())
		directClient, err := client.New(cfg, client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred())
		store := usecase.NewImportBlobStore(directClient, blobLog)
		_, err = store.Append(ctx, usecase.ImportBlobKey(ns, impName, usecase.ImportBlobKindIndex), 0, idxJSON)
		Expect(err).NotTo(HaveOccurred())

		imp := &storagev1alpha1.SnapshotImport{
			ObjectMeta: metav1.ObjectMeta{Name: impName, Namespace: ns},
			Spec:       storagev1alpha1.SnapshotImportSpec{TargetName: snapName, TTL: exportTTL},
		}
		Expect(k8sClient.Create(ctx, imp)).To(Succeed())

		Eventually(func(g Gomega) {
			f := &storagev1alpha1.SnapshotImport{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: impName}, f)).To(Succeed())
			g.Expect(f.Status.IndexUploadURL).NotTo(BeEmpty())
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())
		setImportCondition(ctx, ns, impName, storagev1alpha1.SnapshotImportConditionIndexReceived, "True", "Simulated")

		// The controller creates one DataImport; simulate SVDM having provisioned the populating PVC.
		var diName string
		Eventually(func(g Gomega) {
			items := listUnstructuredInNamespace(ctx, dataImportListGVK, ns)
			g.Expect(items).To(HaveLen(1))
			diName = items[0].GetName()
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())

		populatedPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: diName, Namespace: ns},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Mi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, populatedPVC)).To(Succeed())

		// Simulate the upload endpoint idling out BEFORE the upload finished: Ready=False/reason=Expired
		// and no UploadFinished condition. The data never landed, so the import must fail closed.
		di := &unstructured.Unstructured{}
		di.SetGroupVersionKind(dataImportGVK)
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: diName}, di)).To(Succeed())
		setUnstructuredStatus(ctx, di, []condSpec{{"Ready", "False", "Expired"}}, nil)

		// The import latches into terminal Ready=False/reason=Expired.
		Eventually(func(g Gomega) {
			f := &storagev1alpha1.SnapshotImport{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: impName}, f)).To(Succeed())
			ready := meta.FindStatusCondition(f.Status.Conditions, storagev1alpha1.SnapshotImportConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal(storagev1alpha1.SnapshotImportReasonExpired))
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())

		// free_heavy cleanup: the DataImport is deleted; the populated PVC is deleted (or Terminating in
		// envtest due to the pvc-protection finalizer).
		Eventually(func(g Gomega) {
			g.Expect(listUnstructuredInNamespace(ctx, dataImportListGVK, ns)).To(BeEmpty())
			pvc := &corev1.PersistentVolumeClaim{}
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: diName}, pvc)
			if apierrors.IsNotFound(err) {
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pvc.DeletionTimestamp).NotTo(BeNil(), "populated PVC must be deleted (or marked for deletion)")
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())

		// Latch: a tombstoned import must NOT resurrect the DataImport on subsequent reconciles.
		Consistently(func(g Gomega) {
			g.Expect(listUnstructuredInNamespace(ctx, dataImportListGVK, ns)).To(BeEmpty())
		}).WithTimeout(failClosedSettleWindow).WithPolling(exportImportPoll).Should(Succeed())
	})
})
