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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/deckhouse/state-snapshotter/api/names"
	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

const (
	// vdGcRootSnapshotName is a dedicated root for the GC teardown flow (it deletes its source namespace and
	// then its ObjectKeeper, so it must not share the phase-3 capture tree).
	vdGcRootSnapshotName = "vol-tree-gc"

	// vdGcLargeTTL is an explicitly large snapshotTtlAfterDelete so the durable content tree survives
	// source-namespace deletion for the whole spec window (the ObjectKeeper TTL countdown must not reclaim it
	// out from under Spec 1); the teardown in Spec 2 is driven by deleting the ObjectKeeper directly, not by
	// waiting out the TTL.
	vdGcLargeTTL = "720h"

	// parentProtectFinalizer is the state-snapshotter finalizer every durable SnapshotContent must keep until
	// its own teardown handler completes (the invariant this flow proves end-to-end).
	parentProtectFinalizer = "state-snapshotter.deckhouse.io/parent-protect"

	// vscBoundProtectionFinalizer is the CSI external-snapshotter finalizer on a bound VolumeSnapshotContent;
	// the sidecar removes it only once the physical snapshot is reclaimed.
	vscBoundProtectionFinalizer = "snapshot.storage.kubernetes.io/volumesnapshotcontent-bound-protection"

	// volumeSnapshotBeingDeletedAnnotation is the CSI deletion gate on a bound Delete-policy
	// VolumeSnapshotContent. Spec 2 strips it from the wired-ref VSCs to simulate the lost lifecycle-window
	// stamp; the fix re-stamps it from the SnapshotContent teardown so the reclaim still completes.
	volumeSnapshotBeingDeletedAnnotation = "snapshot.storage.kubernetes.io/volumesnapshot-being-deleted"

	// llvsCSIFinalizer is the sds-local-volume CSI finalizer on a physical LVMLogicalVolumeSnapshot.
	llvsCSIFinalizer = "storage.deckhouse.io/sds-local-volume-csi"
)

// gcVSCRecord is a recorded durable VolumeSnapshotContent data artifact plus the classification the
// teardown flow needs: whether it is bi-directionally bound to a namespaced VolumeSnapshot (wired-ref, the
// leaking population) or unbound (VSC-only, the VCR-leg population), and, when wired, the exact bound
// VolumeSnapshot identity whose deletion is what could have stamped/lost the being-deleted annotation.
type gcVSCRecord struct {
	vsc            string
	snapshotHandle string // -> LVMLogicalVolumeSnapshot name (llvs)
	wired          bool
	vsNamespace    string
	vsName         string
	vsUID          string
}

// gcChunkRecord is a recorded ManifestCheckpoint content chunk plus its owning MCP (for the ownerRef check).
type gcChunkRecord struct {
	name string
	mcp  string
}

// gcTree is the recorded identity of every durable artifact in the captured content tree, gathered while
// the source namespace is still alive so the teardown assertions can address each object by name afterwards.
type gcTree struct {
	root     string
	okName   string
	contents []string          // BFS order; contents[0] == root
	parentOf map[string]string // child content -> parent content ("" for the root)
	mcps     []string
	chunks   []gcChunkRecord
	vscs     []gcVSCRecord
}

// controllerOwnerRef returns the controller (controller==true) ownerReference kind/name of obj.
func controllerOwnerRef(obj *unstructured.Unstructured) (kind, name string, found bool) {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			return ref.Kind, ref.Name, true
		}
	}
	return "", "", false
}

