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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// NOT YET EXECUTED. This spec has never been run against a cluster: it needs a StorageClass whose CSI
// driver is deployed WITHOUT the external-snapshotter sidecar, which the suite cannot provision for
// itself (unlike the thick-SC injection in freeze_deadline_test.go, which only needs a different LVM
// type). It is opt-in for exactly that reason, and compiles as part of the suite so it does not rot.
//
//	KUBECONFIG=... E2E_CAPTURE_STALL=true E2E_CAPTURE_STALL_STORAGE_CLASS=ceph-rbd-sc \
//	  go test ./tests -count=1 -timeout=240m -v -ginkgo.v \
//	  -ginkgo.focus='Capture stall diagnostics'
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
	// has even added its finalizer. Accepted as a pass with a warning: which of the two is reported
	// depends on whether the common snapshot-controller runs, which is not what this spec pins.
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
	csVMName           = "capture-stall-vm"
	csDiskName         = "capture-stall-disk"
	csPVCName          = "capture-stall-pvc"
	csProbePod         = "capture-stall-probe"
)

// csNoPickupGrace mirrors defaultNoPickupGrace in storage-foundation's stall classifier: below it a young
// VolumeSnapshotContent is simply young, not stalled. Waits are sized past it, not around it.
const csNoPickupGrace = 2 * time.Minute

// buildCaptureStallSource returns the source tree: a ConfigMap for the manifest leg plus a
// DemoVirtualMachine owning ONE DemoVirtualDisk backed by a PVC on the snapshotter-less StorageClass.
//
// The shape deliberately copies buildFreezeDeadlineSource — it is the proven way to obtain a domain child
// whose data leg goes through a VolumeCaptureRequest. The difference is the failure injected: there, CSI
// CreateSnapshot runs and rejects a thick volume; here nothing runs at all, because no sidecar watches the
// VolumeSnapshotContent. That is precisely the distinction the stall classifier has to express, and the
// reason it cannot claim "the snapshotter is absent" — it can only report that nothing is observable.
func buildCaptureStallSource(ns, stallSC string) []*unstructured.Unstructured {
	configMap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      csConfigMapName,
			"namespace": ns,
		},
		"data": map[string]interface{}{"capture": "stall"},
	}}
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
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{"storage": "64Mi"},
			},
		},
	}}
	vm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualMachine",
		"metadata": map[string]interface{}{
			"name":      csVMName,
			"namespace": ns,
		},
		"spec": map[string]interface{}{"virtualDiskName": csDiskName},
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
			"size":                      "64Mi",
			"storageClassName":          stallSC,
		},
	}}
	return []*unstructured.Unstructured{configMap, pvc, vm, disk}
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

// captureStallSpecs registers the case-B regression: a data leg whose CSI driver has no snapshotter
// sidecar must produce an actionable diagnosis instead of an unexplained wait, and must NOT be mistaken
// for a failure.
//
// What the run proves, end to end:
//  1. the VolumeCaptureRequest reports Stalled=True/SnapshotExecutionUnobservable;
//  2. the diagnosis reaches the domain snapshot as the single reason DataCaptureStalled;
//  3. Ready stays False/TargetsPending — the request is diagnosed, not failed;
//  4. the request is non-terminal and garbage collection leaves it alone, so the capture still completes
//     if the missing sidecar is deployed later.
//
// Opt-in (E2E_CAPTURE_STALL=true): see the NOT-YET-EXECUTED note at the top of this file.
func captureStallSpecs() {
	Context("Capture stall diagnostics (no snapshotter sidecar -> SnapshotExecutionUnobservable)", func() {
		var (
			srcNS   string
			stallSC string
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

			By("Asserting the StorageClass under test exists (" + stallSC + ")")
			_, err := suiteClientset.StorageV1().StorageClasses().Get(ctx, stallSC, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "StorageClass %s must exist; this spec cannot provision a snapshotter-less driver itself", stallSC)

			By("Creating the source namespace and applying the VM + disk source")
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			Expect(applyObjects(ctx, buildCaptureStallSource(srcNS, stallSC), srcNS)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, srcNS)
			})

			By("Starting the probe Pod so the PVC binds (WaitForFirstConsumer binds on schedule)")
			_, err = suiteClientset.CoreV1().Pods(srcNS).Create(ctx, probePodSpec(srcNS, csProbePod, []string{csPVCName}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create source probe pod")
			Expect(waitPodRunning(ctx, srcNS, csProbePod, 10*time.Minute)).To(Succeed())
		})

		It("diagnoses the unobservable data leg without failing it or collecting it", func() {
			// Budget: reach the disk child, then outlast the classifier's no-pickup grace, then the
			// propagation and the non-collection window.
			ctx, cancel := context.WithTimeout(context.Background(), 3*suiteCfg.captureReadyTO+csNoPickupGrace+20*time.Minute)
			defer cancel()

			tl := startCaptureTimeline(srcNS)
			defer tl.stop()

			By("Creating the root Snapshot over the VM + disk tree")
			Expect(createRootSnapshot(ctx, srcNS, csRootSnapshotName)).To(Succeed())

			By("Resolving the DemoVirtualDiskSnapshot child whose data leg can never be picked up")
			diskSnapName, err := waitSnapshotChildOfKind(ctx, srcNS, csRootSnapshotName, "DemoVirtualDiskSnapshot", 2*suiteCfg.captureReadyTO+5*time.Minute)
			Expect(err).NotTo(HaveOccurred(), "the VM must own a DemoVirtualDiskSnapshot child")
			GinkgoWriter.Printf("  stalled disk snapshot: %s\n", diskSnapName)

			By("Waiting for the VolumeCaptureRequest to report the stall (past the no-pickup grace)")
			var vcrName string
			Eventually(func(g Gomega) {
				vcr, gerr := csFindVCRForPVC(ctx, srcNS, csPVCName)
				g.Expect(gerr).NotTo(HaveOccurred())
				vcrName = vcr.GetName()

				stalledStatus, stalledReason, found := conditionStatus(vcr, csConditionStalled)
				g.Expect(found).To(BeTrue(), "VolumeCaptureRequest %s must carry a %s condition", vcrName, csConditionStalled)
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
			conds, _, _ := unstructured.NestedSlice(vcr.Object, "status", "conditions")
			var stallMessage string
			for _, item := range conds {
				m, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				if t, _, _ := unstructured.NestedString(m, "type"); t == csConditionStalled {
					stallMessage, _, _ = unstructured.NestedString(m, "message")
				}
			}
			Expect(stallMessage).To(ContainSubstring("VolumeSnapshotContent"),
				"the diagnosis must name the VolumeSnapshotContent an operator has to look at, got %q", stallMessage)

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
