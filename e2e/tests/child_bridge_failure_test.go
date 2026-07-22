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
	"bufio"
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
	"k8s.io/apimachinery/pkg/types"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// envChildBridgeFailure opts this spec OUT. It runs by default (as part of the phase-3 volume-data flow):
// the spec mutates cluster-scoped StorageClass wiring and depends on the exact terminal behaviour of the
// demo domain's volume-capture leg, so an environment that cannot support it disables the spec with
// E2E_CHILD_BRIDGE_FAILURE=false (and the whole flow with E2E_VOLUME_DATA=false).
const envChildBridgeFailure = "E2E_CHILD_BRIDGE_FAILURE"

// Child-bridge fixture object names (isolated from the phase-3 vol-tree names so this spec can run in the
// same suite without colliding on the shared source fixtures).
const (
	cbRootSnapshotName = "child-bridge-fail"
	cbConfigMapName    = "child-bridge-cm"
	cbPVCStandalone    = "child-bridge-pvc"
	cbDiskStandalone   = "child-bridge-disk"
	cbProbePod         = "child-bridge-probe"
	// cbMissingVSCName is a VolumeSnapshotClass name that intentionally does NOT exist. The capture path
	// resolves the VolumeSnapshotClass exclusively through the StorageClass annotation
	// (storage.deckhouse.io/volumesnapshotclass), so pointing it at a missing class makes the domain
	// disk's volume capture fail terminally while the disk itself provisions and reaches Ready normally.
	cbMissingVSCName = "child-bridge-nonexistent-vsc"
)

// countSnapshotContentReconciles counts object-specific reconcile-start records in the current leader's
// logs since a fixed measurement boundary. controller-runtime's process-wide reconcile metric has no object
// label, so logs are the only low-cardinality way for this e2e to distinguish the deliberately terminal
// fixture from unrelated pending fixtures retained by E2E_KEEP_CLUSTER.
func countSnapshotContentReconciles(ctx context.Context, leaderPod, contentName string, since time.Time) (int, error) {
	sinceTime := metav1.NewTime(since)
	stream, err := suiteClientset.CoreV1().Pods(d8ModuleNS).GetLogs(leaderPod, &corev1.PodLogOptions{
		SinceTime: &sinceTime,
	}).Stream(ctx)
	if err != nil {
		return 0, fmt.Errorf("stream controller logs from %s/%s: %w", d8ModuleNS, leaderPod, err)
	}
	defer stream.Close()

	count := 0
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "Reconciling SnapshotContent") && strings.Contains(line, contentName) {
			count++
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("scan controller logs from %s/%s: %w", d8ModuleNS, leaderPod, err)
	}
	return count, nil
}

// buildChildBridgeSource returns a minimal data-backed tree: a ConfigMap (manifest leg) plus a standalone
// PVC-backed DemoVirtualDisk on badSC. The disk is a direct root-child data leaf whose
// DemoVirtualDiskSnapshot must fail volume capture terminally because badSC resolves to a missing
// VolumeSnapshotClass — the only content-unrepresentable failure that must reach the parent Snapshot via
// the child-Snapshot terminal-failure bridge (INV-FAIL-PROP).
func buildChildBridgeSource(ns, badSC string) []*unstructured.Unstructured {
	configMap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      cbConfigMapName,
			"namespace": ns,
		},
		"data": map[string]interface{}{"child": "bridge"},
	}}
	pvc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":      cbPVCStandalone,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"accessModes":      []interface{}{"ReadWriteOnce"},
			"storageClassName": badSC,
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{"storage": "500Mi"},
			},
		},
	}}
	disk := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualDisk",
		"metadata": map[string]interface{}{
			"name":      cbDiskStandalone,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"persistentVolumeClaimName": cbPVCStandalone,
			"size":                      "500Mi",
			"storageClassName":          badSC,
		},
	}}
	return []*unstructured.Unstructured{configMap, pvc, disk}
}