// recordGcTree BFS-walks the cluster-scoped SnapshotContent tree from root (via childContentNames — NOT the
// namespaced snapshot walk) and records every content, its parent edge, its ManifestCheckpoint + chunks, and
// its durable VolumeSnapshotContent data artifact (with wired-ref/VSC-only classification and bound-VS
// identity). Requires at least one wired-ref VSC (the leaking population under test).
func recordGcTree(ctx context.Context, root, okName string) (gcTree, error) {
	tree := gcTree{root: root, okName: okName, parentOf: map[string]string{root: ""}}
	seen := map[string]bool{}
	queue := []string{root}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		tree.contents = append(tree.contents, name)

		co, err := getResource(ctx, snapshotContentGVR, "", name)
		if err != nil {
			return tree, fmt.Errorf("get SnapshotContent %s: %w", name, err)
		}

		if mcpName, _, _ := unstructured.NestedString(co.Object, "status", "manifestCheckpointName"); mcpName != "" {
			tree.mcps = append(tree.mcps, mcpName)
			mcp, err := getResource(ctx, manifestCheckpointGVR, "", mcpName)
			if err != nil {
				return tree, fmt.Errorf("get ManifestCheckpoint %s: %w", mcpName, err)
			}
			chunks, _, _ := unstructured.NestedSlice(mcp.Object, "status", "chunks")
			for _, c := range chunks {
				if m, ok := c.(map[string]interface{}); ok {
					if cn, _, _ := unstructured.NestedString(m, "name"); cn != "" {
						tree.chunks = append(tree.chunks, gcChunkRecord{name: cn, mcp: mcpName})
					}
				}
			}
		}

		artifactKind, _, _ := unstructured.NestedString(co.Object, "status", "data", "artifactRef", "kind")
		vscName, _, _ := unstructured.NestedString(co.Object, "status", "data", "artifactRef", "name")
		if artifactKind == "VolumeSnapshotContent" && vscName != "" {
			vsc, err := getResource(ctx, volumeSnapshotContentGVR, "", vscName)
			if err != nil {
				return tree, fmt.Errorf("get VolumeSnapshotContent %s: %w", vscName, err)
			}
			handle, _, _ := unstructured.NestedString(vsc.Object, "status", "snapshotHandle")
			vsNS, _, _ := unstructured.NestedString(vsc.Object, "spec", "volumeSnapshotRef", "namespace")
			vsName, _, _ := unstructured.NestedString(vsc.Object, "spec", "volumeSnapshotRef", "name")
			vsUID, _, _ := unstructured.NestedString(vsc.Object, "spec", "volumeSnapshotRef", "uid")
			tree.vscs = append(tree.vscs, gcVSCRecord{
				vsc:            vscName,
				snapshotHandle: handle,
				wired:          vsUID != "",
				vsNamespace:    vsNS,
				vsName:         vsName,
				vsUID:          vsUID,
			})
		}

		for _, child := range childContentNames(co) {
			if _, ok := tree.parentOf[child]; !ok {
				tree.parentOf[child] = name
			}
			queue = append(queue, child)
		}
	}

	wired := 0
	for _, v := range tree.vscs {
		if v.wired {
			wired++
		}
	}
	if wired == 0 {
		return tree, fmt.Errorf("expected at least one wired-ref VolumeSnapshotContent (the orphan-PVC native-VS leg); recorded %d VSCs, none wired", len(tree.vscs))
	}
	return tree, nil
}

