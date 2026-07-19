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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// envFreezeDeadline opts this spec OUT. It runs by default (as part of the phase-3 volume-data flow): the
// spec mutates the SHARED poc domain-controller Deployment (a cluster-wide FREEZE_DEADLINE env patch,
// restored on cleanup) and provisions cluster-scoped StorageClass wiring, so an environment that cannot
// support it disables the spec with E2E_FREEZE_DEADLINE=false (and the whole flow with E2E_VOLUME_DATA=false).
// Mirrors the E2E_CHILD_BRIDGE_FAILURE gate.
const envFreezeDeadline = "E2E_FREEZE_DEADLINE"

// envFreezeDeadlineValue overrides the short FREEZE_DEADLINE injected into the domain controller for this
// spec (default fdDefaultFreezeDeadline). It must be a Go duration; a very small value risks the deadline
// firing before the reconcile loop can stamp+observe the freeze marker, so keep it a few seconds at least.
const envFreezeDeadlineValue = "E2E_FREEZE_DEADLINE_VALUE"

// fdDefaultFreezeDeadline is the short freeze deadline injected for the spec. The deadline clock only starts
// AFTER the VM's own (manifest-only) leg is captured and its children are found not-yet-settled (the domain
// stamps the freeze-started-at marker there), so this bounds only the child wait, never the manifest capture.
const fdDefaultFreezeDeadline = 30 * time.Second

// Freeze-deadline fixture object names (isolated from the phase-3 vol-tree and child-bridge names so this
// spec can run in the same suite without colliding on the shared source fixtures).
const (
	fdRootSnapshotName = "freeze-deadline"
	fdConfigMapName    = "freeze-deadline-cm"
	fdVMName           = "freeze-deadline-vm"
	fdDiskName         = "freeze-deadline-disk"
	fdPVCName          = "freeze-deadline-pvc"
	fdProbePod         = "freeze-deadline-probe"
)

// poc domain-controller Deployment coordinates (chart d8-<name>; container == app label == "domain-controller").
// The FreezeDeadline is read from the FREEZE_DEADLINE env (pkg/config), which the chart does NOT template as a
// value/ModuleConfig knob — so the ONLY way to shorten it for a deployed controller is to patch this env and
// wait for the rollout (verified against images/domain-controller/pkg/config/config.go +
// templates/domain-controller/deployment.yaml).
const (
	pocDomainControllerNS        = "d8-sds-unified-snapshots-poc"
	pocDomainControllerDeploy    = "domain-controller"
	pocDomainControllerContainer = "domain-controller"
	freezeDeadlineEnvName        = "FREEZE_DEADLINE"
)

// Demo domain contract mirrored here (kept in sync with the poc module, feat/domain-aggapi-facades):
//   - the VM snapshot self-Fails with this free-form domain reason when the freeze deadline elapses before
//     every child disk snapshot settled (materialization_constants.go: demoReasonConsistencyDeadlineExceeded);
//   - the durable freeze-start marker the domain stamps while waiting and CLEARS on the (notational) unfreeze
//     (materialization_constants.go: demoAnnotationFreezeStartedAt).
const (
	demoReasonConsistencyDeadlineExceededE2E = "ConsistencyDeadlineExceeded"
	demoFreezeStartedAtAnnotation            = "sds-unified-snapshots-poc.deckhouse.io/freeze-started-at"
)

// buildFreezeDeadlineSource returns the source tree for the freeze-deadline scenario: a ConfigMap (manifest
// leg) plus a DemoVirtualMachine owning ONE DemoVirtualDisk backed by a PVC on the thick, snapshot-INCAPABLE
// StorageClass. The disk provisions and binds normally, but its data-leg VolumeCaptureRequest can never
// complete: sds-local-volume's CSI CreateSnapshot rejects a non-thin (thick) source volume, and (per the sf
// VCR fix) a CSI snapshot error is NON-terminal — the VCR stays Ready=False/TargetsPending and requeues
// forever. So the child DemoVirtualDiskSnapshot HANGS non-terminally; it never goes terminal, so the VM's
// core-owned childrenSettled latch never flips true, and the domain-side freeze deadline is the only thing
// that resolves the VM snapshot.
func buildFreezeDeadlineSource(ns, thickSC string) []*unstructured.Unstructured {
	configMap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      fdConfigMapName,
			"namespace": ns,
		},
		"data": map[string]interface{}{"freeze": "deadline"},
	}}
	pvc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":      fdPVCName,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"accessModes":      []interface{}{"ReadWriteOnce"},
			"storageClassName": thickSC,
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{"storage": "500Mi"},
			},
		},
	}}
	vm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualMachine",
		"metadata": map[string]interface{}{
			"name":      fdVMName,
			"namespace": ns,
		},
		"spec": map[string]interface{}{"virtualDiskName": fdDiskName},
	}}
	disk := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualDisk",
		"metadata": map[string]interface{}{
			"name":      fdDiskName,
			"namespace": ns,
		},
		// size + storageClassName satisfy the scratch-provisioning guards; the disk adopts the pre-created PVC.
		"spec": map[string]interface{}{
			"persistentVolumeClaimName": fdPVCName,
			"size":                      "500Mi",
			"storageClassName":          thickSC,
		},
	}}
	return []*unstructured.Unstructured{configMap, pvc, vm, disk}
}

