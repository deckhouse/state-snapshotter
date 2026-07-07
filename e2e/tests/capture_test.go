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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Source object names for the manifest-only (no-volume-data) capture tree: an ownerless ConfigMap (the
// manifest leg) and a single manifest-only DemoVirtualMachine that provides the domain child-snapshot
// node. No DemoVirtualDisk is created here — dataless disks are no longer allowed (every DemoVirtualDisk
// must be PVC-backed per a spec XValidation rule), so disks live only in the volume-data phase where they
// get real PVCs. Generic-object discovery (RBAC/Service/Deployment/etc.) is covered by the
// namespace-capture rework specs and deliberately not duplicated here.
const (
	srcConfigMapName = "demo-snapshot-cm"
	srcVMName        = "vm-1"
	rootSnapshotName = "demo-tree"
)

// capturedTree holds the shared state produced by captureSpecs and consumed by the aggregated-API,
// restore and import specs that run after it in the merged manifest-only flow (Ordered container).
type capturedTree struct {
	namespace   string // source namespace the demo tree was applied into
	rootSnap    string // root Snapshot name
	rootContent string // root SnapshotContent name (status.boundSnapshotContentName)
}

var captured capturedTree

// captureTL records the capture state-transition timeline for the manifest-only capture (started in
// BeforeAll, stopped in AfterAll) so bottlenecks in snapshot creation are visible in the test log.
var captureTL *captureTimeline

// buildManifestOnlySource returns the no-volume-data capture source: an ownerless ConfigMap (the manifest
// leg) and a single manifest-only DemoVirtualMachine (no virtualDiskName) that provides the domain
// child-snapshot node. No DemoVirtualDisk is created: dataless disks are no longer allowed (every
// DemoVirtualDisk must be PVC-backed per a spec XValidation rule), so this tree never models a disk without
// data. Generic-object discovery (RBAC/Service/Deployment/etc.) is exercised by the namespace-capture
// rework specs and deliberately not duplicated here. The result is a pure manifest tree where VolumeReady
// is vacuously true.
func buildManifestOnlySource(ns string) []*unstructured.Unstructured {
	configMap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      srcConfigMapName,
			"namespace": ns,
		},
		"data": map[string]interface{}{"demo": "tree"},
	}}
	// Manifest-only DemoVirtualMachine (no virtualDiskName): the single domain kind in this tree, present
	// solely to produce a DemoVirtualMachineSnapshot child node without needing any volume data.
	vm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualMachine",
		"metadata": map[string]interface{}{
			"name":      srcVMName,
			"namespace": ns,
		},
		"spec": map[string]interface{}{},
	}}
	return []*unstructured.Unstructured{configMap, vm}
}

// createRootSnapshot creates the empty-spec root Snapshot (dynamic discovery of manifests + demo tree).
func createRootSnapshot(ctx context.Context, ns, name string) error {
	snap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "Snapshot",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
		},
		"spec": map[string]interface{}{},
	}}
	_, err := suiteDyn.Resource(snapshotGVR).Namespace(ns).Create(ctx, snap, metav1.CreateOptions{})
	return err
}

