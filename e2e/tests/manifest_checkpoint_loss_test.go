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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// envManifestCheckpointLoss opts this spec OUT. It runs by default (as part of the phase-3 volume-data flow)
// because it DELETES cluster-scoped ManifestCheckpoint/chunk artifacts and depends on the exact terminal
// ManifestCheckpointFailed classification + its ChildrenFailed propagation; like the child-bridge and
// freeze-deadline negative specs, an environment that cannot support it disables the spec with
// E2E_MANIFEST_CHECKPOINT_LOSS=false (and the whole flow with E2E_VOLUME_DATA=false).
const envManifestCheckpointLoss = "E2E_MANIFEST_CHECKPOINT_LOSS"

// Manifest-checkpoint-loss fixture object names (isolated from the phase-3 vol-tree names so this spec can
// run in the same suite without colliding on the shared source fixtures). The VM wires a PVC-backed disk so
// the captured tree is exactly three levels deep: root ns Snapshot -> DemoVirtualMachineSnapshot ->
// DemoVirtualDiskSnapshot, which is what lets the three delete cases target a root, a child, and a
// grandchild ManifestCheckpoint respectively.
const (
	mcpLossRootSnapshotName = "mcp-loss-tree"
	mcpLossConfigMapName    = "mcp-loss-cm"
	mcpLossPVCName          = "mcp-loss-pvc"
	mcpLossVMName           = "mcp-loss-vm"
	mcpLossDiskName         = "mcp-loss-disk"
	mcpLossProbePod         = "mcp-loss-probe"
)

// buildManifestCheckpointLossSource returns a minimal three-level data-backed tree: a ConfigMap (the
// root's own manifest leg), one PVC on the snapshot-capable StorageClass, a DemoVirtualMachine wiring that
// disk, and the PVC-backed DemoVirtualDisk it adopts. The VM->disk wiring makes the DemoVirtualDiskSnapshot
// a grandchild of the root under the DemoVirtualMachineSnapshot, so the tree has a distinct root, child and
// grandchild ManifestCheckpoint to delete.
func buildManifestCheckpointLossSource(ns, sc string) []*unstructured.Unstructured {
	configMap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      mcpLossConfigMapName,
			"namespace": ns,
		},
		"data": map[string]interface{}{"mcp": "loss"},
	}}
	pvc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":      mcpLossPVCName,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"accessModes":      []interface{}{"ReadWriteOnce"},
			"storageClassName": sc,
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{"storage": "500Mi"},
			},
		},
	}}
	vm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualMachine",
		"metadata": map[string]interface{}{
			"name":      mcpLossVMName,
			"namespace": ns,
		},
		"spec": map[string]interface{}{"virtualDiskName": mcpLossDiskName},
	}}
	disk := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualDisk",
		"metadata": map[string]interface{}{
			"name":      mcpLossDiskName,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"persistentVolumeClaimName": mcpLossPVCName,
			"size":                      "500Mi",
			"storageClassName":          sc,
		},
	}}
	return []*unstructured.Unstructured{configMap, pvc, vm, disk}
}