// provisionThickNoSnapStorageClass materializes a THICK, snapshot-INCAPABLE StorageClass that reuses the
// base thin SC's LVMVolumeGroups (thick LVs live in the VG space outside the thin pool), then annotates it
// with an EXISTING, driver-matching VolumeSnapshotClass. This is the deterministic "hung child" injection:
//
//   - the SC/VSC resolution in the sf VCR path SUCCEEDS (the SC carries storage.deckhouse.io/volumesnapshotclass,
//     the VSC exists and its driver == the thick PV's CSI driver local.csi.storage.deckhouse.io) — so the VCR
//     gets PAST every terminal precondition (NotFound / RBACDenied / driver-mismatch / InternalError) and reaches
//     the actual CSI CreateSnapshot;
//   - but sds-local-volume's CSI CreateSnapshot rejects a non-thin source with codes.InvalidArgument
//     ("Source LVMLogicalVolume '...' is not of 'Thin' type"), which the external-snapshotter records on the
//     VolumeSnapshotContent status.error and retries forever; and the sf VCR fix makes that CSI error
//     NON-terminal (Ready=False/TargetsPending + requeue), never a terminal SnapshotCreationFailed.
//
// Contrast with child_bridge_failure_test.go, which points the SC at a NON-existent VSC and thus fails the
// child TERMINALLY (NotFound) — the opposite of what this spec needs (a terminal child would flip
// childrenSettled=true and the deadline would never fire).
func provisionThickNoSnapStorageClass(ctx context.Context, baseSC, thickSC, vscName string) error {
	base, err := suiteDyn.Resource(storagekube.LocalStorageClassGVR).Get(ctx, baseSC, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get base LocalStorageClass %s: %w", baseSC, err)
	}
	lvgRefs, found, err := unstructured.NestedSlice(base.Object, "spec", "lvm", "lvmVolumeGroups")
	if err != nil || !found {
		return fmt.Errorf("base LocalStorageClass %s has no spec.lvm.lvmVolumeGroups (found=%v): %w", baseSC, found, err)
	}
	var lvgNames []string
	for _, item := range lvgRefs {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if n, _, _ := unstructured.NestedString(m, "name"); n != "" {
			lvgNames = append(lvgNames, n)
		}
	}
	if len(lvgNames) == 0 {
		return fmt.Errorf("base LocalStorageClass %s exposes no lvmVolumeGroup names", baseSC)
	}

	if err := storagekube.CreateLocalStorageClass(ctx, suiteRestCfg, storagekube.LocalStorageClassConfig{
		Name:            thickSC,
		LVMVolumeGroups: lvgNames,
		LVMType:         "Thick",
	}); err != nil {
		return fmt.Errorf("create thick LocalStorageClass %s: %w", thickSC, err)
	}
	if err := storagekube.WaitForLocalStorageClassCreated(ctx, suiteRestCfg, thickSC, 5*time.Minute); err != nil {
		return fmt.Errorf("thick LocalStorageClass %s did not reach Created: %w", thickSC, err)
	}
	if err := storagekube.WaitForStorageClass(ctx, suiteRestCfg, thickSC, 2*time.Minute); err != nil {
		return fmt.Errorf("thick StorageClass %s did not appear: %w", thickSC, err)
	}
	// Annotation-only update on the freshly created SC: wire it to the existing thin VolumeSnapshotClass so
	// the VCR resolves the class and reaches CreateSnapshot (which then fails on the thick source volume).
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, annStorageClassVSC, vscName))
	if _, err := suiteClientset.StorageV1().StorageClasses().Patch(ctx, thickSC, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("annotate thick StorageClass %s with %s=%s: %w", thickSC, annStorageClassVSC, vscName, err)
	}
	return nil
}