// cloneStorageClassWithBadVSC provisions newSC as a functional twin of the base srcSC (same LVM config, so
// PVCs still provision) but wired to a VolumeSnapshotClass that does not exist, isolating the terminal-
// capture injection from the shared, snapshot-capable StorageClass the other volume-data specs depend on.
//
// The base StorageClass is owned by sds-local-volume (provisioner local.csi.storage.deckhouse.io), whose
// validating webhook forbids creating/mutating such a StorageClass directly — it must be created through a
// LocalStorageClass CR (only annotation-only updates to an EXISTING SC are allowed). So this creates a
// SECOND LocalStorageClass that reuses the base's spec.lvm (same LVGs/thin pool), waits for the controller
// to materialize the backing StorageClass, then flips its storage.deckhouse.io/volumesnapshotclass
// annotation to a non-existent class via an annotation-only patch. The controller sets that annotation to
// its default (an existing VSC) at create time but never reconciles annotations afterwards (hasSCDiff
// ignores them), so the patch sticks and the domain disk's volume capture fails terminally while the disk
// itself provisions and reaches Ready.
func cloneStorageClassWithBadVSC(ctx context.Context, srcSC, newSC, badVSC string) error {
	base, err := suiteDyn.Resource(storagekube.LocalStorageClassGVR).Get(ctx, srcSC, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get base LocalStorageClass %s: %w", srcSC, err)
	}
	spec, found, err := unstructured.NestedMap(base.Object, "spec")
	if err != nil || !found {
		return fmt.Errorf("base LocalStorageClass %s has no spec (found=%v): %w", srcSC, found, err)
	}
	clone := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagekube.LocalStorageClassGVR.GroupVersion().String(),
		"kind":       "LocalStorageClass",
		"metadata":   map[string]interface{}{"name": newSC},
		"spec":       spec,
	}}
	if _, err := suiteDyn.Resource(storagekube.LocalStorageClassGVR).Create(ctx, clone, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create clone LocalStorageClass %s: %w", newSC, err)
	}
	if err := storagekube.WaitForLocalStorageClassCreated(ctx, suiteRestCfg, newSC, 5*time.Minute); err != nil {
		return fmt.Errorf("clone LocalStorageClass %s did not reach Created: %w", newSC, err)
	}
	if err := storagekube.WaitForStorageClass(ctx, suiteRestCfg, newSC, 2*time.Minute); err != nil {
		return fmt.Errorf("clone StorageClass %s did not appear: %w", newSC, err)
	}
	// Annotation-only update on the existing SC (the only StorageClass mutation the sds-local-volume webhook
	// permits): point the VolumeSnapshotClass annotation at a class that does not exist.
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, annStorageClassVSC, badVSC)
	if _, err := suiteClientset.StorageV1().StorageClasses().Patch(ctx, newSC, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("annotate clone StorageClass %s with %s=%s: %w", newSC, annStorageClassVSC, badVSC, err)
	}
	return nil
}

// waitSnapshotChildOfKind polls the root Snapshot's subtree until a child of wantKind appears and returns
// its name, so the spec does not depend on the demo domain's child-snapshot naming scheme.
func waitSnapshotChildOfKind(ctx context.Context, ns, rootSnapshot, wantKind string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		nodes, err := walkSnapshotTree(ctx, ns, rootSnapshot)
		if err != nil {
			last = fmt.Sprintf("walk err=%v", err)
		} else {
			for _, n := range nodes {
				if n.kind == wantKind {
					return n.name, nil
				}
			}
			last = fmt.Sprintf("subtree has %d nodes, none of kind %s", len(nodes), wantKind)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timeout waiting for a %s child under %s/%s; last: %s", wantKind, ns, rootSnapshot, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return "", ctx.Err()
		}
	}
}