// nodeManifestCheckpointName resolves a snapshot node (root Snapshot or a demo child snapshot) to the
// ManifestCheckpoint its bound SnapshotContent published (status.boundSnapshotContentName ->
// content.status.manifestCheckpointName). It is the target of the delete cases: removing this MCP is the
// post-capture manifest-artifact loss under test.
func nodeManifestCheckpointName(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (string, error) {
	snap, err := getResource(ctx, gvr, ns, name)
	if err != nil {
		return "", err
	}
	contentName, _, _ := unstructured.NestedString(snap.Object, "status", "boundSnapshotContentName")
	if contentName == "" {
		return "", fmt.Errorf("node %s %s/%s has no bound content yet", gvr.Resource, ns, name)
	}
	content, err := getResource(ctx, snapshotContentGVR, "", contentName)
	if err != nil {
		return "", err
	}
	mcpName, _, _ := unstructured.NestedString(content.Object, "status", "manifestCheckpointName")
	if mcpName == "" {
		return "", fmt.Errorf("content %s (node %s/%s) has no manifestCheckpointName yet", contentName, ns, name)
	}
	return mcpName, nil
}

// firstManifestCheckpointChunkName returns the name of the first chunk in the MCP's status.chunks[]. The
// chunk-integrity case deletes it to corrupt a still-present checkpoint.
func firstManifestCheckpointChunkName(ctx context.Context, mcpName string) (string, error) {
	mcp, err := getResource(ctx, manifestCheckpointGVR, "", mcpName)
	if err != nil {
		return "", err
	}
	chunks, _, _ := unstructured.NestedSlice(mcp.Object, "status", "chunks")
	for _, c := range chunks {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if n, _, _ := unstructured.NestedString(m, "name"); n != "" {
			return n, nil
		}
	}
	return "", fmt.Errorf("ManifestCheckpoint %s has no chunks in status.chunks[]", mcpName)
}

// bumpManifestCheckpoint patches a throwaway annotation onto the MCP to generate an update event. Chunk
// deletion does NOT self-wake the owning content (no chunk watch by design); this bump wakes it via the MCP
// ownerRef watch so the next reconcile re-runs the chunk-existence check and flips the leg terminal.
func bumpManifestCheckpoint(ctx context.Context, mcpName string) error {
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`,
		"e2e.state-snapshotter.deckhouse.io/chunk-loss-bump", time.Now().Format(time.RFC3339Nano)))
	_, err := suiteDyn.Resource(manifestCheckpointGVR).Patch(ctx, mcpName, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// waitNodeReadyFalseReason polls any snapshot tree node (root Snapshot or a demo child snapshot) until its
// Ready condition is False with wantReason. It is the generic counterpart of waitSnapshotReadyFalseReason
// (which is pinned to the root snapshotGVR).
func waitNodeReadyFalseReason(ctx context.Context, gvr schema.GroupVersionResource, ns, name, wantReason string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		obj, err := getResource(ctx, gvr, ns, name)
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
			return fmt.Errorf("timeout waiting for %s %s/%s Ready=False/%s; last: %s", gvr.Resource, ns, name, wantReason, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// manifestCheckpointLossSpecs registers the manifest-leg counterpart of the child-bridge failure regression
// (INV-FAIL-PROP): a manifest artifact (ManifestCheckpoint) that is LOST after capture — the whole MCP CR
// deleted, or one of its content chunks deleted — must flip the owning node's Snapshot to the terminal
// Ready=False/ManifestCheckpointFailed and propagate up the tree as Ready=False/ChildrenFailed on the root.
//
// Deleting the whole MCP after capture is treated as a terminal loss (symmetric with a missing chunk): once
// the owning snapshot's commonController.manifestCaptured latch is set, the publish-before-create window is
// closed, so a genuinely-absent published MCP is ManifestCheckpointFailed (not the non-terminal
// ManifestCapturePending). See the SnapshotContentController post-capture integrity check.
//
// Opt-out (E2E_MANIFEST_CHECKPOINT_LOSS=false): it deletes cluster-scoped artifacts and depends on the exact
// terminal classification + propagation; it runs by default as part of the volume-data flow and is disabled
// on environments that cannot support it.
func manifestCheckpointLossSpecs() {
	Context("Manifest checkpoint loss fails the tree closed (INV-FAIL-PROP, manifest leg)", func() {
		var sc string

		BeforeAll(func() {
			if !suiteCfg.volumeData || !envEnabledByDefault(os.Getenv(envManifestCheckpointLoss)) {
				Skip("manifest-checkpoint-loss spec disabled: it runs by default; set " + envManifestCheckpointLoss + "=false (or E2E_VOLUME_DATA=false) to disable")
			}
			sc = suiteCfg.storageClass

			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
			defer cancel()

			By("Provisioning a thin, snapshot-capable StorageClass via storage-e2e (" + sc + ")")
			_, err := testkit.EnsureDefaultStorageClass(ctx, suiteRestCfg, testkit.DefaultStorageClassConfig{
				StorageClassName:     sc,
				LVMType:              "Thin",
				ThinPoolName:         "thinpool",
				BaseKubeconfig:       suiteClusterResources.BaseKubeconfig,
				VMNamespace:          suiteCfg.vmNamespace,
				BaseStorageClassName: suiteCfg.baseStorageClass,
			})
			Expect(err).NotTo(HaveOccurred(), "provision StorageClass")

			By("Wiring the StorageClass to a VolumeSnapshotClass for the local CSI driver")
			Expect(ensureStorageClassVolumeSnapshotClass(ctx, sc)).To(Succeed())
		})

		// captureFreshDataTree provisions a fresh, fully-Ready three-level data tree in its own namespace and
		// returns that namespace. Each delete case needs a pristine Ready tree (MCP loss is terminal and does
		// not self-heal), so this cannot be shared across cases. caseSuffix keeps each case's namespace
		// distinct within one run (p3-mcploss-<case>-neg) — without a per-call random suffix, reusing one role
		// across the four cases would collide on the still-terminating namespace of the previous case. The
		// namespace is reaped by DeferCleanup.
		captureFreshDataTree := func(ctx context.Context, caseSuffix string) string {
			GinkgoHelper()
			ns := uniqueNS("p3-mcploss-" + caseSuffix + "-neg")

			By("Creating the source namespace " + ns + " and applying the VM+disk source")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			Expect(applyObjects(ctx, buildManifestCheckpointLossSource(ns, sc), ns)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, ns)
			})

			By("Starting the probe Pod so the PVC binds (WaitForFirstConsumer)")
			_, err := suiteClientset.CoreV1().Pods(ns).Create(ctx, probePodSpec(ns, mcpLossProbePod, []string{mcpLossPVCName}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create probe pod")
			Expect(waitPodRunning(ctx, ns, mcpLossProbePod, 10*time.Minute)).To(Succeed())

			By("Creating the root Snapshot and waiting for the whole tree to reach Ready")
			Expect(createRootSnapshot(ctx, ns, mcpLossRootSnapshotName)).To(Succeed())
			content, err := waitSnapshotReady(ctx, ns, mcpLossRootSnapshotName, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred(), "root Snapshot must reach Ready before the MCP is deleted")
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())

			By("Asserting the demo child + grandchild snapshot nodes are Ready before injection")
			var nodes []childRef
			Eventually(func(g Gomega) {
				var werr error
				nodes, werr = walkSnapshotTree(ctx, ns, mcpLossRootSnapshotName)
				g.Expect(werr).NotTo(HaveOccurred())
				kinds := map[string]int{}
				for _, n := range nodes {
					kinds[n.kind]++
				}
				g.Expect(kinds["DemoVirtualMachineSnapshot"]).To(BeNumerically(">=", 1), "expected a DemoVirtualMachineSnapshot child")
				g.Expect(kinds["DemoVirtualDiskSnapshot"]).To(BeNumerically(">=", 1), "expected a DemoVirtualDiskSnapshot grandchild")
			}).WithContext(ctx).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())
			Expect(waitChildrenReady(ctx, ns, nodes, suiteCfg.captureReadyTO)).To(Succeed())

			return ns
		}

		It("root ns ManifestCheckpoint deleted -> root Snapshot Ready=False/ManifestCheckpointFailed", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+20*time.Minute)
			defer cancel()

			ns := captureFreshDataTree(ctx, "root")

			By("Resolving and deleting the root Snapshot's ManifestCheckpoint")
			mcpName, err := nodeManifestCheckpointName(ctx, snapshotGVR, ns, mcpLossRootSnapshotName)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("  deleting root MCP: %s\n", mcpName)
			Expect(suiteDyn.Resource(manifestCheckpointGVR).Delete(ctx, mcpName, metav1.DeleteOptions{})).To(Succeed())

			By("Asserting the root Snapshot fails closed on its OWN manifest leg (Ready=False/ManifestCheckpointFailed)")
			Expect(waitNodeReadyFalseReason(ctx, snapshotGVR, ns, mcpLossRootSnapshotName, "ManifestCheckpointFailed", 2*suiteCfg.captureReadyTO+5*time.Minute)).
				To(Succeed(), "deleting the root's own published ManifestCheckpoint after capture must be terminal, not pending")
		})

		It("child DemoVirtualMachineSnapshot ManifestCheckpoint deleted -> child terminal, root Ready=False/ChildrenFailed", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+20*time.Minute)
			defer cancel()

			ns := captureFreshDataTree(ctx, "child")

			By("Resolving the DemoVirtualMachineSnapshot child node")
			vmSnap, err := waitSnapshotChildOfKind(ctx, ns, mcpLossRootSnapshotName, "DemoVirtualMachineSnapshot", suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())

			By("Resolving and deleting the child's ManifestCheckpoint")
			mcpName, err := nodeManifestCheckpointName(ctx, demoVMSnapshotGVR, ns, vmSnap)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("  deleting child VM MCP: %s (node %s)\n", mcpName, vmSnap)
			Expect(suiteDyn.Resource(manifestCheckpointGVR).Delete(ctx, mcpName, metav1.DeleteOptions{})).To(Succeed())

			By("Asserting the child DemoVirtualMachineSnapshot fails closed (Ready=False/ManifestCheckpointFailed)")
			Expect(waitNodeReadyFalseReason(ctx, demoVMSnapshotGVR, ns, vmSnap, "ManifestCheckpointFailed", 2*suiteCfg.captureReadyTO+5*time.Minute)).
				To(Succeed(), "the child node must go terminal on its own lost manifest artifact")

			By("Asserting the failure propagates up: root Snapshot Ready=False/ChildrenFailed")
			Expect(waitSnapshotReadyFalseReason(ctx, ns, mcpLossRootSnapshotName, "ChildrenFailed", 2*suiteCfg.captureReadyTO+5*time.Minute)).
				To(Succeed(), "a terminal child manifest failure must fail the root closed (INV-FAIL-PROP)")
		})

		It("grandchild DemoVirtualDiskSnapshot ManifestCheckpoint deleted -> grandchild terminal, root Ready=False/ChildrenFailed", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+20*time.Minute)
			defer cancel()

			ns := captureFreshDataTree(ctx, "disk")

			By("Resolving the DemoVirtualDiskSnapshot grandchild node")
			diskSnap, err := waitSnapshotChildOfKind(ctx, ns, mcpLossRootSnapshotName, "DemoVirtualDiskSnapshot", suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())

			By("Resolving and deleting the grandchild's ManifestCheckpoint")
			mcpName, err := nodeManifestCheckpointName(ctx, demoDiskSnapshotGVR, ns, diskSnap)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("  deleting grandchild disk MCP: %s (node %s)\n", mcpName, diskSnap)
			Expect(suiteDyn.Resource(manifestCheckpointGVR).Delete(ctx, mcpName, metav1.DeleteOptions{})).To(Succeed())

			By("Asserting the grandchild DemoVirtualDiskSnapshot fails closed (Ready=False/ManifestCheckpointFailed)")
			Expect(waitNodeReadyFalseReason(ctx, demoDiskSnapshotGVR, ns, diskSnap, "ManifestCheckpointFailed", 2*suiteCfg.captureReadyTO+5*time.Minute)).
				To(Succeed(), "the grandchild node must go terminal on its own lost manifest artifact")

			By("Asserting the failure propagates all the way up: root Snapshot Ready=False/ChildrenFailed")
			Expect(waitSnapshotReadyFalseReason(ctx, ns, mcpLossRootSnapshotName, "ChildrenFailed", 2*suiteCfg.captureReadyTO+5*time.Minute)).
				To(Succeed(), "a terminal grandchild manifest failure must fail the root closed (INV-FAIL-PROP)")
		})

		It("grandchild ManifestCheckpoint chunk deleted -> grandchild terminal, root Ready=False/ChildrenFailed", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+20*time.Minute)
			defer cancel()

			ns := captureFreshDataTree(ctx, "chunk")

			By("Resolving the DemoVirtualDiskSnapshot grandchild node and its ManifestCheckpoint")
			diskSnap, err := waitSnapshotChildOfKind(ctx, ns, mcpLossRootSnapshotName, "DemoVirtualDiskSnapshot", suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			mcpName, err := nodeManifestCheckpointName(ctx, demoDiskSnapshotGVR, ns, diskSnap)
			Expect(err).NotTo(HaveOccurred())

			By("Deleting one content chunk of the still-present ManifestCheckpoint, then bumping the MCP to wake reconcile")
			chunkName, err := firstManifestCheckpointChunkName(ctx, mcpName)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("  deleting chunk %s of MCP %s (node %s)\n", chunkName, mcpName, diskSnap)
			Expect(suiteDyn.Resource(manifestCheckpointContentChunkGVR).Delete(ctx, chunkName, metav1.DeleteOptions{})).To(Succeed())
			Expect(bumpManifestCheckpoint(ctx, mcpName)).To(Succeed())

			By("Asserting the grandchild fails closed on the missing chunk (Ready=False/ManifestCheckpointFailed)")
			Expect(waitNodeReadyFalseReason(ctx, demoDiskSnapshotGVR, ns, diskSnap, "ManifestCheckpointFailed", 2*suiteCfg.captureReadyTO+5*time.Minute)).
				To(Succeed(), "a missing chunk is a terminal integrity loss of the checkpoint")

			By("Asserting the failure propagates up: root Snapshot Ready=False/ChildrenFailed")
			Expect(waitSnapshotReadyFalseReason(ctx, ns, mcpLossRootSnapshotName, "ChildrenFailed", 2*suiteCfg.captureReadyTO+5*time.Minute)).
				To(Succeed(), "a terminal grandchild chunk failure must fail the root closed (INV-FAIL-PROP)")
		})
	})
}