// currentDomainControllerFreezeDeadlineEnv reports the FREEZE_DEADLINE env currently set on the
// domain-controller container (present + value), so the spec can restore it exactly on cleanup.
func currentDomainControllerFreezeDeadlineEnv(ctx context.Context) (value string, present bool, err error) {
	d, err := suiteClientset.AppsV1().Deployments(pocDomainControllerNS).Get(ctx, pocDomainControllerDeploy, metav1.GetOptions{})
	if err != nil {
		return "", false, fmt.Errorf("get Deployment %s/%s: %w", pocDomainControllerNS, pocDomainControllerDeploy, err)
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name != pocDomainControllerContainer {
			continue
		}
		for _, e := range c.Env {
			if e.Name == freezeDeadlineEnvName {
				return e.Value, true, nil
			}
		}
	}
	return "", false, nil
}

// setDomainControllerFreezeDeadlineEnv strategic-merge-patches the FREEZE_DEADLINE env (merge key: env name)
// on the domain-controller container, leaving the other env entries untouched.
func setDomainControllerFreezeDeadlineEnv(ctx context.Context, value string) error {
	patch := []byte(fmt.Sprintf(
		`{"spec":{"template":{"spec":{"containers":[{"name":%q,"env":[{"name":%q,"value":%q}]}]}}}}`,
		pocDomainControllerContainer, freezeDeadlineEnvName, value))
	_, err := suiteClientset.AppsV1().Deployments(pocDomainControllerNS).Patch(
		ctx, pocDomainControllerDeploy, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("set %s=%s on Deployment %s/%s: %w", freezeDeadlineEnvName, value, pocDomainControllerNS, pocDomainControllerDeploy, err)
	}
	return nil
}

// deleteDomainControllerFreezeDeadlineEnv removes the FREEZE_DEADLINE env from the domain-controller
// container via a strategic-merge delete directive (used to restore when the env was absent originally).
func deleteDomainControllerFreezeDeadlineEnv(ctx context.Context) error {
	patch := []byte(fmt.Sprintf(
		`{"spec":{"template":{"spec":{"containers":[{"name":%q,"env":[{"name":%q,"$patch":"delete"}]}]}}}}`,
		pocDomainControllerContainer, freezeDeadlineEnvName))
	_, err := suiteClientset.AppsV1().Deployments(pocDomainControllerNS).Patch(
		ctx, pocDomainControllerDeploy, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("delete %s from Deployment %s/%s: %w", freezeDeadlineEnvName, pocDomainControllerNS, pocDomainControllerDeploy, err)
	}
	return nil
}