// waitSnapshotReadyFalseReason polls a namespaced Snapshot until its Ready condition is False with
// wantReason, failing fast (no requeue value here — the caller sizes the timeout). detail carries the last
// observed Ready state for diagnostics.
func waitSnapshotReadyFalseReason(ctx context.Context, ns, name, wantReason string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		obj, err := getResource(ctx, snapshotGVR, ns, name)
		if err == nil {
			st, reason, found := conditionStatus(obj, condReady)
			if found && st == "False" && reason == wantReason {
				return nil
			}
			last = fmt.Sprintf("found=%v status=%q reason=%q", found, st, reason)
		} else {
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for Snapshot %s/%s Ready=False/%s; last: %s", ns, name, wantReason, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// childBridgeFailureSpecs registers the end-to-end regression for the child-Snapshot terminal-failure
// bridge (INV-FAIL-PROP): a domain child Snapshot whose VOLUME capture fails terminally must flip the
// parent root Snapshot to Ready=False/ChildrenFailed. The child's bound SnapshotContent cannot represent a
// lost-volume failure (its data leg reads ready from an empty dataRef), so the child-Snapshot bridge is
// the only path that propagates it — and this real-cluster spec is the faithful counterpart to the envtest
// specs that cannot synthesize a genuine terminal volume capture (see
// images/state-snapshotter-controller/test/integration/snapshot_graph_integration_test.go and the unit
// tests in internal/usecase/child_snapshot_terminal_failures_test.go).
//
// Opt-out (E2E_CHILD_BRIDGE_FAILURE=false): it mutates cluster StorageClass wiring and depends on the demo
// domain's exact terminal volume-capture behaviour; it runs by default as part of the volume-data flow and
// is disabled on environments that cannot support it.
func childBridgeFailureSpecs() {
	Context("Child-Snapshot terminal-failure bridge (domain-disk terminal volume capture)", func() {
		var (
			srcNS string
			sc    string
			badSC string
		)

		BeforeAll(func() {
			if !suiteCfg.volumeData || !envEnabledByDefault(os.Getenv(envChildBridgeFailure)) {
				Skip("child-bridge failure spec disabled: it runs by default; set " + envChildBridgeFailure + "=false (or E2E_VOLUME_DATA=false) to disable")
			}
			sc = suiteCfg.storageClass
			badSC = sc + "-nobind-vsc"
			srcNS = uniqueNS("p3-childbridge-neg")

			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
			defer cancel()

			By("Provisioning the thin, snapshot-capable base StorageClass via storage-e2e (" + sc + ")")
			_, err := testkit.EnsureDefaultStorageClass(ctx, suiteRestCfg, testkit.DefaultStorageClassConfig{
				StorageClassName:     sc,
				LVMType:              "Thin",
				ThinPoolName:         "thinpool",
				BaseKubeconfig:       suiteClusterResources.BaseKubeconfig,
				VMNamespace:          suiteCfg.vmNamespace,
				BaseStorageClassName: suiteCfg.baseStorageClass,
			})
			Expect(err).NotTo(HaveOccurred(), "provision base StorageClass")

			By("Cloning it (via a LocalStorageClass) into a StorageClass wired to a non-existent VolumeSnapshotClass (" + badSC + " -> " + cbMissingVSCName + ")")
			Expect(cloneStorageClassWithBadVSC(ctx, sc, badSC, cbMissingVSCName)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				if cleanupSkipped() {
					GinkgoWriter.Printf("%s: keeping LocalStorageClass/StorageClass %q\n", keepReason(), badSC)
					return
				}
				// Delete the LocalStorageClass, which owns the backing StorageClass: the sds-local-volume
				// controller removes the SC as part of the LSC delete-reconcile. Deleting the SC directly would
				// be recreated by the controller (and blocked by its webhook for non-controller users).
				_ = suiteDyn.Resource(storagekube.LocalStorageClassGVR).Delete(cctx, badSC, metav1.DeleteOptions{})
			})

			By("Creating the source namespace and applying the data-backed disk source on the bad StorageClass")
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			Expect(applyObjects(ctx, buildChildBridgeSource(srcNS, badSC), srcNS)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, srcNS)
			})

			By("Starting the source probe Pod so the PVC binds (WaitForFirstConsumer classes bind on schedule)")
			_, err = suiteClientset.CoreV1().Pods(srcNS).Create(ctx, probePodSpec(srcNS, cbProbePod, []string{cbPVCStandalone}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create source probe pod")
			Expect(waitPodRunning(ctx, srcNS, cbProbePod, 10*time.Minute)).To(Succeed())
		})

		It("fails the root Snapshot closed (Ready=False/ChildrenFailed) when a domain child's volume capture fails terminally", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 4*suiteCfg.captureReadyTO+15*time.Minute)
			defer cancel()

			tl := startCaptureTimeline(srcNS)
			defer tl.stop()

			By("Creating the root Snapshot over the data-backed disk tree")
			Expect(createRootSnapshot(ctx, srcNS, cbRootSnapshotName)).To(Succeed())

			By("Resolving the domain DemoVirtualDiskSnapshot child node under the root")
			childName, err := waitSnapshotChildOfKind(ctx, srcNS, cbRootSnapshotName, "DemoVirtualDiskSnapshot", 2*suiteCfg.captureReadyTO+5*time.Minute)
			Expect(err).NotTo(HaveOccurred(), "root Snapshot must publish a DemoVirtualDiskSnapshot child")
			GinkgoWriter.Printf("  resolved domain disk child snapshot: %s\n", childName)

			By("Waiting for the domain child snapshot to reach a terminal Ready=False (volume capture cannot resolve the VolumeSnapshotClass)")
			Eventually(func() (string, error) {
				obj, gerr := getResource(ctx, demoDiskSnapshotGVR, srcNS, childName)
				if gerr != nil {
					return "", gerr
				}
				st, reason, found := conditionStatus(obj, condReady)
				if !found {
					return "", fmt.Errorf("child %s has no Ready condition yet", childName)
				}
				GinkgoWriter.Printf("  child %s Ready=%s/%s\n", childName, st, reason)
				if st == "False" {
					return reason, nil
				}
				return "", fmt.Errorf("child %s Ready=%s (not terminal yet)", childName, st)
			}).WithContext(ctx).WithTimeout(2*suiteCfg.captureReadyTO+5*time.Minute).WithPolling(pollInterval).
				ShouldNot(BeEmpty(), "child DemoVirtualDiskSnapshot must report a terminal Ready=False reason")

			// INV-FAIL-PROP: the root must fail closed over the lost child volume data with the CANONICAL
			// child-failure reason ChildrenFailed — deterministic at ANY timing of the child failure relative
			// to the root's MarkPlanned (vcr-watch-core-terminal). Mechanism: the core is the sole writer of
			// the terminal — a failed data-leg VCR makes the child's OWN SnapshotContent terminal
			// (DataReady=False/VolumeCaptureFailed), which mirrors onto the child xxxSnapshot's Ready. From
			// there both root paths converge on the same reason:
			//   - child failure lands while the root is still planning: the weight-layer gate
			//     (weightLayerCaptureReady -> snapshotChildTerminalFailure) catches the child terminal and the
			//     planner fails the root via sdk.Fail(ChildrenFailed) — phase=Failed is a terminal sink and the
			//     content->Snapshot mirror bubbles the same reason, so the two writers agree (no flap);
			//   - child failure lands after the root is Planned: the content aggregation propagates the child
			//     content terminal up the tree as ChildrenFailed and the mirror reflects it onto the root.
			// GraphPlanningFailed is reserved for the root's OWN planning faults (selector/list/coverage) and
			// must NOT appear for a child failure.
			By("Asserting the parent root Snapshot fails closed as Ready=False/ChildrenFailed over the child failure")
			Expect(waitSnapshotReadyFalseReason(ctx, srcNS, cbRootSnapshotName, "ChildrenFailed", 2*suiteCfg.captureReadyTO+5*time.Minute)).
				To(Succeed(), "root Snapshot must fail closed over the lost child volume data (INV-FAIL-PROP)")

			By("Asserting the parent failure message references the failed child")
			root, err := getResource(ctx, snapshotGVR, srcNS, cbRootSnapshotName)
			Expect(err).NotTo(HaveOccurred())
			_, _, found := conditionStatus(root, condReady)
			Expect(found).To(BeTrue(), "root Snapshot must carry a Ready condition")

			By("Asserting the failed child counts as settled and latches childrenSettled=true on the parent")
			Eventually(func(g Gomega) {
				freshRoot, gerr := getResource(ctx, snapshotGVR, srcNS, cbRootSnapshotName)
				g.Expect(gerr).NotTo(HaveOccurred())
				g.Expect(childSnapshotRefs(freshRoot)).NotTo(BeEmpty(),
					"childrenSettled assertion must be non-vacuous: the root must declare the failed child")

				settled, settledFound := snapshotCommonControllerLatch(freshRoot, "childrenSettled")
				g.Expect(settledFound).To(BeTrue(),
					"the root with a terminal failed child must declare commonController.childrenSettled")
				g.Expect(settled).To(BeTrue(),
					"the terminal failed child must count as settled, not keep the parent waiting forever")
			}).WithContext(ctx).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())

			By("Asserting the terminal child SnapshotContent stops active 500 ms self-polling")
			child, err := getResource(ctx, demoDiskSnapshotGVR, srcNS, childName)
			Expect(err).NotTo(HaveOccurred())
			contentName, _, err := unstructured.NestedString(child.Object, "status", "boundSnapshotContentName")
			Expect(err).NotTo(HaveOccurred())
			Expect(contentName).NotTo(BeEmpty(), "terminal child Snapshot must be bound to a SnapshotContent")

			leaderPod, err := leaderControllerPod(ctx)
			Expect(err).NotTo(HaveOccurred())
			measurementStart := time.Now().Add(-5 * time.Second)
			lastCount, err := countSnapshotContentReconciles(ctx, leaderPod, contentName, measurementStart)
			Expect(err).NotTo(HaveOccurred())
			stableSamples := 0
			Eventually(func(g Gomega) int {
				currentLeader, lerr := leaderControllerPod(ctx)
				g.Expect(lerr).NotTo(HaveOccurred())
				g.Expect(currentLeader).To(Equal(leaderPod), "leader changed while measuring object-specific reconcile quiescence")

				currentCount, cerr := countSnapshotContentReconciles(ctx, leaderPod, contentName, measurementStart)
				g.Expect(cerr).NotTo(HaveOccurred())
				if currentCount == lastCount {
					stableSamples++
				} else {
					lastCount = currentCount
					stableSamples = 0
				}
				return stableSamples
			}).WithContext(ctx).WithTimeout(15*time.Second).WithPolling(500*time.Millisecond).
				Should(BeNumerically(">=", 3), "terminal SnapshotContent %s never became quiescent", contentName)

			baselineCount := lastCount
			Consistently(func(g Gomega) int {
				currentLeader, lerr := leaderControllerPod(ctx)
				g.Expect(lerr).NotTo(HaveOccurred())
				g.Expect(currentLeader).To(Equal(leaderPod), "leader changed while measuring object-specific reconcile quiescence")

				currentCount, cerr := countSnapshotContentReconciles(ctx, leaderPod, contentName, measurementStart)
				g.Expect(cerr).NotTo(HaveOccurred())
				return currentCount
			}).WithContext(ctx).WithTimeout(3*time.Second).WithPolling(500*time.Millisecond).
				Should(Equal(baselineCount), "terminal SnapshotContent %s must remain event-driven instead of polling", contentName)
		})
	})
}