// captureSpecs registers the capture specs of the manifest-only flow: apply the source, create the root
// Snapshot, and assert the whole snapshot tree reaches Ready.
func captureSpecs() {
	Context("Manifest-only capture", func() {
		BeforeAll(func() {
			captured.namespace = uniqueNS("src")
			captured.rootSnap = rootSnapshotName

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			By("Creating the source namespace " + captured.namespace)
			Expect(ensureNamespace(ctx, captured.namespace)).To(Succeed())

			// Start the background capture timeline before applying the source so the demo-CR reconcile and
			// the whole snapshot creation chain are timed (see AfterAll for stop).
			captureTL = startCaptureTimeline(captured.namespace)

			By("Applying the manifest-only source (ConfigMap + manifest-only VM)")
			Expect(applyObjects(ctx, buildManifestOnlySource(captured.namespace), captured.namespace)).To(Succeed())

			By("Creating the root Snapshot " + captured.rootSnap)
			Expect(createRootSnapshot(ctx, captured.namespace, captured.rootSnap)).To(Succeed())
		})

		AfterAll(func() {
			captureTL.stop()
		})

		It("captures the demo snapshot tree (root Snapshot + SnapshotContent Ready)", func() {
			// This is the regression guard for the pre-Planned orphan-wave deadlock (content-single-writer
			// design §9.2): before the eager-shell fix the root Snapshot never reached Ready on this exact
			// demo tree (root content <- root Planned <- children Ready <- child content bound <- root content).
			// If Block 0 regresses, this wait times out.
			//
			// Snapshot creation (capture) must complete quickly, so both waits below are bounded by the
			// short captureReadyTO (fail fast) instead of the generous restore-path snapshotReadyTO.
			// Budget for the two sequential waits plus a buffer for the intervening GETs.
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
			defer cancel()

			By("Waiting for the root Snapshot to become Ready")
			content, err := waitSnapshotReady(ctx, captured.namespace, captured.rootSnap, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(content).NotTo(BeEmpty())
			captured.rootContent = content
			GinkgoWriter.Printf("root SnapshotContent: %s\n", content)

			By("Waiting for the bound SnapshotContent to reach all leg conditions (Manifests/Volumes/Children/Ready)")
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())
		})

		It("writes childrenSnapshotContentRefs equal to the declared children exactly (single-writer edges)", func() {
			// Block 1 (content-single-writer design §3.1/§3.2, INV-CONTENT-CHILDREN-1): the
			// SnapshotContentController is the single writer of status.childrenSnapshotContentRefs, projected
			// from the owning snapshot's status.childrenSnapshotRefs. For every snapshot node in the tree its
			// bound content's childrenSnapshotContentRefs must equal EXACTLY the set of bound-content names of
			// its declared NON-LEAF children — no missing edge, no duplicate. CSI VolumeSnapshot visibility
			// leaves are skipped (they have no backing SnapshotContent; their orphan edge is linked by the
			// snapshot path until Block 3, and the manifest-only tree has no orphan PVCs anyway).
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
			defer cancel()
			Expect(captured.rootContent).NotTo(BeEmpty(), "the capture spec must run first and record the root content")

			ns := captured.namespace

			By("Collecting every snapshot node in the tree (root + descendants)")
			nodes := []childRef{{kind: "Snapshot", name: captured.rootSnap}}
			descendants, err := walkSnapshotTree(ctx, ns, captured.rootSnap)
			Expect(err).NotTo(HaveOccurred())
			nodes = append(nodes, descendants...)

			By("Asserting each node's content edges equal its declared non-leaf children exactly")
			for _, node := range nodes {
				if node.kind == "VolumeSnapshot" {
					continue // CSI visibility leaf: no backing content of its own
				}
				gvr, ok := gvrForSnapshotKind(node.kind)
				Expect(ok).To(BeTrue(), "unknown snapshot kind %q for %s", node.kind, node.name)
				node := node // capture for the closure
				Eventually(func(g Gomega) {
					expected, contentName, err := declaredChildContentNames(ctx, gvr, ns, node.name)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(contentName).NotTo(BeEmpty(), "node %s/%s must be bound to a content", node.kind, node.name)

					actual, err := contentChildEdgeNames(ctx, contentName)
					g.Expect(err).NotTo(HaveOccurred())

					for edge, count := range actual {
						g.Expect(count).To(Equal(1), "duplicate edge %q in content %s (childrenSnapshotContentRefs must be a set)", edge, contentName)
					}
					g.Expect(actual).To(Equal(expected),
						"content %s childrenSnapshotContentRefs must equal its declared non-leaf children exactly (node %s/%s)", contentName, node.kind, node.name)
				}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())
			}
		})

		It("publishes each node's manifestCheckpointName pointing at a Ready ManifestCheckpoint owned by its content (single-writer manifest leg)", func() {
			// Block 2 (content-single-writer design §3.1/§3.2, INV-CONTENT-WRITER-1): the
			// SnapshotContentController aggregator is the single writer of status.manifestCheckpointName,
			// projected from the owning snapshot's ManifestCaptureRequest (MCR -> mcr.status.checkpointName).
			// For every snapshot node in the tree its bound content must publish a manifestCheckpointName that
			// points at a ManifestCheckpoint which is Ready=True AND owned (ownerRef) by that SAME content —
			// the durable ownership handoff that lets the MCP be GC'd with the content. The manifest leg is a
			// hard Ready gate (empty manifestCheckpointName => ManifestsReady=False/ManifestCapturePending), so
			// every node that reached Ready above necessarily satisfies this; the spec pins WHO published it
			// (the aggregator) and the MCP ownership edge. CSI VolumeSnapshot visibility leaves have no backing
			// content and are skipped.
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
			defer cancel()
			Expect(captured.rootContent).NotTo(BeEmpty(), "the capture spec must run first and record the root content")

			ns := captured.namespace

			By("Walking the fully materialized snapshot tree (root + descendants)")
			// The eager-shell read barrier lets the root reach Ready with root ChildrenReady vacuously true
			// while status.childrenSnapshotRefs is still filling in. Walk under Eventually until the tree has
			// materialized AND the domain child node is present, otherwise the per-node manifest assertions
			// below could pass vacuously against a root-only walk. The DemoVirtualMachineSnapshot inventory
			// guard mirrors the sibling childrenSnapshotRefs spec for this manifest-only tree.
			var nodes []childRef
			Eventually(func(g Gomega) {
				descendants, err := walkSnapshotTree(ctx, ns, captured.rootSnap)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(descendants).NotTo(BeEmpty(), "root Snapshot should have demo child snapshots (tree not materialized yet)")
				kinds := map[string]int{}
				for _, n := range descendants {
					kinds[n.kind]++
				}
				g.Expect(kinds["DemoVirtualMachineSnapshot"]).To(BeNumerically(">=", 1), "expected the DemoVirtualMachineSnapshot child in the tree")
				nodes = append([]childRef{{kind: "Snapshot", name: captured.rootSnap}}, descendants...)
			}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())

			By("Asserting each node's content publishes a manifestCheckpointName -> Ready MCP owned by the content")
			for _, node := range nodes {
				if node.kind == "VolumeSnapshot" {
					continue // CSI visibility leaf: no backing content of its own
				}
				gvr, ok := gvrForSnapshotKind(node.kind)
				Expect(ok).To(BeTrue(), "unknown snapshot kind %q for %s", node.kind, node.name)
				node := node // capture for the closure
				Eventually(func(g Gomega) {
					snap, err := getResource(ctx, gvr, ns, node.name)
					g.Expect(err).NotTo(HaveOccurred())
					contentName, _, _ := unstructured.NestedString(snap.Object, "status", "boundSnapshotContentName")
					g.Expect(contentName).NotTo(BeEmpty(), "node %s/%s must be bound to a content", node.kind, node.name)

					content, err := getResource(ctx, snapshotContentGVR, "", contentName)
					g.Expect(err).NotTo(HaveOccurred())
					mcpName, _, _ := unstructured.NestedString(content.Object, "status", "manifestCheckpointName")
					g.Expect(mcpName).NotTo(BeEmpty(),
						"content %s (node %s/%s) must publish status.manifestCheckpointName (single-writer manifest leg)", contentName, node.kind, node.name)

					mcp, err := getResource(ctx, manifestCheckpointGVR, "", mcpName)
					g.Expect(err).NotTo(HaveOccurred(), "ManifestCheckpoint %s referenced by content %s must exist", mcpName, contentName)

					st, reason, found := conditionStatus(mcp, condReady)
					g.Expect(found).To(BeTrue(), "ManifestCheckpoint %s must carry a Ready condition", mcpName)
					g.Expect(st).To(Equal("True"), "ManifestCheckpoint %s must be Ready=True (reason=%q)", mcpName, reason)

					g.Expect(ownedBySnapshotContent(mcp, contentName)).To(BeTrue(),
						"ManifestCheckpoint %s must be owned (ownerRef) by its content %s (ownership handoff)", mcpName, contentName)
				}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())
			}
		})

		It("populates the demo child snapshot tree (childrenSnapshotRefs) with Ready nodes", func() {
			// Snapshot creation: bound the tree walk and children readiness by the short captureReadyTO
			// (fail fast) rather than the restore-path snapshotReadyTO.
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
			defer cancel()

			By("Walking status.childrenSnapshotRefs from the root Snapshot")
			var nodes []childRef
			Eventually(func(g Gomega) {
				var err error
				nodes, err = walkSnapshotTree(ctx, captured.namespace, captured.rootSnap)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(nodes).NotTo(BeEmpty(), "root Snapshot should have demo child snapshots")
			}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())

			By("Asserting the demo inventory is represented in the tree")
			kinds := map[string]int{}
			for _, n := range nodes {
				kinds[n.kind]++
				GinkgoWriter.Printf("  child snapshot: %s/%s\n", n.kind, n.name)
			}
			Expect(kinds["DemoVirtualMachineSnapshot"]).To(BeNumerically(">=", 1), "expected a DemoVirtualMachineSnapshot")
			// No DemoVirtualDiskSnapshot is expected: the manifest-only tree no longer models dataless disks
			// (disks must be PVC-backed); they are exercised in the volume-data phase instead.

			By("Asserting every demo child snapshot is Ready=True")
			Expect(waitChildrenReady(ctx, captured.namespace, nodes, suiteCfg.captureReadyTO)).To(Succeed())
		})
	})
}