// waitDeploymentRolledOut blocks until the Deployment's newest generation is fully rolled out (observed,
// all replicas updated + available, none unavailable), so the spec never races an in-flight rollout of the
// FREEZE_DEADLINE change.
func waitDeploymentRolledOut(ctx context.Context, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		d, err := suiteClientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			desired := int32(1)
			if d.Spec.Replicas != nil {
				desired = *d.Spec.Replicas
			}
			if d.Status.ObservedGeneration >= d.Generation &&
				d.Status.UpdatedReplicas >= desired &&
				d.Status.AvailableReplicas >= desired &&
				d.Status.ReadyReplicas >= desired &&
				d.Status.UnavailableReplicas == 0 {
				return nil
			}
			last = fmt.Sprintf("gen=%d observed=%d updated=%d ready=%d avail=%d unavail=%d desired=%d",
				d.Generation, d.Status.ObservedGeneration, d.Status.UpdatedReplicas, d.Status.ReadyReplicas,
				d.Status.AvailableReplicas, d.Status.UnavailableReplicas, desired)
		} else {
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for Deployment %s/%s rollout; last: %s", ns, name, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// demoDomainPhaseReason reads the domain-owned lifecycle from a demo snapshot's
// status.captureState.domainSpecificController (phase + free-form reason).
func demoDomainPhaseReason(obj *unstructured.Unstructured) (phase, reason string) {
	phase, _, _ = unstructured.NestedString(obj.Object, "status", "captureState", "domainSpecificController", "phase")
	reason, _, _ = unstructured.NestedString(obj.Object, "status", "captureState", "domainSpecificController", "reason")
	return phase, reason
}

// freezeDeadlineSpecs registers the end-to-end regression that ties the three parts of the freeze-deadline
// feature together (VCR-non-terminal -> childrenSettled does NOT latch -> freeze-deadline self-Fail):
//
//	VM snapshot with one disk child whose data-leg VolumeCaptureRequest hangs non-terminally (thick source
//	volume => CSI CreateSnapshot InvalidArgument => sf VCR keeps it Ready=False/TargetsPending forever) =>
//	the VM's core-owned childrenSettled latch never flips true => on the short (injected) FREEZE_DEADLINE the
//	demo VM controller performs its notational guest-fs unfreeze and self-Fails the VM snapshot with the
//	domain reason ConsistencyDeadlineExceeded, clearing the freeze-started-at marker.
//
// This is the integration counterpart to the fake-clock unit tests in
// images/domain-controller/internal/controllers/demo/virtualmachinesnapshot_barrier_test.go, which cannot
// synthesize a genuinely hung child through the real CSI + VCR path.
//
// Opt-out (E2E_FREEZE_DEADLINE=false): it patches the SHARED poc domain-controller Deployment (a cluster-wide
// FREEZE_DEADLINE change, restored on cleanup) and provisions cluster StorageClass wiring; it runs by
// default as part of the volume-data flow and is disabled on environments that cannot support it.
func freezeDeadlineSpecs() {
	Context("Freeze deadline (hung child disk snapshot -> VM self-Fail ConsistencyDeadlineExceeded)", func() {
		var (
			srcNS         string
			baseSC        string
			thickSC       string
			freezeValue   string
			freezeDur     time.Duration
			envWasPresent bool
			envOriginal   string
		)

		BeforeAll(func() {
			if !suiteCfg.volumeData || !envEnabledByDefault(os.Getenv(envFreezeDeadline)) {
				Skip("freeze-deadline spec disabled: it runs by default; set " + envFreezeDeadline + "=false (or E2E_VOLUME_DATA=false) to disable")
			}
			baseSC = suiteCfg.storageClass
			thickSC = baseSC + "-thick-nosnap"
			srcNS = uniqueNS("freeze-deadline")

			freezeValue = os.Getenv(envFreezeDeadlineValue)
			if freezeValue == "" {
				freezeValue = fdDefaultFreezeDeadline.String()
			}
			var perr error
			freezeDur, perr = time.ParseDuration(freezeValue)
			Expect(perr).NotTo(HaveOccurred(), "%s must be a Go duration", envFreezeDeadlineValue)
			Expect(freezeDur > 0).To(BeTrue(), "%s must be positive", envFreezeDeadlineValue)

			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
			defer cancel()

			By("Provisioning the thin, snapshot-capable base StorageClass via storage-e2e (" + baseSC + ")")
			_, err := testkit.EnsureDefaultStorageClass(ctx, suiteRestCfg, testkit.DefaultStorageClassConfig{
				StorageClassName:     baseSC,
				LVMType:              "Thin",
				ThinPoolName:         "thinpool",
				BaseKubeconfig:       suiteClusterResources.BaseKubeconfig,
				VMNamespace:          suiteCfg.vmNamespace,
				BaseStorageClassName: suiteCfg.baseStorageClass,
			})
			Expect(err).NotTo(HaveOccurred(), "provision base StorageClass")

			By("Resolving a driver-matching VolumeSnapshotClass for the local CSI driver")
			vscName, err := resolveLocalVolumeSnapshotClass(ctx)
			Expect(err).NotTo(HaveOccurred(), "resolve local VolumeSnapshotClass")

			By("Provisioning a THICK, snapshot-incapable StorageClass wired to the thin VSC (" + thickSC + " -> " + vscName + ")")
			Expect(provisionThickNoSnapStorageClass(ctx, baseSC, thickSC, vscName)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				if cleanupSkipped() {
					GinkgoWriter.Printf("%s: keeping LocalStorageClass/StorageClass %q\n", keepReason(), thickSC)
					return
				}
				// The LocalStorageClass owns the backing StorageClass; deleting it lets the sds-local-volume
				// controller remove the SC (a direct SC delete would be recreated / blocked by its webhook).
				_ = suiteDyn.Resource(storagekube.LocalStorageClassGVR).Delete(cctx, thickSC, metav1.DeleteOptions{})
			})

			By("Recording and shortening the poc domain-controller FREEZE_DEADLINE to " + freezeValue + " (Deployment env patch + rollout)")
			envOriginal, envWasPresent, err = currentDomainControllerFreezeDeadlineEnv(ctx)
			Expect(err).NotTo(HaveOccurred(), "read current FREEZE_DEADLINE env")
			Expect(setDomainControllerFreezeDeadlineEnv(ctx, freezeValue)).To(Succeed())
			Expect(waitDeploymentRolledOut(ctx, pocDomainControllerNS, pocDomainControllerDeploy, 10*time.Minute)).
				To(Succeed(), "domain-controller must roll out the shortened FREEZE_DEADLINE before any snapshot is created")
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer ccancel()
				// Restore the controller's original FREEZE_DEADLINE so a shared, long-lived cluster is not left
				// with the aggressive test deadline (which would fail slow VM snapshots in later runs).
				if envWasPresent {
					_ = setDomainControllerFreezeDeadlineEnv(cctx, envOriginal)
				} else {
					_ = deleteDomainControllerFreezeDeadlineEnv(cctx)
				}
				_ = waitDeploymentRolledOut(cctx, pocDomainControllerNS, pocDomainControllerDeploy, 10*time.Minute)
			})

			By("Creating the source namespace and applying the VM + thick-disk source")
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			Expect(applyObjects(ctx, buildFreezeDeadlineSource(srcNS, thickSC), srcNS)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, srcNS)
			})

			By("Starting the source probe Pod so the thick PVC binds (WaitForFirstConsumer binds on schedule)")
			_, err = suiteClientset.CoreV1().Pods(srcNS).Create(ctx, probePodSpec(srcNS, fdProbePod, []string{fdPVCName}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create source probe pod")
			Expect(waitPodRunning(ctx, srcNS, fdProbePod, 10*time.Minute)).To(Succeed())
		})

		It("fails the VM snapshot with ConsistencyDeadlineExceeded and clears the freeze marker when a child disk snapshot hangs", func() {
			// Budget: resolve the VM child + reach the deadline (capture + freeze deadline) + the mirror/marker
			// follow-ups. Each sub-wait is sized below; the parent covers their sum plus a generous buffer.
			ctx, cancel := context.WithTimeout(context.Background(), 4*suiteCfg.captureReadyTO+freezeDur+15*time.Minute)
			defer cancel()

			tl := startCaptureTimeline(srcNS)
			defer tl.stop()

			By("Creating the root Snapshot over the VM + thick-disk tree")
			Expect(createRootSnapshot(ctx, srcNS, fdRootSnapshotName)).To(Succeed())

			By("Resolving the DemoVirtualMachineSnapshot and its hung DemoVirtualDiskSnapshot child")
			vmSnapName, err := waitSnapshotChildOfKind(ctx, srcNS, fdRootSnapshotName, "DemoVirtualMachineSnapshot", 2*suiteCfg.captureReadyTO+5*time.Minute)
			Expect(err).NotTo(HaveOccurred(), "root Snapshot must publish a DemoVirtualMachineSnapshot child")
			diskSnapName, err := waitSnapshotChildOfKind(ctx, srcNS, fdRootSnapshotName, "DemoVirtualDiskSnapshot", 2*suiteCfg.captureReadyTO+5*time.Minute)
			Expect(err).NotTo(HaveOccurred(), "the VM must own a DemoVirtualDiskSnapshot child")
			GinkgoWriter.Printf("  VM snapshot: %s; hung disk snapshot: %s\n", vmSnapName, diskSnapName)

			// The deadline clock starts only after the VM's own manifest leg is captured and children are found
			// not-yet-settled, so size this wait as capture-time + the freeze deadline + a buffer.
			By("Waiting for the VM snapshot to self-Fail on the freeze deadline (domain phase=Failed / reason=ConsistencyDeadlineExceeded)")
			Eventually(func(g Gomega) {
				obj, gerr := getResource(ctx, demoVMSnapshotGVR, srcNS, vmSnapName)
				g.Expect(gerr).NotTo(HaveOccurred())
				phase, reason := demoDomainPhaseReason(obj)
				GinkgoWriter.Printf("  VM %s domain phase=%q reason=%q\n", vmSnapName, phase, reason)
				g.Expect(phase).To(Equal("Failed"), "the VM snapshot must reach the terminal domain phase Failed")
				g.Expect(reason).To(Equal(demoReasonConsistencyDeadlineExceededE2E),
					"the VM must self-Fail with the freeze-deadline domain reason")
			}).WithContext(ctx).WithTimeout(2*suiteCfg.captureReadyTO + freezeDur + 5*time.Minute).WithPolling(pollInterval).Should(Succeed())

			// Correctness guard (distinguishes this from child_bridge's TERMINAL child): the deadline must fire
			// because the child HUNG, not because it failed terminally. A terminal child would flip
			// childrenSettled=true and the VM would instead reach Ready=False/ChildrenFailed — the deadline would
			// never run. A terminal data-leg failure (VolumeCaptureFailed) is core-owned and surfaces ONLY on the
			// disk's Ready CONDITION (Ready=False/VolumeCaptureFailed); the demo domain leaves its own phase at
			// Planned in that branch (virtualdisksnapshot_controller.go CaptureOutcomeFailed -> return nil), so a
			// domain-phase check alone cannot see it. So the authoritative "still hung" signal is the disk's Ready
			// reason NOT being in the terminal set. We assert all three: domain phase not Failed/Finished, Ready
			// not True, and Ready reason not terminal (the hang carries TargetsPending, not VolumeCaptureFailed).
			By("Asserting the disk child is still NON-terminal (the VM failed on the deadline, not on a terminal child)")
			disk, err := getResource(ctx, demoDiskSnapshotGVR, srcNS, diskSnapName)
			Expect(err).NotTo(HaveOccurred())
			diskPhase, diskDomainReason := demoDomainPhaseReason(disk)
			diskReadyStatus, diskReadyReason, _ := conditionStatus(disk, condReady)
			GinkgoWriter.Printf("  disk %s domain phase=%q domainReason=%q Ready=%s/%s\n", diskSnapName, diskPhase, diskDomainReason, diskReadyStatus, diskReadyReason)
			Expect(diskPhase).NotTo(Equal("Failed"), "the hung disk child must NOT be terminally Failed (else childrenSettled would latch)")
			Expect(diskPhase).NotTo(Equal("Finished"), "the hung disk child must NOT be Finished (its data leg can never complete)")
			Expect(diskReadyStatus).NotTo(Equal("True"), "the hung disk child must NOT be Ready=True")
			Expect(storagev1alpha1.IsReasonTerminal(diskReadyReason)).To(BeFalse(),
				"the hung disk child's Ready reason %q must NOT be terminal (a terminal VolumeCaptureFailed/ChildrenFailed would latch childrenSettled and skip the deadline)", diskReadyReason)

			By("Asserting the VM's Ready condition mirrors Ready=False/ConsistencyDeadlineExceeded (core mirrors the domain phase)")
			Eventually(func(g Gomega) {
				obj, gerr := getResource(ctx, demoVMSnapshotGVR, srcNS, vmSnapName)
				g.Expect(gerr).NotTo(HaveOccurred())
				st, reason, found := conditionStatus(obj, condReady)
				g.Expect(found).To(BeTrue(), "VM snapshot must carry a Ready condition")
				g.Expect(st).To(Equal("False"), "VM snapshot Ready must be False after the deadline self-Fail")
				g.Expect(reason).To(Equal(demoReasonConsistencyDeadlineExceededE2E),
					"the core must mirror the domain phase=Failed reason verbatim onto Ready")
			}).WithContext(ctx).WithTimeout(2*suiteCfg.captureReadyTO + time.Minute).WithPolling(pollInterval).Should(Succeed())

			// The notational guest-fs unfreeze is observable via its recorded side effect: the domain stamps the
			// freeze-started-at marker while waiting and CLEARS it in thawGuestIfFrozen on the deadline path
			// (the ONLY producer of reason=ConsistencyDeadlineExceeded). So the marker being absent AFTER the
			// deadline self-Fail proves the unfreeze ran. (There is no k8s Event; the real fsthaw lives in the
			// virtualization module, out of scope for this PoC.)
			By("Asserting the freeze-started-at marker is cleared (the notational unfreeze ran)")
			Eventually(func(g Gomega) {
				obj, gerr := getResource(ctx, demoVMSnapshotGVR, srcNS, vmSnapName)
				g.Expect(gerr).NotTo(HaveOccurred())
				_, present := obj.GetAnnotations()[demoFreezeStartedAtAnnotation]
				g.Expect(present).To(BeFalse(), "the freeze-started-at marker must be cleared once the guest fs is (notationally) unfrozen")
			}).WithContext(ctx).WithTimeout(2*suiteCfg.captureReadyTO + time.Minute).WithPolling(pollInterval).Should(Succeed())
		})
	})
}
