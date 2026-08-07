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

package tests

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// Opt-in: this spec needs a StorageClass whose CSI driver is deployed WITHOUT the external-snapshotter
// sidecar, which is an environment property the suite cannot create for itself.
//
//	KUBECONFIG=... E2E_CAPTURE_STALL=true E2E_CAPTURE_STALL_STORAGE_CLASS=ceph-rbd-sc \
//	  go test ./tests -count=1 -timeout=240m -v -ginkgo.v \
//	  -ginkgo.focus='Capture stall diagnostics'
//
// The volume is provisioned STATICALLY on purpose (see buildCaptureStallSource). Known residue in a
// case-B environment: the VolumeSnapshotContent this spec causes has the cluster snapshot-controller's
// finalizer and no sidecar to release it, so after cleanup it can stay Terminating. That is inherent to
// the environment under test — nothing here strips foreign finalizers, and clearing such content is a
// separate manual procedure.
const (
	// envCaptureStall opts this spec IN. Every other volume-data spec runs by default; this one cannot,
	// because a snapshotter-less driver is an environment property, not something the suite can create.
	envCaptureStall = "E2E_CAPTURE_STALL"
	// envCaptureStallStorageClass names the StorageClass whose CSI driver has no snapshotter sidecar.
	envCaptureStallStorageClass = "E2E_CAPTURE_STALL_STORAGE_CLASS"
	// csDefaultStorageClass is the class case B was observed on (design §2).
	csDefaultStorageClass = "ceph-rbd-sc"
)

// Stall vocabulary published by storage-foundation on the VolumeCaptureRequest. Mirrored here as literals
// on purpose: the e2e module must observe the wire contract a deployed controller writes, not recompile
// against the constants, or a rename would silently keep the assertions passing.
const (
	csConditionStalled = "Stalled"
	// csReasonUnobservable is case B: the VolumeSnapshotContent exists, nothing ever picked it up.
	csReasonUnobservable = "SnapshotExecutionUnobservable"
	// csReasonStackUnavailable is the earlier phase of the same absence — before the snapshot-controller
	// has even added its finalizer. Accepted as a pass for the END state (which of the two is reported
	// depends on whether the common snapshot-controller runs, which is not what this spec pins), but
	// FORBIDDEN inside the no-pickup grace — see the negative assertion in the spec body.
	csReasonStackUnavailable = "SnapshotStackUnavailable"
	// csReasonDataCaptureStalled is the single reason the diagnosis is folded into above the foundation.
	csReasonDataCaptureStalled = "DataCaptureStalled"
	// csReasonTargetsPending is what Ready must keep reporting: a stall is a diagnosis, never a verdict.
	csReasonTargetsPending = "TargetsPending"
)

// Fixture names, kept distinct from the phase-3 and freeze-deadline trees so this spec can share a run.
const (
	csRootSnapshotName = "capture-stall"
	csConfigMapName    = "capture-stall-cm"
	csDiskName         = "capture-stall-disk"
	csPVCName          = "capture-stall-pvc"
	csVolumeSize       = "1Gi"
)

// csNoPickupGrace mirrors defaultNoPickupGrace in storage-foundation's stall classifier: below it a young
// VolumeSnapshotContent is simply young, not stalled. Waits are sized past it, not around it.
const csNoPickupGrace = 2 * time.Minute

// csGraceMargin is cut off both ends of the no-pickup grace so the negative assertion below never races
// the threshold it is guarding.
const csGraceMargin = 15 * time.Second

// csVolumeSnapshotClassGVR is local to this spec: it is read only to prove the capture preconditions
// hold before the stall is attributed to a missing executor.
var csVolumeSnapshotClassGVR = schema.GroupVersionResource{
	Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotclasses",
}

// The PersistentVolume is cluster-scoped and the volume handle must not collide with a parallel run, so
// both are derived from the unique source namespace.
func csPVName(ns string) string       { return ns + "-pv" }
func csVolumeHandle(ns string) string { return "e2e-capture-stall-" + ns }