// assertTreeAlive asserts every recorded cluster-scoped artifact is alive and healthy (deletionTimestamp
// nil), matching the durable-content contract while the source namespace is gone.
func assertTreeAlive(ctx context.Context, g Gomega, tree gcTree) {
	// ObjectKeeper still holds the tree.
	ok, err := getResource(ctx, objectKeeperGVR, "", tree.okName)
	g.Expect(err).NotTo(HaveOccurred(), "root ObjectKeeper %s must exist", tree.okName)
	g.Expect(ok.GetDeletionTimestamp()).To(BeNil(), "root ObjectKeeper %s must not be deleting", tree.okName)

	// Every SnapshotContent: 4 leg conditions True, parent-protect retained, correct controller ownerRef.
	for _, name := range tree.contents {
		co, err := getResource(ctx, snapshotContentGVR, "", name)
		g.Expect(err).NotTo(HaveOccurred(), "SnapshotContent %s must survive source-namespace deletion", name)
		g.Expect(co.GetDeletionTimestamp()).To(BeNil(), "SnapshotContent %s must not be deleting", name)
		for _, ct := range []string{condManifestsReady, condDataReady, condChildrenReady, condReady} {
			st, _, found := conditionStatus(co, ct)
			g.Expect(found && st == "True").To(BeTrue(), "SnapshotContent %s condition %s must be True", name, ct)
		}
		g.Expect(co.GetFinalizers()).To(ContainElement(parentProtectFinalizer), "SnapshotContent %s must RETAIN parent-protect (durable node)", name)
		kind, owner, ownerFound := controllerOwnerRef(co)
		g.Expect(ownerFound).To(BeTrue(), "SnapshotContent %s must have a controller ownerRef", name)
		if name == tree.root {
			g.Expect(kind).To(Equal("ObjectKeeper"), "root SnapshotContent %s must be owned by its ObjectKeeper", name)
			g.Expect(owner).To(Equal(tree.okName), "root SnapshotContent %s must be owned by ObjectKeeper %s", name, tree.okName)
		} else {
			g.Expect(kind).To(Equal("SnapshotContent"), "child SnapshotContent %s must be owned by a parent SnapshotContent", name)
			g.Expect(owner).To(Equal(tree.parentOf[name]), "child SnapshotContent %s must be owned by its parent %s", name, tree.parentOf[name])
		}
	}

	// Every ManifestCheckpoint: Ready=True/Completed, chunks populated.
	for _, mcpName := range tree.mcps {
		mcp, err := getResource(ctx, manifestCheckpointGVR, "", mcpName)
		g.Expect(err).NotTo(HaveOccurred(), "ManifestCheckpoint %s must survive", mcpName)
		g.Expect(mcp.GetDeletionTimestamp()).To(BeNil(), "ManifestCheckpoint %s must not be deleting", mcpName)
		st, reason, found := conditionStatus(mcp, condReady)
		g.Expect(found && st == "True").To(BeTrue(), "ManifestCheckpoint %s must be Ready", mcpName)
		g.Expect(reason).To(Equal("Completed"), "ManifestCheckpoint %s Ready reason must be Completed", mcpName)
		chunks, _, _ := unstructured.NestedSlice(mcp.Object, "status", "chunks")
		g.Expect(chunks).NotTo(BeEmpty(), "ManifestCheckpoint %s status.chunks must be populated", mcpName)
	}

	// Every chunk: exists, controller ownerRef -> its MCP.
	for _, chunk := range tree.chunks {
		ch, err := getResource(ctx, manifestCheckpointContentChunkGVR, "", chunk.name)
		g.Expect(err).NotTo(HaveOccurred(), "chunk %s must survive", chunk.name)
		g.Expect(ch.GetDeletionTimestamp()).To(BeNil(), "chunk %s must not be deleting", chunk.name)
		kind, owner, found := controllerOwnerRef(ch)
		g.Expect(found).To(BeTrue(), "chunk %s must have a controller ownerRef", chunk.name)
		g.Expect(kind).To(Equal("ManifestCheckpoint"), "chunk %s must be owned by a ManifestCheckpoint", chunk.name)
		g.Expect(owner).To(Equal(chunk.mcp), "chunk %s must be owned by MCP %s", chunk.name, chunk.mcp)
	}

	// Every VSC: readyToUse, Retain, snapshotHandle set, bound-protection finalizer present. Every llvs:
	// phase Created + sds-local-volume CSI finalizer present.
	for _, v := range tree.vscs {
		vsc, err := getResource(ctx, volumeSnapshotContentGVR, "", v.vsc)
		g.Expect(err).NotTo(HaveOccurred(), "VolumeSnapshotContent %s must survive", v.vsc)
		g.Expect(vsc.GetDeletionTimestamp()).To(BeNil(), "VolumeSnapshotContent %s must not be deleting", v.vsc)
		ready, _, _ := unstructured.NestedBool(vsc.Object, "status", "readyToUse")
		g.Expect(ready).To(BeTrue(), "VolumeSnapshotContent %s must be readyToUse", v.vsc)
		policy, _, _ := unstructured.NestedString(vsc.Object, "spec", "deletionPolicy")
		g.Expect(policy).To(Equal("Retain"), "VolumeSnapshotContent %s must be Retain (pinned durable)", v.vsc)
		handle, _, _ := unstructured.NestedString(vsc.Object, "status", "snapshotHandle")
		g.Expect(handle).NotTo(BeEmpty(), "VolumeSnapshotContent %s must have a snapshotHandle", v.vsc)
		g.Expect(vsc.GetFinalizers()).To(ContainElement(vscBoundProtectionFinalizer), "VolumeSnapshotContent %s must keep the CSI bound-protection finalizer", v.vsc)

		llvs, err := getResource(ctx, lvmLogicalVolumeSnapshotGVR, "", v.snapshotHandle)
		g.Expect(err).NotTo(HaveOccurred(), "llvs %s (VSC %s snapshotHandle) must survive", v.snapshotHandle, v.vsc)
		g.Expect(llvs.GetDeletionTimestamp()).To(BeNil(), "llvs %s must not be deleting", v.snapshotHandle)
		phase, _, _ := unstructured.NestedString(llvs.Object, "status", "phase")
		g.Expect(phase).To(Equal("Created"), "llvs %s status.phase must be Created", v.snapshotHandle)
		g.Expect(llvs.GetFinalizers()).To(ContainElement(llvsCSIFinalizer), "llvs %s must keep the sds-local-volume CSI finalizer", v.snapshotHandle)
	}
}

