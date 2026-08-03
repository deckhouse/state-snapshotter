/*
Copyright 2025 Flant JSC

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

package tests

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// --- DataExport recovery: losing the resources of a running export (E2E_DATAEXPORT_RECOVERY) --------
//
// A live-PVC DataExport borrows the user's volume: it creates an export claim in the module namespace,
// moves the PV's claimRef onto it, and flips the reclaim policy to Retain until it gives the volume back.
// Everything here is about what happens when that borrowed state is disturbed from the outside, which is
// the one class of failure the unit tests cannot settle: they can prove the state machine, but not that
// the API server, the PV binder and the kubelet agree with it.
//
// The specs deliberately do NOT chase the windows that exist only inside a single reconcile pass (the
// export claim is created and the volume is moved in one pass, so "delete the claim before the rebind"
// is not externally reachable). Those checkpoints are covered deterministically by the controller's
// restart matrix on a fake client. What a cluster adds is the opposite: states that persist because
// something real is holding them — a pod that will not die, a claim that is not ours.
//
// Scenario IDs refer to the P0 fault-injection table in the design plan (R2, R4, R5, R7-R13).

const (
	// deRecPVC is the source Filesystem PVC the export borrows the volume from.
	deRecPVC = "de-rec-pvc"
	// deRecExport is the DataExport under test; deRecWriterPod binds the WaitForFirstConsumer PVC and is
	// then deleted, because a live-PVC export can only start once the RWO volume is free.
	deRecExport    = "export-de-rec"
	deRecWriterPod = "de-rec-writer"
	// deRecForeignExport is the second export, used by the provenance spec; its export claim name is
	// occupied by a stranger before the DataExport is created.
	deRecForeignExport = "export-de-rec-foreign"
	// deRecAttachExport is the third export, used by the detach spec; it needs an export of its own
	// because the first one ends in Failed once its volume has been given back.
	deRecAttachExport = "export-de-rec-attach"
	// The remaining scenarios each own an export too: every one of them ends with the export torn down,
	// deleted, refused or deliberately stranded, so none of them can share one.
	deRecReplacedExport = "export-de-rec-replaced"
	deRecDeleteExport   = "export-de-rec-delete"
	deRecConflictExport = "export-de-rec-conflict"
	deRecLegacyExport   = "export-de-rec-legacy"
	// deRecOtherExport never exists; its name is written onto a volume to make it look borrowed by
	// someone else.
	deRecOtherExport = "export-de-rec-other"
	// deRecTTL is an idle TTL comfortably longer than this spec, so nothing expires mid-run.
	deRecTTL = "60m"

	// Labels and annotations the module puts on the objects inspected here. They are mirrored from
	// storage-foundation api/v1alpha1/data_consts.go rather than imported: the suite deliberately keeps
	// no build dependency on the module it tests.
	sfAppLabel             = "app"
	sfExporterAppValue     = "data-exporter"
	sfDeployNameLabel      = "storage-foundation.deckhouse.io/storage-manager-deployment-name"
	sfDataExportUIDAnno    = "storage-foundation.deckhouse.io/data-export-uid"
	sfManagerNamespaceAnno = "storage-foundation.deckhouse.io/storage-manager-namespace"
	sfManagerNameAnno      = "storage-foundation.deckhouse.io/storage-manager-name"
	sfOriginalPVCNameAnno  = "storage-foundation.deckhouse.io/original-pvc-name"
	sfUserPVCUIDAnno       = "storage-foundation.deckhouse.io/original-pvc-uid"
	sfOriginalReclaimAnno  = "storage-foundation.deckhouse.io/original-reclaim-policy"
	// sfFinalizer is the finalizer the module keeps on a DataExport until its teardown has finished. It
	// is what makes a deletion wait for the volume to come back instead of racing it.
	sfFinalizer = "storage-foundation.deckhouse.io/storage-manager-controller"

	// sfControllerDeploy is the DataExport/DataImport controller Deployment in d8-storage-foundation.
	sfControllerDeploy = "data-manager-controller"
	sfControllerApp    = "data-manager-controller"

	// pvcProtectionFinalizer is the kubelet-side guard that keeps a claim alive while a pod references
	// it. Removing it by hand is what an operator does to unstick a terminating claim — and it is the
	// only way to reach the state barrier B1 exists for: the claim gone while its consumer is still up.
	pvcProtectionFinalizer = "kubernetes.io/pvc-protection"
	// deRecHoldFinalizer is this suite's own finalizer, used to keep the exporter pod from disappearing
	// so that the barrier is observable for as long as the assertions need it.
	//
	// The name deliberately stays outside deckhouse.io: the admission-policy-engine module ships a
	// ValidatingAdmissionPolicy (deny-deckhouse-finalizers.deckhouse.io) that lets anyone add a finalizer
	// containing "deckhouse.io" but refuses to let a non-system user remove one. A pin the suite cannot
	// release is worse than no pin at all — it strands the object it was holding.
	deRecHoldFinalizer = "e2e.state-snapshotter.test/recovery-hold"
)

// dataExportRecoverySpecs registers the recovery flow (env-gated by E2E_DATAEXPORT_RECOVERY, default on;
// it also needs the thin StorageClass the volume-data phases provision). Ordered: one export is built in
// (a) and then disturbed by (b) and (c); the provenance spec owns a separate export and namespace.
func dataExportRecoverySpecs() {
	Context("DataExport recovery 1: a running export loses its resources", Ordered, func() {
		var (
			srcNS string
			// pvName is the volume the export borrows; originalReclaim is what the policy must be again
			// once the volume is returned.
			pvName          string
			originalReclaim corev1.PersistentVolumeReclaimPolicy
			exportPVCName   string
			deployName      string
			deUID           string
		)

		deRecDiagnostics(func() string { return srcNS }, deRecExport)

		BeforeAll(func() {
			if !suiteCfg.dataExportRecovery {
				Skip("E2E_DATAEXPORT_RECOVERY=false: skipping the DataExport recovery flow (it runs by default)")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			srcNS = uniqueNS("de-rec")

			By("Ensuring a thin StorageClass (" + suiteCfg.storageClass + ")")
			Expect(ensureSnapshotStorageClass(ctx, suiteCfg.storageClass)).To(Succeed())

			By("Creating the source namespace " + srcNS)
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, srcNS)
			})

			By("Creating the source PVC " + deRecPVC + " and binding it with a short-lived pod")
			Expect(createFilesystemPVC(ctx, srcNS, deRecPVC, suiteCfg.storageClass, "1Gi")).To(Succeed())
			_, err := suiteClientset.CoreV1().Pods(srcNS).Create(ctx, probePodSpec(srcNS, deRecWriterPod, []string{deRecPVC}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create the binding pod")
			Expect(waitPodRunning(ctx, srcNS, deRecWriterPod, 10*time.Minute)).To(Succeed())

			By("Releasing the volume: a live-PVC export refuses to start while a pod holds the claim")
			deletePod(ctx, srcNS, deRecWriterPod)
			Expect(waitPodDeleted(ctx, srcNS, deRecWriterPod, 5*time.Minute)).To(Succeed())

			By("Recording the volume and its reclaim policy before the export borrows it")
			srcPVC, err := suiteClientset.CoreV1().PersistentVolumeClaims(srcNS).Get(ctx, deRecPVC, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			pvName = srcPVC.Spec.VolumeName
			Expect(pvName).NotTo(BeEmpty(), "source PVC must be bound before the export starts")
			pv, err := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			originalReclaim = pv.Spec.PersistentVolumeReclaimPolicy

			DeferCleanup(func() {
				if cleanupSkipped() {
					GinkgoWriter.Printf("%s: keeping DataExport %s/%s\n", keepReason(), srcNS, deRecExport)
					return
				}
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteDataExport(cctx, srcNS, deRecExport)
			})
		})

		// R11: the successful export itself, plus the marker the later provenance checks depend on. The
		// generated name is computed locally and cross-checked here, so the copy of the naming rule in
		// this file cannot drift away from the module unnoticed.
		It("(a) borrows the volume and stamps the export claim with the origin of this DataExport", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.dataTransferTO+5*time.Minute)
			defer cancel()

			By("Creating the DataExport for the live PVC")
			Expect(createDataExportPVCTarget(ctx, srcNS, deRecExport, deRecPVC, deRecTTL)).To(Succeed())
			_, _, err := waitDataExportReady(ctx, srcNS, deRecExport, suiteCfg.dataTransferTO)
			Expect(err).NotTo(HaveOccurred(), "DataExport did not become Ready")

			de, err := getResource(ctx, dataExportGVR, srcNS, deRecExport)
			Expect(err).NotTo(HaveOccurred())
			deUID = string(de.GetUID())
			Expect(deUID).NotTo(BeEmpty())

			exportPVCName = generatedExportName("pvc-for", srcNS, deRecExport)
			deployName = generatedExportName("deploy-for", srcNS, deRecExport)

			By("Checking the export claim exists under the generated name and says who created it")
			claim, err := suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Get(ctx, exportPVCName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "export claim %s/%s not found; the generated-name rule mirrored in this file may have drifted from the module", d8DataManagerNS, exportPVCName)
			Expect(claim.Annotations[sfDataExportUIDAnno]).To(Equal(deUID), "export claim carries no provenance marker of this DataExport")
			Expect(claim.Annotations[sfManagerNamespaceAnno]).To(Equal(srcNS))
			Expect(claim.Annotations[sfManagerNameAnno]).To(Equal(deRecExport))
			Expect(claim.Labels[sfAppLabel]).To(Equal(sfExporterAppValue))

			By("Checking the volume was borrowed, not consumed: Retain + the way back recorded on the PV")
			pv, err := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(pv.Spec.ClaimRef).NotTo(BeNil())
			Expect(pv.Spec.ClaimRef.Namespace).To(Equal(d8DataManagerNS))
			Expect(pv.Spec.ClaimRef.Name).To(Equal(exportPVCName))
			Expect(pv.Spec.ClaimRef.UID).To(Equal(claim.UID))
			Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain))
			Expect(pv.Annotations[sfOriginalReclaimAnno]).To(Equal(string(originalReclaim)))
			Expect(pv.Annotations[sfOriginalPVCNameAnno]).To(Equal(deRecPVC))
			Expect(pv.Annotations[sfDataExportUIDAnno]).To(Equal(deUID))

			By("Checking the takeover identity is persisted on the DataExport")
			recPVName, _, _ := unstructured.NestedString(de.Object, "status", "recovery", "pvName")
			recExportUID, _, _ := unstructured.NestedString(de.Object, "status", "recovery", "exportPVCUID")
			Expect(recPVName).To(Equal(pvName))
			Expect(recExportUID).To(Equal(string(claim.UID)))
		})

		// R10: drift repair must rebuild the serving side without touching the borrowed volume. Taking it
		// over twice would overwrite the recorded identity, which is the only way back.
		It("(b) recreates a deleted exporter Deployment without borrowing the volume a second time", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.dataTransferTO+5*time.Minute)
			defer cancel()

			before, err := getResource(ctx, dataExportGVR, srcNS, deRecExport)
			Expect(err).NotTo(HaveOccurred())
			recoveryBefore, _, _ := unstructured.NestedMap(before.Object, "status", "recovery")
			pvBefore, err := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			claimUIDBefore := pvBefore.Spec.ClaimRef.UID

			By("Deleting the exporter Deployment " + deployName)
			Expect(suiteClientset.AppsV1().Deployments(d8DataManagerNS).Delete(ctx, deployName, metav1.DeleteOptions{})).To(Succeed())

			By("Waiting for the controller to rebuild it and the export to serve again")
			Eventually(func(g Gomega) {
				deploy, gerr := suiteClientset.AppsV1().Deployments(d8DataManagerNS).Get(ctx, deployName, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred())
				g.Expect(deploy.Status.AvailableReplicas).To(BeNumerically(">=", 1))
				obj, gerr := getResource(ctx, dataExportGVR, srcNS, deRecExport)
				g.Expect(gerr).NotTo(HaveOccurred())
				st, _, found := conditionStatus(obj, "Ready")
				g.Expect(found).To(BeTrue())
				g.Expect(st).To(Equal("True"))
			}).WithContext(ctx).WithTimeout(suiteCfg.dataTransferTO).WithPolling(pollInterval).Should(Succeed())

			By("Checking the volume never changed hands")
			pvAfter, err := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(pvAfter.Spec.ClaimRef).NotTo(BeNil())
			Expect(pvAfter.Spec.ClaimRef.UID).To(Equal(claimUIDBefore), "the export took the volume over a second time")
			after, err := getResource(ctx, dataExportGVR, srcNS, deRecExport)
			Expect(err).NotTo(HaveOccurred())
			recoveryAfter, _, _ := unstructured.NestedMap(after.Object, "status", "recovery")
			Expect(recoveryAfter).To(Equal(recoveryBefore), "the recorded takeover identity was rewritten")
		})

		// R2 + R4 + the CRD pruning check + a restart in a state that actually persists. The export claim
		// is removed out from under a live exporter pod (the pod is pinned by a finalizer first, so the
		// claim can be forced away while its consumer is still up). That is exactly the situation barrier
		// B1 exists for, and it holds still for as long as the pod does.
		It("(c) refuses to return the volume while a pod still holds the claim, survives a controller restart, and finishes once the pod is gone", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			By("Pinning the exporter pod so the barrier stays observable")
			podName := exporterPodName(ctx, deployName)
			Expect(addPodFinalizer(ctx, d8DataManagerNS, podName, deRecHoldFinalizer)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				// Best effort: the spec removes it itself on the happy path, but a failure in between
				// would otherwise leave a pod that no namespace deletion can reap.
				_ = removePodFinalizer(cctx, d8DataManagerNS, podName, deRecHoldFinalizer)
			})

			By("Deleting the export claim out from under the running exporter")
			Expect(suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Delete(ctx, exportPVCName, metav1.DeleteOptions{})).To(Succeed())
			// The claim only terminates; pvc-protection keeps it while the pod references it. Dropping that
			// finalizer is what makes the loss real while the consumer is still there.
			Expect(dropPVCProtection(ctx, d8DataManagerNS, exportPVCName)).To(Succeed())
			Eventually(func() bool {
				_, err := suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Get(ctx, exportPVCName, metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}).WithContext(ctx).WithTimeout(5*time.Minute).WithPolling(pollInterval).Should(BeTrue(), "export claim did not go away")

			By("Waiting for the controller to report the barrier, naming the pod that blocks it")
			Eventually(func(g Gomega) {
				obj, gerr := getResource(ctx, dataExportGVR, srcNS, deRecExport)
				g.Expect(gerr).NotTo(HaveOccurred())
				st, reason, found := conditionStatus(obj, "Ready")
				g.Expect(found).To(BeTrue())
				g.Expect(st).To(Equal("False"))
				g.Expect(reason).To(Equal("CleanupBlocked"))
				msg := conditionMessage(obj, "Ready")
				g.Expect(msg).To(ContainSubstring("B1"))
				g.Expect(msg).To(ContainSubstring(podName))
			}).WithContext(ctx).WithTimeout(10 * time.Minute).WithPolling(pollInterval).Should(Succeed())

			// The CRD-first check from §6 of the plan: the discriminator must survive a round trip through
			// the API server. An old schema prunes it silently, and the return of the volume would then
			// never resume. A blocked recovery is the only state where it stays set long enough to observe.
			By("Checking status.cleanupReason is stored, not pruned by the CRD schema")
			Consistently(func(g Gomega) {
				obj, gerr := getResource(ctx, dataExportGVR, srcNS, deRecExport)
				g.Expect(gerr).NotTo(HaveOccurred())
				cleanupReason, _, _ := unstructured.NestedString(obj.Object, "status", "cleanupReason")
				g.Expect(cleanupReason).To(Equal("ExportPVCPostRebindLost"))
			}).WithContext(ctx).WithTimeout(20 * time.Second).WithPolling(5 * time.Second).Should(Succeed())

			By("Restarting the controller while the recovery is blocked")
			Expect(restartDataManagerController(ctx)).To(Succeed())

			By("Checking the restarted controller neither forgets the barrier nor acts around it")
			Consistently(func(g Gomega) {
				pv, gerr := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred())
				// Nothing may move while B1 holds: not the binding, not the policy that protects the data.
				g.Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain))
				g.Expect(pv.Spec.ClaimRef).NotTo(BeNil())
				g.Expect(pv.Spec.ClaimRef.Name).To(Equal(exportPVCName))
				obj, gerr := getResource(ctx, dataExportGVR, srcNS, deRecExport)
				g.Expect(gerr).NotTo(HaveOccurred())
				_, reason, _ := conditionStatus(obj, "Ready")
				g.Expect(reason).To(Equal("CleanupBlocked"))
			}).WithContext(ctx).WithTimeout(90 * time.Second).WithPolling(10 * time.Second).Should(Succeed())

			By("Releasing the pod and letting the return finish")
			Expect(removePodFinalizer(ctx, d8DataManagerNS, podName, deRecHoldFinalizer)).To(Succeed())

			Eventually(func(g Gomega) {
				pv, gerr := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred())
				g.Expect(pv.Spec.ClaimRef).NotTo(BeNil())
				g.Expect(pv.Spec.ClaimRef.Namespace).To(Equal(srcNS))
				g.Expect(pv.Spec.ClaimRef.Name).To(Equal(deRecPVC))
				g.Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(originalReclaim), "the reclaim policy was not restored")
				g.Expect(pv.Annotations).NotTo(HaveKey(sfDataExportUIDAnno), "export metadata left on the volume")
				g.Expect(pv.Annotations).NotTo(HaveKey(sfOriginalReclaimAnno))

				claim, gerr := suiteClientset.CoreV1().PersistentVolumeClaims(srcNS).Get(ctx, deRecPVC, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred())
				g.Expect(claim.Status.Phase).To(Equal(corev1.ClaimBound), "the user's claim did not get its volume back")

				obj, gerr := getResource(ctx, dataExportGVR, srcNS, deRecExport)
				g.Expect(gerr).NotTo(HaveOccurred())
				st, reason, found := conditionStatus(obj, "Ready")
				g.Expect(found).To(BeTrue())
				g.Expect(st).To(Equal("False"))
				g.Expect(reason).To(Equal("ManagedResourceLost"))
				cleanupReason, _, _ := unstructured.NestedString(obj.Object, "status", "cleanupReason")
				g.Expect(cleanupReason).To(BeEmpty(), "the discriminator was not cleared after the volume came back")
				phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
				g.Expect(phase).To(Equal("Failed"))
			}).WithContext(ctx).WithTimeout(15 * time.Minute).WithPolling(pollInterval).Should(Succeed())

			By("Checking the serving infrastructure of this export is gone")
			Eventually(func(g Gomega) {
				_, gerr := suiteClientset.AppsV1().Deployments(d8DataManagerNS).Get(ctx, deployName, metav1.GetOptions{})
				g.Expect(apierrors.IsNotFound(gerr)).To(BeTrue(), "exporter Deployment still exists")
			}).WithContext(ctx).WithTimeout(5 * time.Minute).WithPolling(pollInterval).Should(Succeed())
		})
	})

	// R13: the export claim name is derived from the DataExport's own namespace and name, so a recreated
	// object — or anyone at all — can occupy it. Using such a claim would write a stranger's UID into the
	// takeover identity, where it stops looking like an assumption. The export must stop instead, and
	// leave the object it did not create exactly as it found it.
	Context("DataExport recovery 2: a claim the export did not create", Ordered, func() {
		var (
			srcNS         string
			exportPVCName string
			foreignUID    string
		)

		deRecDiagnostics(func() string { return srcNS }, deRecForeignExport)

		BeforeAll(func() {
			if !suiteCfg.dataExportRecovery {
				Skip("E2E_DATAEXPORT_RECOVERY=false: skipping the DataExport recovery flow (it runs by default)")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			srcNS = uniqueNS("de-rec-fgn")
			Expect(ensureSnapshotStorageClass(ctx, suiteCfg.storageClass)).To(Succeed())
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, srcNS)
			})

			By("Creating the source PVC and freeing its volume")
			Expect(createFilesystemPVC(ctx, srcNS, deRecPVC, suiteCfg.storageClass, "1Gi")).To(Succeed())
			_, err := suiteClientset.CoreV1().Pods(srcNS).Create(ctx, probePodSpec(srcNS, deRecWriterPod, []string{deRecPVC}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(waitPodRunning(ctx, srcNS, deRecWriterPod, 10*time.Minute)).To(Succeed())
			deletePod(ctx, srcNS, deRecWriterPod)
			Expect(waitPodDeleted(ctx, srcNS, deRecWriterPod, 5*time.Minute)).To(Succeed())
		})

		It("(d) refuses to use a claim occupying the export claim name, and leaves it untouched", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()

			exportPVCName = generatedExportName("pvc-for", srcNS, deRecForeignExport)

			By("Occupying the export claim name with a claim of our own, before the export exists")
			Expect(createFilesystemPVC(ctx, d8DataManagerNS, exportPVCName, suiteCfg.storageClass, "1Gi")).To(Succeed())
			foreign, err := suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Get(ctx, exportPVCName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			foreignUID = string(foreign.UID)
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				_ = suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Delete(cctx, exportPVCName, metav1.DeleteOptions{})
			})

			srcPVC, err := suiteClientset.CoreV1().PersistentVolumeClaims(srcNS).Get(ctx, deRecPVC, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			pvName := srcPVC.Spec.VolumeName
			Expect(pvName).NotTo(BeEmpty())
			pvBefore, err := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Creating the DataExport whose generated claim name is already taken")
			Expect(createDataExportPVCTarget(ctx, srcNS, deRecForeignExport, deRecPVC, deRecTTL)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteDataExport(cctx, srcNS, deRecForeignExport)
			})

			By("Waiting for the export to say it cannot prove the claim is its own")
			Eventually(func(g Gomega) {
				obj, gerr := getResource(ctx, dataExportGVR, srcNS, deRecForeignExport)
				g.Expect(gerr).NotTo(HaveOccurred())
				st, reason, found := conditionStatus(obj, "Ready")
				g.Expect(found).To(BeTrue())
				g.Expect(st).To(Equal("False"))
				g.Expect(reason).To(Equal("CleanupBlocked"))
				g.Expect(conditionMessage(obj, "Ready")).To(ContainSubstring(exportPVCName))
			}).WithContext(ctx).WithTimeout(10 * time.Minute).WithPolling(pollInterval).Should(Succeed())

			By("Checking nothing was done to the stranger's claim, the volume, or the takeover identity")
			Consistently(func(g Gomega) {
				foreignNow, gerr := suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Get(ctx, exportPVCName, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred(), "the claim this export did not create was deleted")
				g.Expect(string(foreignNow.UID)).To(Equal(foreignUID))
				g.Expect(foreignNow.DeletionTimestamp).To(BeNil(), "the claim this export did not create was marked for deletion")
				g.Expect(foreignNow.Annotations).NotTo(HaveKey(sfDataExportUIDAnno), "the export stamped its marker on a claim it found by name")

				pvNow, gerr := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred())
				g.Expect(pvNow.Spec.PersistentVolumeReclaimPolicy).To(Equal(pvBefore.Spec.PersistentVolumeReclaimPolicy))
				g.Expect(pvNow.Spec.ClaimRef).NotTo(BeNil())
				g.Expect(pvNow.Spec.ClaimRef.Name).To(Equal(deRecPVC), "the user's volume was taken over anyway")
				g.Expect(pvNow.Annotations).NotTo(HaveKey(sfDataExportUIDAnno))

				obj, gerr := getResource(ctx, dataExportGVR, srcNS, deRecForeignExport)
				g.Expect(gerr).NotTo(HaveOccurred())
				_, hasRecovery, _ := unstructured.NestedMap(obj.Object, "status", "recovery")
				g.Expect(hasRecovery).To(BeFalse(), "a takeover that never happened was recorded")
				cleanupReason, _, _ := unstructured.NestedString(obj.Object, "status", "cleanupReason")
				g.Expect(cleanupReason).To(BeEmpty(), "a teardown discriminator was set for a claim the export must not touch")
			}).WithContext(ctx).WithTimeout(60 * time.Second).WithPolling(10 * time.Second).Should(Succeed())
		})
	})

	// R5: the pod being gone is not the same as the volume being detached. Between the two there is a
	// window in which the node still has the device, and rebinding the volume to the user's claim in that
	// window hands out a volume the old node may still write to. B2 exists for that window; here it is
	// held open on purpose by keeping the VolumeAttachment object alive.
	Context("DataExport recovery 3: the volume is still attached", Ordered, func() {
		var (
			srcNS           string
			pvName          string
			originalReclaim corev1.PersistentVolumeReclaimPolicy
			exportPVCName   string
		)

		deRecDiagnostics(func() string { return srcNS }, deRecAttachExport)

		BeforeAll(func() {
			if !suiteCfg.dataExportRecovery {
				Skip("E2E_DATAEXPORT_RECOVERY=false: skipping the DataExport recovery flow (it runs by default)")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			srcNS = uniqueNS("de-rec-att")
			Expect(ensureSnapshotStorageClass(ctx, suiteCfg.storageClass)).To(Succeed())
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, srcNS)
			})

			By("Creating the source PVC and freeing its volume")
			Expect(createFilesystemPVC(ctx, srcNS, deRecPVC, suiteCfg.storageClass, "1Gi")).To(Succeed())
			_, err := suiteClientset.CoreV1().Pods(srcNS).Create(ctx, probePodSpec(srcNS, deRecWriterPod, []string{deRecPVC}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(waitPodRunning(ctx, srcNS, deRecWriterPod, 10*time.Minute)).To(Succeed())
			deletePod(ctx, srcNS, deRecWriterPod)
			Expect(waitPodDeleted(ctx, srcNS, deRecWriterPod, 5*time.Minute)).To(Succeed())

			srcPVC, err := suiteClientset.CoreV1().PersistentVolumeClaims(srcNS).Get(ctx, deRecPVC, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			pvName = srcPVC.Spec.VolumeName
			Expect(pvName).NotTo(BeEmpty())
			pv, err := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			originalReclaim = pv.Spec.PersistentVolumeReclaimPolicy

			By("Creating the DataExport and waiting until it serves the volume")
			Expect(createDataExportPVCTarget(ctx, srcNS, deRecAttachExport, deRecPVC, deRecTTL)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteDataExport(cctx, srcNS, deRecAttachExport)
			})
			_, _, err = waitDataExportReady(ctx, srcNS, deRecAttachExport, suiteCfg.dataTransferTO)
			Expect(err).NotTo(HaveOccurred(), "DataExport did not become Ready")
			exportPVCName = generatedExportName("pvc-for", srcNS, deRecAttachExport)
		})

		It("(e) refuses to return the volume while a VolumeAttachment for it is still live", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			By("Finding the VolumeAttachment of the exported volume")
			attachmentName := volumeAttachmentForPV(ctx, pvName, 3*time.Minute)
			if attachmentName == "" {
				// Local volumes are mounted without an attach step, so no such object is ever created and
				// this barrier cannot be reached from outside. The barrier itself is covered by the
				// controller's unit tests; skipping here says the storage cannot show it, not that it is
				// unimportant.
				Skip("no VolumeAttachment for " + pvName + ": this CSI driver does not require attach, so B2 is not reachable on this storage")
			}

			By("Pinning the VolumeAttachment " + attachmentName + " so the detach cannot complete")
			Expect(setVolumeAttachmentFinalizer(ctx, attachmentName, deRecHoldFinalizer, true)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				// A VolumeAttachment left with our finalizer would wedge the node's view of this volume
				// long after the suite is gone.
				_ = setVolumeAttachmentFinalizer(cctx, attachmentName, deRecHoldFinalizer, false)
			})

			By("Deleting the export claim so the recovery starts")
			Expect(suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Delete(ctx, exportPVCName, metav1.DeleteOptions{})).To(Succeed())
			Expect(dropPVCProtection(ctx, d8DataManagerNS, exportPVCName)).To(Succeed())
			Eventually(func() bool {
				_, err := suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Get(ctx, exportPVCName, metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}).WithContext(ctx).WithTimeout(5*time.Minute).WithPolling(pollInterval).Should(BeTrue(), "export claim did not go away")

			By("Waiting until the exporter pod is gone and the attachment is what blocks the recovery")
			Eventually(func(g Gomega) {
				obj, gerr := getResource(ctx, dataExportGVR, srcNS, deRecAttachExport)
				g.Expect(gerr).NotTo(HaveOccurred())
				st, reason, found := conditionStatus(obj, "Ready")
				g.Expect(found).To(BeTrue())
				g.Expect(st).To(Equal("False"))
				g.Expect(reason).To(Equal("CleanupBlocked"))
				msg := conditionMessage(obj, "Ready")
				g.Expect(msg).To(ContainSubstring("B2"), "expected the attachment barrier, got: %s", msg)
				g.Expect(msg).To(ContainSubstring(attachmentName))
			}).WithContext(ctx).WithTimeout(15 * time.Minute).WithPolling(pollInterval).Should(Succeed())

			By("Checking the rebind has not started while the volume may still be in use on the node")
			Consistently(func(g Gomega) {
				pv, gerr := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred())
				g.Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain))
				g.Expect(pv.Spec.ClaimRef).NotTo(BeNil())
				g.Expect(pv.Spec.ClaimRef.Name).To(Equal(exportPVCName), "the volume was rebound with the attachment still live")

				claim, gerr := suiteClientset.CoreV1().PersistentVolumeClaims(srcNS).Get(ctx, deRecPVC, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred())
				g.Expect(claim.Status.Phase).NotTo(Equal(corev1.ClaimBound))
			}).WithContext(ctx).WithTimeout(60 * time.Second).WithPolling(10 * time.Second).Should(Succeed())

			By("Letting the detach complete")
			Expect(setVolumeAttachmentFinalizer(ctx, attachmentName, deRecHoldFinalizer, false)).To(Succeed())

			Eventually(func(g Gomega) {
				pv, gerr := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred())
				g.Expect(pv.Spec.ClaimRef).NotTo(BeNil())
				g.Expect(pv.Spec.ClaimRef.Namespace).To(Equal(srcNS))
				g.Expect(pv.Spec.ClaimRef.Name).To(Equal(deRecPVC))
				g.Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(originalReclaim))

				claim, gerr := suiteClientset.CoreV1().PersistentVolumeClaims(srcNS).Get(ctx, deRecPVC, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred())
				g.Expect(claim.Status.Phase).To(Equal(corev1.ClaimBound))

				obj, gerr := getResource(ctx, dataExportGVR, srcNS, deRecAttachExport)
				g.Expect(gerr).NotTo(HaveOccurred())
				_, reason, _ := conditionStatus(obj, "Ready")
				g.Expect(reason).To(Equal("ManagedResourceLost"))
				cleanupReason, _, _ := unstructured.NestedString(obj.Object, "status", "cleanupReason")
				g.Expect(cleanupReason).To(BeEmpty())
			}).WithContext(ctx).WithTimeout(15 * time.Minute).WithPolling(pollInterval).Should(Succeed())
		})
	})

	// R7: the claim that occupies the export's name is not necessarily the claim it borrowed the volume
	// for. Only the recorded UID tells the two apart, and everything downstream depends on the controller
	// believing the record over the name: it must diagnose the replacement, return the volume to the user
	// on the strength of the record, and leave the namesake alone — deleting it would destroy an object
	// this export never created.
	Context("DataExport recovery 4: the export claim is replaced by a namesake", Ordered, func() {
		var f *deRecExportFixture

		deRecFixtureDiagnostics(&f, deRecReplacedExport)

		BeforeAll(func() {
			if !suiteCfg.dataExportRecovery {
				Skip("E2E_DATAEXPORT_RECOVERY=false: skipping the DataExport recovery flow (it runs by default)")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			f = deRecFreeVolume(ctx, "de-rec-repl")
			f.startExport(ctx, deRecReplacedExport)
		})

		It("(f) reports the replacement, returns the volume anyway, and does not touch the namesake", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			recordedUID := recordedExportPVCUID(ctx, f.ns, f.exportName)
			Expect(recordedUID).NotTo(BeEmpty())

			// "Deleted and recreated under the same name" is not reachable while the controller is
			// watching: it would see the absence first and settle on ManagedResourceLost, which is spec
			// (c)'s scenario, not this one. Pausing it makes the replacement a single observed step — the
			// state a controller that died between the two writes comes back to.
			By("Pausing the controller so the replacement is observed as one step")
			replicas, err := pauseDataManagerController(ctx)
			Expect(err).NotTo(HaveOccurred())
			resumed := false
			DeferCleanup(func() {
				if resumed {
					return
				}
				cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer ccancel()
				_ = resumeDataManagerController(cctx, replicas)
			})

			By("Deleting the export claim and putting a claim of our own under its name")
			Expect(suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Delete(ctx, f.exportPVCName, metav1.DeleteOptions{})).To(Succeed())
			Expect(dropPVCProtection(ctx, d8DataManagerNS, f.exportPVCName)).To(Succeed())
			Eventually(func() bool {
				_, gerr := suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Get(ctx, f.exportPVCName, metav1.GetOptions{})
				return apierrors.IsNotFound(gerr)
			}).WithContext(ctx).WithTimeout(5*time.Minute).WithPolling(pollInterval).Should(BeTrue(), "export claim did not go away")

			Expect(createFilesystemPVC(ctx, d8DataManagerNS, f.exportPVCName, suiteCfg.storageClass, "1Gi")).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				_ = suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Delete(cctx, f.exportPVCName, metav1.DeleteOptions{})
			})
			namesake, err := suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Get(ctx, f.exportPVCName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			namesakeUID := string(namesake.UID)
			Expect(namesakeUID).NotTo(Equal(recordedUID), "the replacement must be a different object")

			// The distinction the whole scenario rests on: the volume is still bound to the claim that is
			// gone, so the namesake holds nothing and may be passed over rather than deleted.
			By("Checking the volume still records the claim that borrowed it, not the namesake")
			pv, err := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, f.pvName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(pv.Spec.ClaimRef).NotTo(BeNil())
			Expect(string(pv.Spec.ClaimRef.UID)).To(Equal(recordedUID))

			By("Resuming the controller")
			Expect(resumeDataManagerController(ctx, replicas)).To(Succeed())
			resumed = true

			By("Waiting for the export to name the replacement and give the volume back regardless")
			Eventually(func(g Gomega) {
				obj, gerr := getResource(ctx, dataExportGVR, f.ns, f.exportName)
				g.Expect(gerr).NotTo(HaveOccurred())
				st, reason, found := conditionStatus(obj, "Ready")
				g.Expect(found).To(BeTrue())
				g.Expect(st).To(Equal("False"))
				g.Expect(reason).To(Equal("ManagedResourceIdentityMismatch"), "message was: %s", conditionMessage(obj, "Ready"))
				cleanupReason, _, _ := unstructured.NestedString(obj.Object, "status", "cleanupReason")
				g.Expect(cleanupReason).To(BeEmpty(), "the discriminator was not cleared after the volume came back")
				phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
				g.Expect(phase).To(Equal("Failed"))
				f.assertVolumeIsBack(ctx, g)
			}).WithContext(ctx).WithTimeout(15 * time.Minute).WithPolling(pollInterval).Should(Succeed())

			By("Checking the namesake claim is exactly as we left it")
			Consistently(func(g Gomega) {
				now, gerr := suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Get(ctx, f.exportPVCName, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred(), "the claim this export did not create was deleted")
				g.Expect(string(now.UID)).To(Equal(namesakeUID))
				g.Expect(now.DeletionTimestamp).To(BeNil(), "the claim this export did not create was marked for deletion")
				g.Expect(now.Annotations).NotTo(HaveKey(sfDataExportUIDAnno), "the export stamped its marker on a claim it found by name")
			}).WithContext(ctx).WithTimeout(60 * time.Second).WithPolling(10 * time.Second).Should(Succeed())
		})
	})

	// R9: deleting the DataExport during a recovery must not become a second, competing teardown. The
	// deletion path runs the same primitive and waits behind the same barrier; the finalizer is what buys
	// that wait, and dropping it early would leave the volume borrowed with no object left to give it back.
	Context("DataExport recovery 5: the DataExport is deleted mid-recovery", Ordered, func() {
		var f *deRecExportFixture

		deRecFixtureDiagnostics(&f, deRecDeleteExport)

		BeforeAll(func() {
			if !suiteCfg.dataExportRecovery {
				Skip("E2E_DATAEXPORT_RECOVERY=false: skipping the DataExport recovery flow (it runs by default)")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			f = deRecFreeVolume(ctx, "de-rec-del")
			f.startExport(ctx, deRecDeleteExport)
		})

		It("(g) keeps the object alive behind the barrier and removes it only once the volume is home", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			By("Pinning the exporter pod so the barrier outlives the deletion request")
			podName := exporterPodName(ctx, f.deployName)
			Expect(addPodFinalizer(ctx, d8DataManagerNS, podName, deRecHoldFinalizer)).To(Succeed())
			released := false
			DeferCleanup(func() {
				if released {
					return
				}
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				_ = removePodFinalizer(cctx, d8DataManagerNS, podName, deRecHoldFinalizer)
			})

			By("Deleting the export claim to start a recovery that cannot finish yet")
			Expect(suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Delete(ctx, f.exportPVCName, metav1.DeleteOptions{})).To(Succeed())
			Expect(dropPVCProtection(ctx, d8DataManagerNS, f.exportPVCName)).To(Succeed())
			Eventually(func(g Gomega) {
				obj, gerr := getResource(ctx, dataExportGVR, f.ns, f.exportName)
				g.Expect(gerr).NotTo(HaveOccurred())
				_, reason, _ := conditionStatus(obj, "Ready")
				g.Expect(reason).To(Equal("CleanupBlocked"))
				g.Expect(conditionMessage(obj, "Ready")).To(ContainSubstring("B1"))
			}).WithContext(ctx).WithTimeout(10 * time.Minute).WithPolling(pollInterval).Should(Succeed())

			By("Deleting the DataExport itself")
			deleteDataExport(ctx, f.ns, f.exportName)

			By("Checking the object waits behind the same barrier instead of disappearing")
			Consistently(func(g Gomega) {
				obj, gerr := getResource(ctx, dataExportGVR, f.ns, f.exportName)
				g.Expect(gerr).NotTo(HaveOccurred(), "the DataExport was removed while the volume was still borrowed")
				g.Expect(obj.GetDeletionTimestamp()).NotTo(BeNil(), "the deletion request did not reach the object")
				g.Expect(obj.GetFinalizers()).To(ContainElement(sfFinalizer), "the finalizer was dropped before the teardown finished")

				pv, gerr := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, f.pvName, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred())
				g.Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain))
				g.Expect(pv.Spec.ClaimRef).NotTo(BeNil())
				g.Expect(pv.Spec.ClaimRef.Name).To(Equal(f.exportPVCName), "the volume was rebound while the barrier held")

				claim, gerr := suiteClientset.CoreV1().PersistentVolumeClaims(f.ns).Get(ctx, deRecPVC, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred())
				g.Expect(claim.Status.Phase).NotTo(Equal(corev1.ClaimBound))
			}).WithContext(ctx).WithTimeout(90 * time.Second).WithPolling(10 * time.Second).Should(Succeed())

			By("Releasing the pod and letting the deletion complete")
			Expect(removePodFinalizer(ctx, d8DataManagerNS, podName, deRecHoldFinalizer)).To(Succeed())
			released = true

			Eventually(func(g Gomega) {
				f.assertVolumeIsBack(ctx, g)
				_, gerr := getResource(ctx, dataExportGVR, f.ns, f.exportName)
				g.Expect(apierrors.IsNotFound(gerr)).To(BeTrue(), "the DataExport is still there after the volume came back")
				_, gerr = suiteClientset.AppsV1().Deployments(d8DataManagerNS).Get(ctx, f.deployName, metav1.GetOptions{})
				g.Expect(apierrors.IsNotFound(gerr)).To(BeTrue(), "exporter Deployment still exists")
			}).WithContext(ctx).WithTimeout(15 * time.Minute).WithPolling(pollInterval).Should(Succeed())
		})
	})

	// R8: the marks an export leaves on a volume are also how a second export learns to keep its hands
	// off. Two exports borrowing one volume would each record a different way back, and the second one to
	// finish would undo the first one's takeover against a claim that no longer holds it.
	Context("DataExport recovery 6: the volume is already borrowed by another export", Ordered, func() {
		var f *deRecExportFixture

		deRecFixtureDiagnostics(&f, deRecConflictExport)

		BeforeAll(func() {
			if !suiteCfg.dataExportRecovery {
				Skip("E2E_DATAEXPORT_RECOVERY=false: skipping the DataExport recovery flow (it runs by default)")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			f = deRecFreeVolume(ctx, "de-rec-conf")
		})

		It("(h) refuses to start and leaves the volume exactly as it found it", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()

			By("Marking the volume as borrowed by a DataExport that is not this one")
			srcPVC, err := suiteClientset.CoreV1().PersistentVolumeClaims(f.ns).Get(ctx, deRecPVC, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(annotatePV(ctx, f.pvName, map[string]string{
				sfManagerNamespaceAnno: f.ns,
				sfManagerNameAnno:      deRecOtherExport,
				sfDataExportUIDAnno:    "11111111-1111-1111-1111-111111111111",
				sfUserPVCUIDAnno:       string(srcPVC.UID),
			})).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				// The volume outlives the namespace, and a stranger's marks on it would make the next run
				// (or the reclaim) deal with an export that never existed.
				_ = annotatePV(cctx, f.pvName, map[string]string{
					sfManagerNamespaceAnno: "",
					sfManagerNameAnno:      "",
					sfDataExportUIDAnno:    "",
					sfUserPVCUIDAnno:       "",
				})
			})
			pvBefore, err := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, f.pvName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Creating a second DataExport for the same volume")
			exportPVCName := generatedExportName("pvc-for", f.ns, deRecConflictExport)
			deployName := generatedExportName("deploy-for", f.ns, deRecConflictExport)
			Expect(createDataExportPVCTarget(ctx, f.ns, deRecConflictExport, deRecPVC, deRecTTL)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteDataExport(cctx, f.ns, deRecConflictExport)
			})

			By("Waiting for it to report the conflict, naming the export that holds the volume")
			Eventually(func(g Gomega) {
				obj, gerr := getResource(ctx, dataExportGVR, f.ns, deRecConflictExport)
				g.Expect(gerr).NotTo(HaveOccurred())
				st, reason, found := conditionStatus(obj, "Ready")
				g.Expect(found).To(BeTrue())
				g.Expect(st).To(Equal("False"))
				g.Expect(reason).To(Equal("PVConflict"), "message was: %s", conditionMessage(obj, "Ready"))
				g.Expect(conditionMessage(obj, "Ready")).To(ContainSubstring(deRecOtherExport))
			}).WithContext(ctx).WithTimeout(10 * time.Minute).WithPolling(pollInterval).Should(Succeed())

			By("Checking it built nothing and changed nothing about the volume")
			Consistently(func(g Gomega) {
				pvNow, gerr := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, f.pvName, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred())
				g.Expect(pvNow.Annotations).To(Equal(pvBefore.Annotations), "the volume's marks were rewritten")
				g.Expect(pvNow.Spec.PersistentVolumeReclaimPolicy).To(Equal(pvBefore.Spec.PersistentVolumeReclaimPolicy))
				g.Expect(pvNow.Spec.ClaimRef).NotTo(BeNil())
				g.Expect(pvNow.Spec.ClaimRef.Name).To(Equal(deRecPVC), "the volume was taken over by the second export")
				g.Expect(pvNow.Spec.ClaimRef.UID).To(Equal(srcPVC.UID))

				_, gerr = suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Get(ctx, exportPVCName, metav1.GetOptions{})
				g.Expect(apierrors.IsNotFound(gerr)).To(BeTrue(), "an export claim was created for a volume this export may not borrow")
				_, gerr = suiteClientset.AppsV1().Deployments(d8DataManagerNS).Get(ctx, deployName, metav1.GetOptions{})
				g.Expect(apierrors.IsNotFound(gerr)).To(BeTrue(), "an exporter was built for a volume this export may not borrow")

				obj, gerr := getResource(ctx, dataExportGVR, f.ns, deRecConflictExport)
				g.Expect(gerr).NotTo(HaveOccurred())
				_, hasRecovery, _ := unstructured.NestedMap(obj.Object, "status", "recovery")
				g.Expect(hasRecovery).To(BeFalse(), "a takeover that never happened was recorded")
			}).WithContext(ctx).WithTimeout(60 * time.Second).WithPolling(10 * time.Second).Should(Succeed())
		})
	})

	// R12: an export that started before the identity model has no recorded UIDs, and the module refuses
	// to invent them (see takeoverIdentityIsProvable). While it serves, that costs nothing. If it loses its
	// claim after the rebind, the volume's way back is only a name — and a name is what a recreated claim
	// also has, so the module stops and says so rather than hand a volume to whoever holds the name now.
	//
	// The scenario is simulated: the suite cannot install the pre-upgrade controller, so it strips from a
	// live export exactly what the upgrade added. The state it produces is unrecoverable on purpose, so
	// this spec — unlike every other one here — has to clean up by hand.
	Context("DataExport recovery 7: an export from before the identity model loses its claim", Ordered, func() {
		var f *deRecExportFixture

		deRecFixtureDiagnostics(&f, deRecLegacyExport)

		BeforeAll(func() {
			if !suiteCfg.dataExportRecovery {
				Skip("E2E_DATAEXPORT_RECOVERY=false: skipping the DataExport recovery flow (it runs by default)")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			f = deRecFreeVolume(ctx, "de-rec-lgc")
			f.startExport(ctx, deRecLegacyExport)
		})

		It("(i) refuses to guess which claim the volume belongs to", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			By("Remembering the identity before taking it away: it is what an operator would have to restore")
			liveExport, err := getResource(ctx, dataExportGVR, f.ns, f.exportName)
			Expect(err).NotTo(HaveOccurred())
			recordedIdentity, hasIdentity, err := unstructured.NestedMap(liveExport.Object, "status", "recovery")
			Expect(err).NotTo(HaveOccurred())
			Expect(hasIdentity).To(BeTrue(), "the export recorded no takeover identity to begin with")
			livePV, err := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, f.pvName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			volumeMarks := map[string]string{
				sfDataExportUIDAnno: livePV.Annotations[sfDataExportUIDAnno],
				sfUserPVCUIDAnno:    livePV.Annotations[sfUserPVCUIDAnno],
			}
			Expect(volumeMarks[sfUserPVCUIDAnno]).NotTo(BeEmpty(), "the volume carried no identity to begin with")

			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer ccancel()
				// Only reached if the spec did not get as far as restoring the identity: nothing here is
				// reaped by deleting the namespace while the DataExport still waits on its finalizer and
				// the volume keeps Retain with a claimRef to a claim that is gone.
				f.forceCleanupStrandedExport(cctx)
			})

			By("Stripping what the upgrade added, leaving the export as a pre-upgrade one")
			Expect(stripDataExportRecovery(ctx, f.ns, f.exportName)).To(Succeed())
			Expect(annotatePV(ctx, f.pvName, map[string]string{
				sfDataExportUIDAnno: "",
				sfUserPVCUIDAnno:    "",
			})).To(Succeed())

			// If the controller re-derived the identity from whatever is live now, every later comparison
			// would agree with itself and the scenario would silently become the happy path.
			By("Checking the controller does not write the identity back")
			Consistently(func(g Gomega) {
				obj, gerr := getResource(ctx, dataExportGVR, f.ns, f.exportName)
				g.Expect(gerr).NotTo(HaveOccurred())
				_, hasRecovery, _ := unstructured.NestedMap(obj.Object, "status", "recovery")
				g.Expect(hasRecovery).To(BeFalse(), "the takeover identity was re-recorded from live state")
				pv, gerr := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, f.pvName, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred())
				g.Expect(pv.Annotations).NotTo(HaveKey(sfUserPVCUIDAnno), "the volume was re-stamped with an identity nobody verified")
			}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(10 * time.Second).Should(Succeed())

			By("Losing the export claim, the way a pre-upgrade export would")
			Expect(suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Delete(ctx, f.exportPVCName, metav1.DeleteOptions{})).To(Succeed())
			Expect(dropPVCProtection(ctx, d8DataManagerNS, f.exportPVCName)).To(Succeed())
			Eventually(func() bool {
				_, gerr := suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Get(ctx, f.exportPVCName, metav1.GetOptions{})
				return apierrors.IsNotFound(gerr)
			}).WithContext(ctx).WithTimeout(5*time.Minute).WithPolling(pollInterval).Should(BeTrue(), "export claim did not go away")

			By("Waiting for the export to say it cannot prove where the volume came from")
			Eventually(func(g Gomega) {
				obj, gerr := getResource(ctx, dataExportGVR, f.ns, f.exportName)
				g.Expect(gerr).NotTo(HaveOccurred())
				st, reason, found := conditionStatus(obj, "Ready")
				g.Expect(found).To(BeTrue())
				g.Expect(st).To(Equal("False"))
				g.Expect(reason).To(Equal("CleanupBlocked"), "message was: %s", conditionMessage(obj, "Ready"))
				g.Expect(conditionMessage(obj, "Ready")).To(ContainSubstring("takeover identity"))
			}).WithContext(ctx).WithTimeout(10 * time.Minute).WithPolling(pollInterval).Should(Succeed())

			By("Checking the volume was not handed to a claim the export cannot prove")
			Consistently(func(g Gomega) {
				pv, gerr := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, f.pvName, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred())
				g.Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain), "the volume lost its protection while stranded")
				g.Expect(pv.Spec.ClaimRef).NotTo(BeNil())
				g.Expect(pv.Spec.ClaimRef.Name).To(Equal(f.exportPVCName), "the volume was rebound on the strength of a name")

				claim, gerr := suiteClientset.CoreV1().PersistentVolumeClaims(f.ns).Get(ctx, deRecPVC, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred())
				g.Expect(claim.Status.Phase).NotTo(Equal(corev1.ClaimBound))

				obj, gerr := getResource(ctx, dataExportGVR, f.ns, f.exportName)
				g.Expect(gerr).NotTo(HaveOccurred())
				cleanupReason, _, _ := unstructured.NestedString(obj.Object, "status", "cleanupReason")
				g.Expect(cleanupReason).To(BeEmpty(), "a teardown was scheduled against a claim the export cannot identify")
			}).WithContext(ctx).WithTimeout(60 * time.Second).WithPolling(10 * time.Second).Should(Succeed())

			// What was missing was the identity, not a working volume: supply it and the same export
			// finishes the return by itself. It is also the only way out of this state that an operator
			// actually has — removing the module's finalizer by hand is refused on clusters running the
			// admission-policy-engine module, which reserves deckhouse.io finalizers for the module's own
			// service account.
			By("Putting the identity back the way an operator would, and letting the return finish")
			Expect(restoreDataExportRecovery(ctx, f.ns, f.exportName, recordedIdentity)).To(Succeed())
			Expect(annotatePV(ctx, f.pvName, volumeMarks)).To(Succeed())
			Eventually(func(g Gomega) {
				f.assertVolumeIsBack(ctx, g)
			}).WithContext(ctx).WithTimeout(10 * time.Minute).WithPolling(pollInterval).Should(Succeed())
		})
	})
}

// deRecExportFixture is the starting state of a recovery scenario: a namespace of its own, a Filesystem
// PVC whose volume is free, and (for most scenarios) the export that borrowed that volume.
type deRecExportFixture struct {
	ns              string
	pvName          string
	originalReclaim corev1.PersistentVolumeReclaimPolicy
	exportName      string
	exportPVCName   string
	deployName      string
}

// deRecFreeVolume creates the namespace and a bound PVC, then frees the volume: the WaitForFirstConsumer
// class needs a pod to bind at all, and a live-PVC export refuses to start while one holds the claim.
// Registers its own namespace teardown.
func deRecFreeVolume(ctx context.Context, role string) *deRecExportFixture {
	GinkgoHelper()
	f := &deRecExportFixture{ns: uniqueNS(role)}

	By("Ensuring a thin StorageClass (" + suiteCfg.storageClass + ")")
	Expect(ensureSnapshotStorageClass(ctx, suiteCfg.storageClass)).To(Succeed())

	By("Creating the source namespace " + f.ns)
	Expect(ensureNamespace(ctx, f.ns)).To(Succeed())
	DeferCleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer ccancel()
		deleteNamespace(cctx, f.ns)
	})

	By("Creating the source PVC and freeing its volume")
	Expect(createFilesystemPVC(ctx, f.ns, deRecPVC, suiteCfg.storageClass, "1Gi")).To(Succeed())
	_, err := suiteClientset.CoreV1().Pods(f.ns).Create(ctx, probePodSpec(f.ns, deRecWriterPod, []string{deRecPVC}), metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "create the binding pod")
	Expect(waitPodRunning(ctx, f.ns, deRecWriterPod, 10*time.Minute)).To(Succeed())
	deletePod(ctx, f.ns, deRecWriterPod)
	Expect(waitPodDeleted(ctx, f.ns, deRecWriterPod, 5*time.Minute)).To(Succeed())

	srcPVC, err := suiteClientset.CoreV1().PersistentVolumeClaims(f.ns).Get(ctx, deRecPVC, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	f.pvName = srcPVC.Spec.VolumeName
	Expect(f.pvName).NotTo(BeEmpty(), "source PVC must be bound before the export starts")
	pv, err := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, f.pvName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	f.originalReclaim = pv.Spec.PersistentVolumeReclaimPolicy
	return f
}

// startExport creates the DataExport and waits until it serves the volume, filling in the generated names
// the scenarios disturb.
func (f *deRecExportFixture) startExport(ctx context.Context, name string) {
	GinkgoHelper()
	f.exportName = name
	f.exportPVCName = generatedExportName("pvc-for", f.ns, name)
	f.deployName = generatedExportName("deploy-for", f.ns, name)

	By("Creating the DataExport " + name + " and waiting until it serves the volume")
	Expect(createDataExportPVCTarget(ctx, f.ns, name, deRecPVC, deRecTTL)).To(Succeed())
	DeferCleanup(func() {
		if cleanupSkipped() {
			GinkgoWriter.Printf("%s: keeping DataExport %s/%s\n", keepReason(), f.ns, name)
			return
		}
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer ccancel()
		deleteDataExport(cctx, f.ns, name)
	})
	_, _, err := waitDataExportReady(ctx, f.ns, name, suiteCfg.dataTransferTO)
	Expect(err).NotTo(HaveOccurred(), "DataExport %s/%s did not become Ready", f.ns, name)
}

// assertVolumeIsBack is the end state every completed recovery must reach, whatever started it: the volume
// bound to the user's claim again, under its original policy, with no export marks left on it.
func (f *deRecExportFixture) assertVolumeIsBack(ctx context.Context, g Gomega) {
	pv, err := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, f.pvName, metav1.GetOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pv.Spec.ClaimRef).NotTo(BeNil())
	g.Expect(pv.Spec.ClaimRef.Namespace).To(Equal(f.ns))
	g.Expect(pv.Spec.ClaimRef.Name).To(Equal(deRecPVC))
	g.Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(f.originalReclaim), "the reclaim policy was not restored")
	g.Expect(pv.Annotations).NotTo(HaveKey(sfDataExportUIDAnno), "export metadata left on the volume")
	g.Expect(pv.Annotations).NotTo(HaveKey(sfOriginalReclaimAnno))

	claim, err := suiteClientset.CoreV1().PersistentVolumeClaims(f.ns).Get(ctx, deRecPVC, metav1.GetOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(claim.Status.Phase).To(Equal(corev1.ClaimBound), "the user's claim did not get its volume back")
}

// restoreDataExportRecovery writes a previously recorded takeover identity back onto an export. It is the
// operator's move in the legacy scenario: the module refuses to guess which claim a volume came from, but
// it acts on a record, so handing the record back is what lets the return finish.
func restoreDataExportRecovery(ctx context.Context, ns, name string, recovery map[string]interface{}) error {
	patch, err := json.Marshal(map[string]interface{}{"status": map[string]interface{}{"recovery": recovery}})
	if err != nil {
		return fmt.Errorf("marshal the recorded identity of DataExport %s/%s: %w", ns, name, err)
	}
	if _, err := suiteDyn.Resource(dataExportGVR).Namespace(ns).Patch(
		ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}, "status",
	); err != nil {
		return fmt.Errorf("restore status.recovery of DataExport %s/%s: %w", ns, name, err)
	}
	return nil
}

// forceCleanupStrandedExport is the fallback for a legacy scenario that never reached the point of putting
// the identity back: the DataExport still waits on its finalizer and the volume is still kept by Retain
// with a claimRef to a claim that no longer exists, so deleting the namespace reaps neither.
//
// It is best-effort by nature. On a cluster running the admission-policy-engine module, removing a
// finalizer containing "deckhouse.io" is refused for everyone but the module's own service account, so the
// debris is reported rather than cleared — which is also why the spec's normal ending restores the identity
// and lets the module release its own finalizer instead.
func (f *deRecExportFixture) forceCleanupStrandedExport(ctx context.Context) {
	if cleanupSkipped() {
		GinkgoWriter.Printf("%s: leaving the stranded export %s/%s and volume %s in place\n", keepReason(), f.ns, f.exportName, f.pvName)
		return
	}
	deleteDataExport(ctx, f.ns, f.exportName)
	if _, err := getResource(ctx, dataExportGVR, f.ns, f.exportName); apierrors.IsNotFound(err) {
		return
	}

	if err := retryOnConflict(ctx, func() error {
		obj, err := getResource(ctx, dataExportGVR, f.ns, f.exportName)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		obj.SetFinalizers(nil)
		_, err = suiteDyn.Resource(dataExportGVR).Namespace(f.ns).Update(ctx, obj, metav1.UpdateOptions{})
		return err
	}); err != nil {
		GinkgoWriter.Printf("force cleanup: clearing finalizers of DataExport %s/%s: %v\n", f.ns, f.exportName, err)
	}
	if err := clearPVCFinalizer(ctx, f.ns, deRecPVC, sfFinalizer); err != nil {
		GinkgoWriter.Printf("force cleanup: clearing the module finalizer of claim %s/%s: %v\n", f.ns, deRecPVC, err)
	}
	if err := suiteClientset.CoreV1().PersistentVolumes().Delete(ctx, f.pvName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		GinkgoWriter.Printf("force cleanup: deleting stranded PV %s: %v\n", f.pvName, err)
	}
}

// clearPVCFinalizer removes one finalizer from a claim. dropPVCProtection does the same for the kubelet's
// own guard; this one is for the module's, which a recovery that never completes never releases.
func clearPVCFinalizer(ctx context.Context, ns, name, finalizer string) error {
	return retryOnConflict(ctx, func() error {
		claim, err := suiteClientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		kept := claim.Finalizers[:0]
		for _, f := range claim.Finalizers {
			if f != finalizer {
				kept = append(kept, f)
			}
		}
		if len(kept) == len(claim.Finalizers) {
			return nil
		}
		claim.Finalizers = kept
		_, err = suiteClientset.CoreV1().PersistentVolumeClaims(ns).Update(ctx, claim, metav1.UpdateOptions{})
		return err
	})
}

// recordedExportPVCUID reads the export claim UID the export pinned when it borrowed the volume. It is the
// value the scenarios have to disagree with in order to be about identity at all.
func recordedExportPVCUID(ctx context.Context, ns, name string) string {
	GinkgoHelper()
	obj, err := getResource(ctx, dataExportGVR, ns, name)
	Expect(err).NotTo(HaveOccurred())
	uid, _, _ := unstructured.NestedString(obj.Object, "status", "recovery", "exportPVCUID")
	return uid
}

// pauseDataManagerController scales the controller to zero and waits until none of its pods is left, so a
// test can compose a state out of several writes and have the controller observe only the result. It
// returns the replica count to restore.
//
// Deckhouse owns this Deployment and will reconcile the count back on its own schedule, so the pause is
// kept to a few writes and resume restores it explicitly rather than relying on that.
func pauseDataManagerController(ctx context.Context) (int32, error) {
	deploy, err := suiteClientset.AppsV1().Deployments(d8DataManagerNS).Get(ctx, sfControllerDeploy, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("get Deployment %s/%s: %w", d8DataManagerNS, sfControllerDeploy, err)
	}
	replicas := int32(1)
	if deploy.Spec.Replicas != nil {
		replicas = *deploy.Spec.Replicas
	}
	if err := scaleDataManagerController(ctx, 0); err != nil {
		return replicas, err
	}

	selector := fmt.Sprintf("%s=%s", sfAppLabel, sfControllerApp)
	Eventually(func(g Gomega) {
		list, gerr := suiteClientset.CoreV1().Pods(d8DataManagerNS).List(ctx, metav1.ListOptions{LabelSelector: selector})
		g.Expect(gerr).NotTo(HaveOccurred())
		g.Expect(list.Items).To(BeEmpty(), "controller pods are still running")
	}).WithContext(ctx).WithTimeout(5 * time.Minute).WithPolling(pollInterval).Should(Succeed())
	return replicas, nil
}

func resumeDataManagerController(ctx context.Context, replicas int32) error {
	if err := scaleDataManagerController(ctx, replicas); err != nil {
		return err
	}
	Eventually(func(g Gomega) {
		deploy, gerr := suiteClientset.AppsV1().Deployments(d8DataManagerNS).Get(ctx, sfControllerDeploy, metav1.GetOptions{})
		g.Expect(gerr).NotTo(HaveOccurred())
		g.Expect(deploy.Status.AvailableReplicas).To(BeNumerically(">=", 1))
	}).WithContext(ctx).WithTimeout(10 * time.Minute).WithPolling(pollInterval).Should(Succeed())
	return nil
}

func scaleDataManagerController(ctx context.Context, replicas int32) error {
	patch := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	_, err := suiteClientset.AppsV1().Deployments(d8DataManagerNS).Patch(ctx, sfControllerDeploy, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("scale Deployment %s/%s to %d: %w", d8DataManagerNS, sfControllerDeploy, replicas, err)
	}
	return nil
}

// annotatePV sets annotations on a volume; an empty value removes the key. Used to plant marks a foreign
// export would have left, and to take the module's own marks away again.
func annotatePV(ctx context.Context, name string, annotations map[string]string) error {
	return retryOnConflict(ctx, func() error {
		pv, err := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if pv.Annotations == nil {
			pv.Annotations = map[string]string{}
		}
		for key, value := range annotations {
			if value == "" {
				delete(pv.Annotations, key)
				continue
			}
			pv.Annotations[key] = value
		}
		_, err = suiteClientset.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
		return err
	})
}

// stripDataExportRecovery removes the recorded takeover identity from a live export, which is half of what
// an export started before the identity model has: no record of its own, and no UIDs on the volume.
func stripDataExportRecovery(ctx context.Context, ns, name string) error {
	_, err := suiteDyn.Resource(dataExportGVR).Namespace(ns).Patch(
		ctx, name, types.MergePatchType, []byte(`{"status":{"recovery":null}}`), metav1.PatchOptions{}, "status",
	)
	if err != nil {
		return fmt.Errorf("strip status.recovery of DataExport %s/%s: %w", ns, name, err)
	}
	return nil
}

// createDataExportPVCTarget creates a DataExport (publish:false) for a live PersistentVolumeClaim. It is
// the non-publish counterpart of createPublishDataExportPVC: these specs are about the volume, and the
// ingress machinery would only add unrelated ways to fail.
func createDataExportPVCTarget(ctx context.Context, ns, name, pvcName, ttl string) error {
	de := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": dataExportGVR.GroupVersion().String(),
		"kind":       "DataExport",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"ttl":     ttl,
			"publish": false,
			"targetRef": map[string]interface{}{
				"group": "",
				"kind":  "PersistentVolumeClaim",
				"name":  pvcName,
			},
		},
	}}
	_, err := suiteDyn.Resource(dataExportGVR).Namespace(ns).Create(ctx, de, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// generatedExportName rebuilds a generated resource name the way the module does (common/names.go:
// "<prefix>-<targetKindShort>-<suffix>", where the suffix is the sha256 of "<namespace>\x00<name>", first
// 10 bytes, lowercase base32-hex without padding). The target kind short of a live PVC export is "pvc".
//
// The rule is duplicated rather than imported because the suite keeps no build dependency on the module;
// spec (a) cross-checks the result against the claim the controller actually created, so a drift shows up
// there instead of turning every scenario into a mystery.
func generatedExportName(prefix, deNamespace, deName string) string {
	sum := sha256.Sum256([]byte(deNamespace + "\x00" + deName))
	suffix := strings.ToLower(base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:10]))
	return fmt.Sprintf("%s-pvc-%s", prefix, suffix)
}

// conditionMessage returns the message of one condition (conditionStatus returns only status+reason, and
// the barrier contract lives in the message: which barrier, and which object is holding it).
func conditionMessage(obj *unstructured.Unstructured, condType string) string {
	conds, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return ""
	}
	for _, raw := range conds {
		cond, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(cond, "type"); t != condType {
			continue
		}
		msg, _, _ := unstructured.NestedString(cond, "message")
		return msg
	}
	return ""
}

// exporterPodName returns the single pod of an exporter Deployment, waiting for it to appear.
func exporterPodName(ctx context.Context, deployName string) string {
	var name string
	Eventually(func(g Gomega) {
		list, err := suiteClientset.CoreV1().Pods(d8DataManagerNS).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s,%s=%s", sfAppLabel, sfExporterAppValue, sfDeployNameLabel, deployName),
		})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(list.Items).To(HaveLen(1), "expected exactly one exporter pod for %s", deployName)
		g.Expect(list.Items[0].Status.Phase).To(Equal(corev1.PodRunning))
		name = list.Items[0].Name
	}).WithContext(ctx).WithTimeout(10 * time.Minute).WithPolling(pollInterval).Should(Succeed())
	return name
}

// addPodFinalizer / removePodFinalizer pin a pod in place and let it go again. Both re-read the object on
// conflict: the controller and the kubelet write to these pods too.
func addPodFinalizer(ctx context.Context, ns, name, finalizer string) error {
	return retryOnConflict(ctx, func() error {
		pod, err := suiteClientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for _, f := range pod.Finalizers {
			if f == finalizer {
				return nil
			}
		}
		pod.Finalizers = append(pod.Finalizers, finalizer)
		_, err = suiteClientset.CoreV1().Pods(ns).Update(ctx, pod, metav1.UpdateOptions{})
		return err
	})
}

func removePodFinalizer(ctx context.Context, ns, name, finalizer string) error {
	return retryOnConflict(ctx, func() error {
		pod, err := suiteClientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		kept := pod.Finalizers[:0]
		for _, f := range pod.Finalizers {
			if f != finalizer {
				kept = append(kept, f)
			}
		}
		if len(kept) == len(pod.Finalizers) {
			return nil
		}
		pod.Finalizers = kept
		_, err = suiteClientset.CoreV1().Pods(ns).Update(ctx, pod, metav1.UpdateOptions{})
		return err
	})
}

// volumeAttachmentForPV returns the name of the VolumeAttachment referencing a volume, or "" if none
// appears within the timeout. An empty result is a legitimate answer: a driver that does not require
// attach never creates one.
func volumeAttachmentForPV(ctx context.Context, pvName string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for {
		list, err := suiteClientset.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
		if err == nil {
			for i := range list.Items {
				source := list.Items[i].Spec.Source.PersistentVolumeName
				if source != nil && *source == pvName {
					return list.Items[i].Name
				}
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		if !sleepCtx(ctx, pollInterval) {
			return ""
		}
	}
}

// setVolumeAttachmentFinalizer adds or removes this suite's finalizer on a VolumeAttachment. Holding one
// keeps the object alive after the attach-detach controller has deleted it, which is what makes the
// window between "the pod is gone" and "the volume is detached" observable from a test.
func setVolumeAttachmentFinalizer(ctx context.Context, name, finalizer string, present bool) error {
	return retryOnConflict(ctx, func() error {
		attachment, err := suiteClientset.StorageV1().VolumeAttachments().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		kept := make([]string, 0, len(attachment.Finalizers)+1)
		found := false
		for _, f := range attachment.Finalizers {
			if f == finalizer {
				found = true
				continue
			}
			kept = append(kept, f)
		}
		if present {
			if found {
				return nil
			}
			kept = append(kept, finalizer)
		} else if !found {
			return nil
		}
		attachment.Finalizers = kept
		_, err = suiteClientset.StorageV1().VolumeAttachments().Update(ctx, attachment, metav1.UpdateOptions{})
		return err
	})
}

// dropPVCProtection removes the kubelet's pvc-protection finalizer from a terminating claim, which is
// what makes the claim actually disappear while a pod still references it.
func dropPVCProtection(ctx context.Context, ns, name string) error {
	return retryOnConflict(ctx, func() error {
		claim, err := suiteClientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		kept := claim.Finalizers[:0]
		for _, f := range claim.Finalizers {
			if f != pvcProtectionFinalizer {
				kept = append(kept, f)
			}
		}
		if len(kept) == len(claim.Finalizers) {
			return nil
		}
		claim.Finalizers = kept
		_, err = suiteClientset.CoreV1().PersistentVolumeClaims(ns).Update(ctx, claim, metav1.UpdateOptions{})
		return err
	})
}

// restartDataManagerController deletes the controller pods and waits until the Deployment is available
// again with none of the old pods left. It does not wait for leader election: what the caller asserts is
// the behaviour of the object, and a controller that has not taken the lease yet simply does nothing.
func restartDataManagerController(ctx context.Context) error {
	selector := fmt.Sprintf("%s=%s", sfAppLabel, sfControllerApp)
	before, err := suiteClientset.CoreV1().Pods(d8DataManagerNS).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list controller pods: %w", err)
	}
	old := make(map[string]struct{}, len(before.Items))
	for i := range before.Items {
		old[before.Items[i].Name] = struct{}{}
		if derr := suiteClientset.CoreV1().Pods(d8DataManagerNS).Delete(ctx, before.Items[i].Name, metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
			return fmt.Errorf("delete controller pod %s: %w", before.Items[i].Name, derr)
		}
	}
	if len(old) == 0 {
		return fmt.Errorf("no pods matching %s in %s", selector, d8DataManagerNS)
	}

	Eventually(func(g Gomega) {
		deploy, gerr := suiteClientset.AppsV1().Deployments(d8DataManagerNS).Get(ctx, sfControllerDeploy, metav1.GetOptions{})
		g.Expect(gerr).NotTo(HaveOccurred())
		g.Expect(deploy.Status.AvailableReplicas).To(BeNumerically(">=", 1))
		list, gerr := suiteClientset.CoreV1().Pods(d8DataManagerNS).List(ctx, metav1.ListOptions{LabelSelector: selector})
		g.Expect(gerr).NotTo(HaveOccurred())
		for i := range list.Items {
			_, isOld := old[list.Items[i].Name]
			g.Expect(isOld).To(BeFalse(), "old controller pod %s is still there", list.Items[i].Name)
		}
	}).WithContext(ctx).WithTimeout(10 * time.Minute).WithPolling(pollInterval).Should(Succeed())
	return nil
}

// retryOnConflict re-runs an update that lost an optimistic-lock race, until the context runs out.
func retryOnConflict(ctx context.Context, fn func() error) error {
	for {
		err := fn()
		if !apierrors.IsConflict(err) {
			return err
		}
		if !sleepCtx(ctx, time.Second) {
			return ctx.Err()
		}
	}
}