// buildCaptureStallSource returns the source tree: a ConfigMap for the manifest leg plus ONE
// DemoVirtualDisk backed by a STATICALLY provisioned, pre-bound PVC on the snapshotter-less StorageClass.
//
// Why static provisioning and no probe Pod. The fixture must not depend on the component whose absence is
// under test: in a real deployment the external-provisioner and the external-snapshotter sidecars live in
// the SAME Deployment, so a driver without a snapshotter usually cannot provision a PVC either —
// dynamic provisioning would make the fixture unbuildable exactly where case B exists. A pre-bound PVC
// removes that coupling (and the WaitForFirstConsumer probe Pod with it), which is also more faithful:
// this spec ends BEFORE any CSI RPC, so a live data path proves nothing here. The volume handle is
// synthetic for the same reason — nothing ever calls the driver with it.
//
// No DemoVirtualMachine either: the VM controller starts a Pod that mounts the disk, and a synthetic
// volume cannot be mounted. The data leg needs no VM parent — a DemoVirtualDisk is its own snapshottable
// kind (CustomSnapshotDefinition demo-virtual-disk), so the root Snapshot resolves a DemoVirtualDiskSnapshot
// child directly.
func buildCaptureStallSource(ns, stallSC, driver string) []*unstructured.Unstructured {
	configMap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      csConfigMapName,
			"namespace": ns,
		},
		"data": map[string]interface{}{"capture": "stall"},
	}}
	// Retain: this PV must never hand a synthetic handle to a deletion call, and the object is removed
	// explicitly in cleanup.
	pv := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolume",
		"metadata": map[string]interface{}{
			"name": csPVName(ns),
		},
		"spec": map[string]interface{}{
			"capacity":                      map[string]interface{}{"storage": csVolumeSize},
			"accessModes":                   []interface{}{"ReadWriteOnce"},
			"persistentVolumeReclaimPolicy": "Retain",
			"storageClassName":              stallSC,
			"volumeMode":                    "Filesystem",
			"claimRef": map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "PersistentVolumeClaim",
				"namespace":  ns,
				"name":       csPVCName,
			},
			"csi": map[string]interface{}{
				"driver":       driver,
				"volumeHandle": csVolumeHandle(ns),
				"fsType":       "ext4",
			},
		},
	}}
	// volumeName pre-binds the claim, so it binds without a provisioner and without a consumer.
	pvc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":      csPVCName,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"accessModes":      []interface{}{"ReadWriteOnce"},
			"storageClassName": stallSC,
			"volumeName":       csPVName(ns),
			"volumeMode":       "Filesystem",
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{"storage": csVolumeSize},
			},
		},
	}}
	disk := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualDisk",
		"metadata": map[string]interface{}{
			"name":      csDiskName,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"persistentVolumeClaimName": csPVCName,
			"size":                      csVolumeSize,
			"storageClassName":          stallSC,
			"volumeMode":                "Filesystem",
		},
	}}
	return []*unstructured.Unstructured{configMap, pv, pvc, disk}
}

// csFindVCRForPVC returns the in-flight VolumeCaptureRequest in ns whose immutable spec.target names the
// source PVC. Looking it up by target rather than by name keeps the spec independent of the core's VCR
// naming formula (which also feeds the foundation's event delivery index).
func csFindVCRForPVC(ctx context.Context, ns, pvc string) (*unstructured.Unstructured, error) {
	list, err := suiteDyn.Resource(volumeCaptureRequestGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list VolumeCaptureRequests in %s: %w", ns, err)
	}
	for i := range list.Items {
		targetName, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "target", "name")
		if targetName == pvc {
			return &list.Items[i], nil
		}
	}
	return nil, fmt.Errorf("no VolumeCaptureRequest in %s targets PVC %s", ns, pvc)
}

// csFindContentForHandle returns the VolumeSnapshotContent the capture produced for this run's volume.
// It is matched by the synthetic volume handle instead of by name so the spec does not re-derive
// storage-foundation's internal VolumeSnapshotContent naming formula.
func csFindContentForHandle(ctx context.Context, handle string) (*unstructured.Unstructured, error) {
	list, err := suiteDyn.Resource(volumeSnapshotContentGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list VolumeSnapshotContents: %w", err)
	}
	for i := range list.Items {
		got, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "source", "volumeHandle")
		if got == handle {
			return &list.Items[i], nil
		}
	}
	return nil, fmt.Errorf("no VolumeSnapshotContent sources volume handle %s", handle)
}

// csStallCondition returns the status, reason and message of the Stalled condition, if present.
func csStallCondition(vcr *unstructured.Unstructured) (status, reason, message string, found bool) {
	conds, _, _ := unstructured.NestedSlice(vcr.Object, "status", "conditions")
	for _, item := range conds {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(m, "type"); t != csConditionStalled {
			continue
		}
		status, _, _ = unstructured.NestedString(m, "status")
		reason, _, _ = unstructured.NestedString(m, "reason")
		message, _, _ = unstructured.NestedString(m, "message")
		return status, reason, message, true
	}
	return "", "", "", false
}