// volumeDataGcSpecs registers the durable-teardown flow (env-gated by E2E_VOLUME_DATA, hard local-CSI
// dependency): with a large snapshotTtlAfterDelete a data-bearing tree survives its source-namespace deletion
// intact (every content keeps parent-protect naturally), and deleting the root ObjectKeeper — after
// simulating the lost being-deleted stamp on the wired-ref VSCs — reclaims the whole tree including the
// physical llvs, with no synthetic finalizer barrier anywhere.
func volumeDataGcSpecs() {
	Context("Phase 3: durable volume-data teardown (large TTL, no synthetic finalizer)", func() {
		var (
			srcNS string
			sc    string
			tree  gcTree

			prevTTL    string
			prevTTLSet bool
		)

		BeforeAll(func() {
			if !suiteCfg.volumeData {
				Skip("E2E_VOLUME_DATA=false: skipping the phase-3 durable volume-data teardown flow (it runs by default)")
			}
			sc = suiteCfg.storageClass
			srcNS = uniqueNS("p3-voldata-gc")

			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
			defer cancel()

			By("Reading the current module snapshotTtlAfterDelete and registering its restore BEFORE changing it")
			mc, err := getResource(ctx, moduleConfigGVR, "", moduleName)
			Expect(err).NotTo(HaveOccurred(), "read ModuleConfig")
			prevTTL, prevTTLSet, _ = unstructured.NestedString(mc.Object, "spec", "settings", "snapshotTtlAfterDelete")
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 2*suiteCfg.moduleReadyTO+5*time.Minute)
				defer ccancel()
				var want *string
				if prevTTLSet {
					want = &prevTTL
				}
				_ = patchModuleSnapshotRootOkTTL(cctx, want)
				_ = storagekube.WaitForModuleReady(cctx, suiteRestCfg, moduleName, suiteCfg.moduleReadyTO)
				_ = waitControllerSnapshotRootOkTTLRolledOut(cctx, want, suiteCfg.moduleReadyTO)
				deleteNamespace(cctx, srcNS)
			})

			By("Setting a large snapshotTtlAfterDelete (" + vdGcLargeTTL + ") and waiting for the controller to roll out")
			ttl := vdGcLargeTTL
			Expect(patchModuleSnapshotRootOkTTL(ctx, &ttl)).To(Succeed())
			Expect(storagekube.WaitForModuleReady(ctx, suiteRestCfg, moduleName, suiteCfg.moduleReadyTO)).To(Succeed())
			Expect(waitControllerSnapshotRootOkTTLRolledOut(ctx, &ttl, suiteCfg.moduleReadyTO)).To(Succeed())

			By("Provisioning a thin, snapshot-capable default StorageClass via storage-e2e (" + sc + ")")
			_, err = testkit.EnsureDefaultStorageClass(ctx, suiteRestCfg, testkit.DefaultStorageClassConfig{
				StorageClassName:     sc,
				LVMType:              "Thin",
				ThinPoolName:         "thinpool",
				BaseKubeconfig:       suiteClusterResources.BaseKubeconfig,
				VMNamespace:          suiteCfg.vmNamespace,
				BaseStorageClassName: suiteCfg.baseStorageClass,
			})
			Expect(err).NotTo(HaveOccurred(), "provision default StorageClass")

			By("Hard local-CSI guard: the StorageClass provisioner MUST be sds-local-volume (llvs exist only there)")
			scObj, err := suiteClientset.StorageV1().StorageClasses().Get(ctx, sc, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "get StorageClass %s", sc)
			Expect(scObj.Provisioner).To(Equal(localCSIDriver),
				"the durable-teardown flow asserts LVMLogicalVolumeSnapshots, which exist only on sds-local-volume (%s); got provisioner %q", localCSIDriver, scObj.Provisioner)

			By("Wiring the StorageClass to a VolumeSnapshotClass for the local CSI driver")
			Expect(ensureStorageClassVolumeSnapshotClass(ctx, sc)).To(Succeed())

			By("Creating the source namespace and applying the full PVC source")
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			Expect(applyObjects(ctx, buildVolumeSource(srcNS, sc), srcNS)).To(Succeed())

			By("Starting the source probe Pod and waiting for it to run (binds all three PVCs)")
			_, err = suiteClientset.CoreV1().Pods(srcNS).Create(ctx, probePodSpec(srcNS, vdProbePod, []string{vdPVCRoot, vdPVCDisk, vdPVCStandalone}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create source probe pod")
			Expect(waitPodRunning(ctx, srcNS, vdProbePod, 10*time.Minute)).To(Succeed())

			By("Writing real marker bytes into all three source PVCs")
			marker := fmt.Sprintf("gc-%d", time.Now().UnixNano())
			writeCmd := fmt.Sprintf("printf %%s %q > %s && printf %%s %q > %s && printf %%s %q > %s && sync",
				marker, markerVolumePath(vdPVCRoot),
				marker, markerVolumePath(vdPVCDisk),
				marker, markerVolumePath(vdPVCStandalone))
			_, _, err = storagekube.ExecInPod(ctx, suiteRestCfg, srcNS, vdProbePod, vdProbeContainer, []string{"sh", "-c", writeCmd})
			Expect(err).NotTo(HaveOccurred(), "write marker bytes")

			By("Creating the root Snapshot over the PVC tree and waiting for it + its SnapshotContent Ready")
			Expect(createRootSnapshot(ctx, srcNS, vdGcRootSnapshotName)).To(Succeed())
			content, err := waitSnapshotReady(ctx, srcNS, vdGcRootSnapshotName, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())

			By("Waiting until every source PVC has a published data leg (whole tree materialized)")
			_, err = waitContentDataRefs(ctx, content, []string{vdPVCRoot, vdPVCDisk, vdPVCStandalone}, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())

			By("Confirming the large TTL propagated to the root ObjectKeeper")
			largeTTL, perr := time.ParseDuration(vdGcLargeTTL)
			Expect(perr).NotTo(HaveOccurred())
			Expect(waitRootOkTTL(ctx, srcNS, vdGcRootSnapshotName, largeTTL, suiteCfg.captureReadyTO)).To(Succeed())

			By("Recording every durable artifact identity (contents, MCPs+chunks, VSCs+llvs) before teardown")
			snap, err := getResource(ctx, snapshotGVR, srcNS, vdGcRootSnapshotName)
			Expect(err).NotTo(HaveOccurred())
			okName := names.ObjectKeeperName(snap.GetUID())
			tree, err = recordGcTree(ctx, content, okName)
			Expect(err).NotTo(HaveOccurred(), "record the durable artifact tree")
			wired := 0
			for _, v := range tree.vscs {
				if v.wired {
					wired++
				}
			}
			GinkgoWriter.Printf("GC tree: root=%s ok=%s contents=%d mcps=%d chunks=%d vscs=%d (wired=%d)\n",
				tree.root, tree.okName, len(tree.contents), len(tree.mcps), len(tree.chunks), len(tree.vscs), wired)
		})

		It("keeps the whole data-bearing tree durable after the source namespace is deleted", func() {
			Expect(tree.root).NotTo(BeEmpty(), "the BeforeAll must have recorded the tree")

			ctx, cancel := context.WithTimeout(context.Background(), 3*suiteCfg.snapshotReadyTO+10*time.Minute)
			defer cancel()

			By("Deleting the source namespace directly (not the keep-aware cleanup helper)")
			Expect(suiteClientset.CoreV1().Namespaces().Delete(ctx, srcNS, metav1.DeleteOptions{})).To(Succeed())

			By("Waiting for the namespace, the root Snapshot, and every bound (wired-ref) VolumeSnapshot to be gone")
			Eventually(func() error {
				_, err := suiteClientset.CoreV1().Namespaces().Get(ctx, srcNS, metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					return nil
				}
				if err != nil {
					return err
				}
				return fmt.Errorf("namespace %s still exists", srcNS)
			}).WithContext(ctx).WithTimeout(3*suiteCfg.snapshotReadyTO + 5*time.Minute).WithPolling(pollInterval).Should(Succeed())
			assertResourceGone(ctx, snapshotGVR, srcNS, vdGcRootSnapshotName, 2*time.Minute)
			for _, v := range tree.vscs {
				if v.wired {
					assertResourceGone(ctx, volumeSnapshotGVR, v.vsNamespace, v.vsName, 2*time.Minute)
				}
			}

			By("Asserting every cluster-scoped artifact is alive + healthy and every content naturally retains parent-protect")
			// Converge first: deleting the namespace kicks a reconcile wave (the binder latches
			// boundSnapshotDeleted on the surviving root content), so give the durable state a moment to
			// settle before demanding it stays put — a bare one-shot assert could catch a transient blip.
			Eventually(func(g Gomega) {
				assertTreeAlive(ctx, g, tree)
			}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(pollInterval).Should(Succeed())

			By("Confirming the tree stays stable against the large TTL (short Consistently)")
			Consistently(func(g Gomega) {
				assertTreeAlive(ctx, g, tree)
			}).WithContext(ctx).WithTimeout(20 * time.Second).WithPolling(5 * time.Second).Should(Succeed())
		})

		It("reclaims the whole tree incl. llvs when the root ObjectKeeper is deleted (lost being-deleted stamp simulated)", func() {
			Expect(tree.root).NotTo(BeEmpty(), "the BeforeAll must have recorded the tree")

			ctx, cancel := context.WithTimeout(context.Background(), 4*suiteCfg.snapshotReadyTO+15*time.Minute)
			defer cancel()

			By("Simulating the lost lifecycle stamp: stripping being-deleted from the wired-ref VSCs only")
			// The exact bound VolumeSnapshots are already NotFound (Spec 1), so nothing can re-add the
			// annotation. A merge patch with a null value drops the key (a no-op if it was never stamped).
			nullAnno := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:null}}}`, volumeSnapshotBeingDeletedAnnotation))
			for _, v := range tree.vscs {
				if !v.wired {
					continue
				}
				_, err := suiteDyn.Resource(volumeSnapshotContentGVR).Patch(ctx, v.vsc, types.MergePatchType, nullAnno, metav1.PatchOptions{})
				Expect(err).NotTo(HaveOccurred(), "strip being-deleted annotation from wired VSC %s", v.vsc)
			}
			By("Confirming the stripped annotation stays absent (no writer remains to re-stamp it)")
			Consistently(func(g Gomega) {
				for _, v := range tree.vscs {
					if !v.wired {
						continue
					}
					vsc, err := getResource(ctx, volumeSnapshotContentGVR, "", v.vsc)
					g.Expect(err).NotTo(HaveOccurred())
					_, present := vsc.GetAnnotations()[volumeSnapshotBeingDeletedAnnotation]
					g.Expect(present).To(BeFalse(), "wired VSC %s must not carry being-deleted before ObjectKeeper deletion", v.vsc)
				}
			}).WithContext(ctx).WithTimeout(10 * time.Second).WithPolling(2 * time.Second).Should(Succeed())

			By("Deleting the root ObjectKeeper " + tree.okName)
			Expect(suiteDyn.Resource(objectKeeperGVR).Delete(ctx, tree.okName, metav1.DeleteOptions{})).To(Succeed())

			// Generous teardown budget: OK deletion -> per-node ownerRef cascade (each node self-finalizes) ->
			// CSI DeleteSnapshot -> llvs finalizer removal -> VSC GC. No synthetic finalizer is manipulated:
			// natural parent-protect retention is part of the production contract under test.
			teardownTO := 3*suiteCfg.snapshotReadyTO + 12*time.Minute

			By("Asserting the whole tree is reclaimed: every SnapshotContent, MCP, chunk, VSC, llvs, and the ObjectKeeper")
			for _, name := range tree.contents {
				assertResourceGone(ctx, snapshotContentGVR, "", name, teardownTO)
			}
			for _, mcpName := range tree.mcps {
				assertResourceGone(ctx, manifestCheckpointGVR, "", mcpName, teardownTO)
			}
			for _, chunk := range tree.chunks {
				assertResourceGone(ctx, manifestCheckpointContentChunkGVR, "", chunk.name, teardownTO)
			}
			for _, v := range tree.vscs {
				assertResourceGone(ctx, volumeSnapshotContentGVR, "", v.vsc, teardownTO)
				assertResourceGone(ctx, lvmLogicalVolumeSnapshotGVR, "", v.snapshotHandle, teardownTO)
			}
			assertResourceGone(ctx, objectKeeperGVR, "", tree.okName, teardownTO)
		})
	})
}