// declaredChildContentNames resolves a snapshot node's bound content name (status.boundSnapshotContentName)
// and the multiset of bound-content names of its declared NON-LEAF children (status.childrenSnapshotRefs,
// excluding CSI VolumeSnapshot visibility leaves). It is the "expected" edge set for the aggregator's
// single-writer projection: each declared non-leaf child snapshot -> its own bound SnapshotContent name.
func declaredChildContentNames(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (map[string]int, string, error) {
	snap, err := getResource(ctx, gvr, ns, name)
	if err != nil {
		return nil, "", err
	}
	contentName, _, _ := unstructured.NestedString(snap.Object, "status", "boundSnapshotContentName")
	expected := map[string]int{}
	for _, ch := range childSnapshotRefs(snap) {
		if ch.kind == "VolumeSnapshot" {
			continue // orphan CSI visibility leaf: edge linked by the snapshot path, not a content child here
		}
		chGVR, ok := gvrForSnapshotKind(ch.kind)
		if !ok {
			return nil, "", fmt.Errorf("unknown child snapshot kind %q (%s)", ch.kind, ch.name)
		}
		childSnap, err := getResource(ctx, chGVR, ns, ch.name)
		if err != nil {
			return nil, "", fmt.Errorf("get declared child %s %s/%s: %w", ch.kind, ns, ch.name, err)
		}
		childContent, _, _ := unstructured.NestedString(childSnap.Object, "status", "boundSnapshotContentName")
		if childContent == "" {
			return nil, "", fmt.Errorf("declared child %s %s/%s has no bound content yet", ch.kind, ns, ch.name)
		}
		expected[childContent]++
	}
	return expected, contentName, nil
}

// ownedBySnapshotContent reports whether obj carries an ownerReference to the named SnapshotContent. The
// aggregator re-parents each node's ManifestCheckpoint onto its bound content (the ownership handoff) so the
// MCP is GC'd together with the content; this verifies that durable ownership edge is present.
func ownedBySnapshotContent(obj *unstructured.Unstructured, contentName string) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == "SnapshotContent" && ref.Name == contentName {
			return true
		}
	}
	return false
}

// contentChildEdgeNames reads a (cluster-scoped) SnapshotContent's status.childrenSnapshotContentRefs into
// a multiset of edge names, so a duplicate edge shows up as a count > 1.
func contentChildEdgeNames(ctx context.Context, contentName string) (map[string]int, error) {
	content, err := getResource(ctx, snapshotContentGVR, "", contentName)
	if err != nil {
		return nil, err
	}
	actual := map[string]int{}
	raw, _, _ := unstructured.NestedSlice(content.Object, "status", "childrenSnapshotContentRefs")
	for _, r := range raw {
		m, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		n, _, _ := unstructured.NestedString(m, "name")
		if n == "" {
			continue
		}
		actual[n]++
	}
	return actual, nil
}