// captureStallSpecs registers the case-B regression: a data leg whose CSI driver has no snapshotter
// sidecar must produce an actionable diagnosis instead of an unexplained wait, and must NOT be mistaken
// for a failure.
//
// What the run proves, end to end:
//  1. every capture precondition holds (bound volume, CSI handle, snapshot class) — so the diagnosis can
//     only be about the missing executor and not about a broken fixture;
//  2. NOTHING is diagnosed while the content is younger than the no-pickup grace;
//  3. past the grace the VolumeCaptureRequest reports Stalled=True/SnapshotExecutionUnobservable;
//  4. the diagnosis reaches the domain snapshot as the single reason DataCaptureStalled;
//  5. Ready stays False/TargetsPending — the request is diagnosed, not failed;
//  6. the request is non-terminal and garbage collection leaves it alone, so the capture still completes
//     if the missing sidecar is deployed later.
func captureStallSpecs() {
	Context("Capture stall diagnostics (no snapshotter sidecar -> SnapshotExecutionUnobservable)", func() {
		var (
			srcNS   string
			stallSC string
			driver  string
		)

		BeforeAll(func() {
			if !envBool(os.Getenv(envCaptureStall)) {
				Skip("capture-stall spec is opt-in: set " + envCaptureStall + "=true and point " +
					envCaptureStallStorageClass + " at a StorageClass whose CSI driver runs without the snapshotter sidecar")
			}
			stallSC = strings.TrimSpace(os.Getenv(envCaptureStallStorageClass))
			if stallSC == "" {
				stallSC = csDefaultStorageClass
			}
			srcNS = uniqueNS("p3-capture-stall")

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			By("Reading the StorageClass under test (" + stallSC + ")")
			sc, err := suiteClientset.StorageV1().StorageClasses().Get(ctx, stallSC, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "StorageClass %s must exist; this spec cannot deploy a snapshotter-less driver itself", stallSC)
			driver = sc.Provisioner
			Expect(driver).NotTo(BeEmpty(), "StorageClass %s must name a CSI provisioner", stallSC)

			// Preconditions of the capture path itself. Asserting them here is what makes the later
			// diagnosis attributable: a stall reported on a fixture that never had a snapshot class would
			// say nothing about a missing executor.
			By("Asserting the StorageClass carries the snapshot class annotation")
			vscClassName := sc.Annotations[annStorageClassVSC]
			Expect(vscClassName).NotTo(BeEmpty(), "StorageClass %s must be annotated with %s for a volume capture to be possible at all", stallSC, annStorageClassVSC)

			By("Asserting the VolumeSnapshotClass exists and matches the driver")
			vscClass, err := suiteDyn.Resource(csVolumeSnapshotClassGVR).Get(ctx, vscClassName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "VolumeSnapshotClass %s (from the %s annotation) must exist", vscClassName, annStorageClassVSC)
			classDriver, _, _ := unstructured.NestedString(vscClass.Object, "driver")
			Expect(classDriver).To(Equal(driver), "VolumeSnapshotClass %s must serve the StorageClass driver", vscClassName)

			By("Creating the source namespace and applying the static volume + disk source")
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				// The PersistentVolume is cluster-scoped, so it outlives the namespace and is removed
				// explicitly. It only releases once the claim is gone, hence the ordering: this runs after
				// the namespace deletion registered below (Ginkgo cleanup is LIFO).
				err := suiteClientset.CoreV1().PersistentVolumes().Delete(cctx, csPVName(srcNS), metav1.DeleteOptions{})
				if err != nil && !apierrors.IsNotFound(err) {
					GinkgoWriter.Printf("  cleanup: delete PersistentVolume %s: %v\n", csPVName(srcNS), err)
				}
			})
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, srcNS)
			})
			Expect(applyObjects(ctx, buildCaptureStallSource(srcNS, stallSC, driver), srcNS)).To(Succeed())

			By("Asserting the statically provisioned claim binds without a provisioner and without a consumer")
			Eventually(func(g Gomega) {
				pvc, err := suiteClientset.CoreV1().PersistentVolumeClaims(srcNS).Get(ctx, csPVCName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pvc.Status.Phase).To(Equal(corev1.ClaimBound), "a pre-bound claim must not wait for dynamic provisioning")
			}).WithContext(ctx).WithTimeout(5 * time.Minute).WithPolling(pollInterval).Should(Succeed())

			By("Asserting the bound volume carries the CSI identity the capture path reads")
			pv, err := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, csPVName(srcNS), metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(pv.Spec.CSI).NotTo(BeNil(), "the volume must be a CSI volume or the capture fails on preconditions, not on a stall")
			Expect(pv.Spec.CSI.Driver).To(Equal(driver))
			Expect(pv.Spec.CSI.VolumeHandle).NotTo(BeEmpty())
		})

		It("diagnoses the unobservable data leg without failing it or collecting it", func() {
			// Budget: reach the disk child, outlast the classifier's no-pickup grace, then the propagation
			// and the non-collection window.
			ctx, cancel := context.WithTimeout(context.Background(), 3*suiteCfg.captureReadyTO+csNoPickupGrace+20*time.Minute)
			defer cancel()

			tl := startCaptureTimeline(srcNS)
			defer tl.stop()

			By("Creating the root Snapshot over the disk tree")
			Expect(createRootSnapshot(ctx, srcNS, csRootSnapshotName)).To(Succeed())

			By("Resolving the DemoVirtualDiskSnapshot child whose data leg can never be picked up")
			diskSnapName, err := waitSnapshotChildOfKind(ctx, srcNS, csRootSnapshotName, "DemoVirtualDiskSnapshot", 2*suiteCfg.captureReadyTO+5*time.Minute)
			Expect(err).NotTo(HaveOccurred(), "the disk must produce a DemoVirtualDiskSnapshot child")
			GinkgoWriter.Printf("  stalled disk snapshot: %s\n", diskSnapName)

			// Found by handle and as early as possible: the negative window below is measured from this
			// content's own creationTimestamp, which is also the classifier's reference point.
			By("Resolving the VolumeSnapshotContent the capture produced")
			var content *unstructured.Unstructured
			Eventually(func(g Gomega) {
				var gerr error
				content, gerr = csFindContentForHandle(ctx, csVolumeHandle(srcNS))
				g.Expect(gerr).NotTo(HaveOccurred())
			}).WithContext(ctx).WithTimeout(2*suiteCfg.captureReadyTO + 5*time.Minute).WithPolling(time.Second).Should(Succeed())
			GinkgoWriter.Printf("  content: %s (age %s at discovery)\n", content.GetName(), time.Since(content.GetCreationTimestamp().Time).Truncate(time.Second))

			By("Asserting the content was created and nothing has written a result to it")
			status, found, _ := unstructured.NestedMap(content.Object, "status")
			Expect(found && len(status) > 0).To(BeFalse(), "a content nobody picked up must carry no status, got %v", status)

			// Regression guard for the false alarm this spec exists to keep out: the content watch wakes the
			// reconcile on the content's own creation event, so the classifier runs about a second after the
			// content appears — before the cluster snapshot-controller can add its finalizer. Reporting
			// SnapshotStackUnavailable there accuses a healthy stack on every capture. Asserting only the
			// end state would let that regression back in unnoticed.
			negativeWindow := csNoPickupGrace - csGraceMargin - time.Since(content.GetCreationTimestamp().Time)
			if negativeWindow <= 0 {
				GinkgoWriter.Printf("  WARNING: content was already %s old when discovered; skipping the in-grace negative assertion\n",
					time.Since(content.GetCreationTimestamp().Time).Truncate(time.Second))
			} else {
				By(fmt.Sprintf("Asserting nothing is diagnosed for the next %s (within the no-pickup grace)", negativeWindow.Truncate(time.Second)))
				Consistently(func(g Gomega) {
					vcr, gerr := csFindVCRForPVC(ctx, srcNS, csPVCName)
					g.Expect(gerr).NotTo(HaveOccurred())

					st, reason, _, ok := csStallCondition(vcr)
					g.Expect(reason).NotTo(Equal(csReasonStackUnavailable),
						"a young content must never be blamed on the snapshot stack: the finalizer simply has not arrived yet")
					if ok {
						g.Expect(st).NotTo(Equal("True"), "no stall may be diagnosed within the no-pickup grace, got reason %q", reason)
					}

					_, readyReason, rok := conditionStatus(vcr, condReady)
					g.Expect(rok).To(BeTrue())
					g.Expect(readyReason).To(Equal(csReasonTargetsPending))
				}).WithContext(ctx).WithTimeout(negativeWindow).WithPolling(2 * time.Second).Should(Succeed())

				// Informational: the finalizer normally lands inside the window, and its arrival must not
				// have produced a diagnosis either (asserted above).
				if latest, gerr := csFindContentForHandle(ctx, csVolumeHandle(srcNS)); gerr == nil {
					GinkgoWriter.Printf("  content finalizers after the grace window: %v\n", latest.GetFinalizers())
				}
			}

			By("Waiting for the VolumeCaptureRequest to report the stall (past the no-pickup grace)")
			var vcrName string
			Eventually(func(g Gomega) {
				vcr, gerr := csFindVCRForPVC(ctx, srcNS, csPVCName)
				g.Expect(gerr).NotTo(HaveOccurred())
				vcrName = vcr.GetName()

				stalledStatus, stalledReason, _, ok := csStallCondition(vcr)
				g.Expect(ok).To(BeTrue(), "VolumeCaptureRequest %s must carry a %s condition", vcrName, csConditionStalled)
				GinkgoWriter.Printf("  VCR %s Stalled=%s/%s\n", vcrName, stalledStatus, stalledReason)
				g.Expect(stalledStatus).To(Equal("True"), "a data leg nothing ever picks up must be reported as stalled")
				g.Expect(stalledReason).To(BeElementOf(csReasonUnobservable, csReasonStackUnavailable),
					"the diagnosis must be one of the two absence reasons, got %q", stalledReason)
				if stalledReason == csReasonStackUnavailable {
					GinkgoWriter.Printf("  note: the common snapshot-controller has not claimed the content either (%s)\n", csReasonStackUnavailable)
				}
			}).WithContext(ctx).WithTimeout(2*suiteCfg.captureReadyTO + csNoPickupGrace + 5*time.Minute).
				WithPolling(pollInterval).Should(Succeed())

			By("Asserting the message is actionable: it names the artifact nothing is working on")
			vcr, err := csFindVCRForPVC(ctx, srcNS, csPVCName)
			Expect(err).NotTo(HaveOccurred())
			_, _, stallMessage, _ := csStallCondition(vcr)
			Expect(stallMessage).To(ContainSubstring("VolumeSnapshotContent"),
				"the diagnosis must name the VolumeSnapshotContent an operator has to look at, got %q", stallMessage)
			Expect(stallMessage).To(ContainSubstring(driver),
				"the diagnosis must name the CSI driver whose executor is missing, got %q", stallMessage)

			By("Asserting Ready stays False/TargetsPending — a diagnosis is not a verdict")
			readyStatus, readyReason, found := conditionStatus(vcr, condReady)
			Expect(found).To(BeTrue(), "VolumeCaptureRequest %s must carry a Ready condition", vcrName)
			Expect(readyStatus).To(Equal("False"))
			Expect(readyReason).To(Equal(csReasonTargetsPending),
				"the stall reason must never be written onto Ready (it would make the request look terminal)")

			By("Asserting the diagnosis reaches the domain snapshot as a single DataCaptureStalled reason")
			Eventually(func(g Gomega) {
				disk, gerr := getResource(ctx, demoDiskSnapshotGVR, srcNS, diskSnapName)
				g.Expect(gerr).NotTo(HaveOccurred())
				st, reason, ok := conditionStatus(disk, condReady)
				g.Expect(ok).To(BeTrue(), "disk snapshot must carry a Ready condition")
				GinkgoWriter.Printf("  disk %s Ready=%s/%s\n", diskSnapName, st, reason)
				g.Expect(st).To(Equal("False"))
				g.Expect(reason).To(Equal(csReasonDataCaptureStalled),
					"the storage-specific reason must be folded into one snapshot-level reason above the foundation")
				g.Expect(storagev1alpha1.IsReasonTerminal(reason)).To(BeFalse(),
					"%s must stay non-terminal: the capture still succeeds if the sidecar is deployed later", reason)
			}).WithContext(ctx).WithTimeout(2*suiteCfg.captureReadyTO + 5*time.Minute).WithPolling(pollInterval).Should(Succeed())

			By("Asserting the stalled request survives: nothing terminalizes or collects it")
			Consistently(func(g Gomega) {
				live, gerr := csFindVCRForPVC(ctx, srcNS, csPVCName)
				g.Expect(gerr).NotTo(HaveOccurred(), "the stalled VolumeCaptureRequest must not be garbage collected")
				g.Expect(live.GetName()).To(Equal(vcrName), "the request must not be recreated under a new identity")
				g.Expect(live.GetDeletionTimestamp()).To(BeNil(), "a stalled request must not be marked for deletion")

				_, readyReason, ok := conditionStatus(live, condReady)
				g.Expect(ok).To(BeTrue())
				g.Expect(readyReason).To(Equal(csReasonTargetsPending), "Ready must not drift to a terminal reason while stalled")
			}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(pollInterval).Should(Succeed())
		})
	})
}
