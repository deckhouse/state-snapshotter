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
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Plural resource names of the demo snapshot kinds, as addressed on the aggregated subresource paths.
const (
	resDemoVMSnapshots   = "demovirtualmachinesnapshots"
	resDemoDiskSnapshots = "demovirtualdisksnapshots"
)

// findManifest returns the first object of the given kind/name in a decoded manifest array.
func findManifest(objs []unstructured.Unstructured, kind, name string) (*unstructured.Unstructured, bool) {
	for i := range objs {
		if objs[i].GetKind() == kind && objs[i].GetName() == name {
			return &objs[i], true
		}
	}
	return nil, false
}

// firstNodeOfKind returns the first walked child snapshot node of the requested kind.
func firstNodeOfKind(nodes []childRef, kind string) (childRef, bool) {
	for _, n := range nodes {
		if n.kind == kind {
			return n, true
		}
	}
	return childRef{}, false
}

// subtreeManifestIdentity mirrors the wire contract of the subtree-manifest-identities subresource
// (pkg/snapshotsdk.SubtreeManifestIdentity). It is duplicated here (not imported) to keep the e2e module
// dependency-light, the same convention the suite uses for the leg-condition constants.
type subtreeManifestIdentity struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

// subtreeManifestIdentitiesResponse is the JSON body of the subtree-manifest-identities subresource.
type subtreeManifestIdentitiesResponse struct {
	Identities []subtreeManifestIdentity `json:"identities"`
}

// decodeSubtreeIdentities parses the subtree-manifest-identities response body.
func decodeSubtreeIdentities(data []byte) ([]subtreeManifestIdentity, error) {
	var resp subtreeManifestIdentitiesResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode subtree-manifest-identities: %w (body: %s)", err, truncate(data, 512))
	}
	return resp.Identities, nil
}

// subtreeIdentityKey renders the exclude-match key (apiVersion|kind|namespace|name); uid disambiguates a
// recreated object but is not part of the dedup key, matching the server/SDK contract.
func subtreeIdentityKey(id subtreeManifestIdentity) string {
	return id.APIVersion + "|" + id.Kind + "|" + id.Namespace + "|" + id.Name
}

// hasSubtreeIdentity reports whether the identity set contains an object of the given kind/name.
func hasSubtreeIdentity(ids []subtreeManifestIdentity, kind, name string) bool {
	for _, id := range ids {
		if id.Kind == kind && id.Name == name {
			return true
		}
	}
	return false
}

// aggregatedAPISpecs registers the aggregated subresource read specs of the manifest-only flow: the
// per-node manifests-download surface (core + demo groups) and the apply-ready manifests-with-data-restoration.
func aggregatedAPISpecs() {
	Context("Aggregated subresource APIs", func() {
		var vmSnapshot childRef

		BeforeAll(func() {
			Expect(captured.namespace).NotTo(BeEmpty(), "capture phase must have run first")
			Expect(captured.rootContent).NotTo(BeEmpty(), "root SnapshotContent must be resolved")

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			nodes, err := walkSnapshotTree(ctx, captured.namespace, captured.rootSnap)
			Expect(err).NotTo(HaveOccurred())
			var ok bool
			vmSnapshot, ok = firstNodeOfKind(nodes, "DemoVirtualMachineSnapshot")
			Expect(ok).To(BeTrue(), "expected a DemoVirtualMachineSnapshot node")
		})

		It("serves the root Snapshot own-node manifests via manifests-download (core group)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			path := coreSnapshotSubPath(captured.namespace, captured.rootSnap, subManifestsDownload)
			body, err := aggGet(ctx, path, nil)
			Expect(err).NotTo(HaveOccurred(), "GET %s", path)

			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			_, found := findManifest(objs, "ConfigMap", srcConfigMapName)
			Expect(found).To(BeTrue(), "manifests-download should contain the source ConfigMap %s", srcConfigMapName)
		})

		It("serves a demo node's own manifests via the core generic manifests-download path", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			path := coreGenericSubPath(captured.namespace, resDemoVMSnapshots, vmSnapshot.name, subManifestsDownload)
			body, err := aggGet(ctx, path, nil)
			Expect(err).NotTo(HaveOccurred(), "GET %s", path)

			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			_, found := findManifest(objs, "DemoVirtualMachine", srcVMName)
			Expect(found).To(BeTrue(), "demo manifests-download should contain DemoVirtualMachine %s", srcVMName)
		})

		It("returns sanitized, apply-ready objects via manifests-with-data-restoration (core group)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			const targetNS = "restore-read-probe"
			path := coreSnapshotSubPath(captured.namespace, captured.rootSnap, subManifestsRestore)
			body, err := aggGet(ctx, path, map[string]string{"targetNamespace": targetNS})
			Expect(err).NotTo(HaveOccurred(), "GET %s", path)

			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())

			cm, found := findManifest(objs, "ConfigMap", srcConfigMapName)
			Expect(found).To(BeTrue(), "restore output should contain the ConfigMap")

			By("asserting the restore output is namespace-rewritten and sanitized")
			Expect(cm.GetNamespace()).To(Equal(targetNS), "namespace must be rewritten to the target")
			_, hasStatus := cm.Object["status"]
			Expect(hasStatus).To(BeFalse(), "restore output must strip status")
			_, hasManagedFields, _ := unstructured.NestedFieldNoCopy(cm.Object, "metadata", "managedFields")
			Expect(hasManagedFields).To(BeFalse(), "restore output must strip metadata.managedFields")

			By("asserting cluster-scoped objects (Namespace) are dropped from restore output")
			_, hasNS := findManifest(objs, "Namespace", captured.namespace)
			Expect(hasNS).To(BeFalse(), "cluster-scoped Namespace must not be in restore output")
		})

		It("serves a demo subtree via manifests-with-data-restoration (demo group)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			const targetNS = "restore-read-probe"
			path := demoSubPath(captured.namespace, resDemoVMSnapshots, vmSnapshot.name, subManifestsRestore)
			body, err := aggGet(ctx, path, map[string]string{"targetNamespace": targetNS})
			Expect(err).NotTo(HaveOccurred(), "GET %s", path)

			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			vm, found := findManifest(objs, "DemoVirtualMachine", srcVMName)
			Expect(found).To(BeTrue(), "demo restore subtree should contain DemoVirtualMachine %s", srcVMName)
			Expect(vm.GetNamespace()).To(Equal(targetNS), "demo restore output must be namespace-rewritten")
			// The manifest-only VM owns no DemoVirtualDisk in this tree (dataless disks are disallowed), so
			// the delegated subtree is just the VM's own manifest.
		})
	})

	// Block 7 (content-single-writer design §2/§3/§8.1/§8.3, decision #10 — main-owned commonController):
	// the whole captureState.commonController (the manifest/data capture-leg latches plus the new
	// subtreePlanned field) is written by the SnapshotContentController (main) SIDEWAYS onto the
	// xxxSnapshot, and main REAPS the domain MCR/VCR in the same pass after the durable handoff
	// (latch-before-reap => no re-creation churn). The root manifest-exclude is computed from the
	// subtree-manifest-identities subresource. These specs run on the shared manifest-only capture tree.
	Context("Block 7: main-owned commonController + root manifest-exclude via subtree-manifest-identities", func() {
		BeforeAll(func() {
			Expect(captured.namespace).NotTo(BeEmpty(), "capture phase must have run first")
			Expect(captured.rootContent).NotTo(BeEmpty(), "root SnapshotContent must be resolved")
		})

		It("serves the root subtree-manifest-identities as a de-duplicated set spanning the whole subtree (design §8.3)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.captureReadyTO+time.Minute)
			defer cancel()

			// The endpoint is FAIL-CLOSED: it returns HTTP 409 (surfaced by aggGet as an error) while any
			// subtree ManifestCheckpoint is not Ready or an object is double-captured across nodes. On the
			// Ready tree it returns 200 with the flat, de-duplicated identity set. Poll until it settles.
			var ids []subtreeManifestIdentity
			path := coreContentSubPath(captured.rootContent, subManifestsIdentities)
			Eventually(func(g Gomega) {
				body, err := aggGet(ctx, path, nil)
				g.Expect(err).NotTo(HaveOccurred(), "GET %s (409 => a subtree MCP is not Ready or an object is double-captured)", path)
				ids, err = decodeSubtreeIdentities(body)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(hasSubtreeIdentity(ids, "DemoVirtualMachine", srcVMName)).To(BeTrue(),
					"the child DemoVirtualMachineSnapshot subtree must contribute DemoVirtualMachine %s", srcVMName)
			}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())

			By("Asserting the set spans the whole subtree (root's own manifest + the child subtree)")
			Expect(hasSubtreeIdentity(ids, "ConfigMap", srcConfigMapName)).To(BeTrue(),
				"the root's own node manifest must contribute ConfigMap %s", srcConfigMapName)

			By("Asserting the identity set is de-duplicated (each apiVersion|kind|namespace|name exactly once)")
			seen := map[string]int{}
			for _, id := range ids {
				seen[subtreeIdentityKey(id)]++
			}
			for key, n := range seen {
				Expect(n).To(Equal(1), "identity %q appears %d times; the subtree exclude set must be de-duplicated", key, n)
			}
		})

		It("excludes subtree-captured objects from the root own-manifests so no object is double-captured (design §8.3)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.captureReadyTO+time.Minute)
			defer cancel()

			// Both endpoints are only servable once the tree has settled: subtree-manifest-identities is
			// FAIL-CLOSED (409 while any subtree MCP is not Ready), and the root own-manifests are served only
			// after main latches commonController.manifestCaptured. Poll the read+assert block together so the
			// spec does not race the still-converging tree (a one-shot read would flake on 409/empty).
			var ids []subtreeManifestIdentity
			var own []unstructured.Unstructured
			Eventually(func(g Gomega) {
				body, err := aggGet(ctx, coreContentSubPath(captured.rootContent, subManifestsIdentities), nil)
				g.Expect(err).NotTo(HaveOccurred(), "GET subtree-manifest-identities (409 => a subtree MCP is not Ready)")
				ids, err = decodeSubtreeIdentities(body)
				g.Expect(err).NotTo(HaveOccurred())

				own, err = getRootOwnManifests(ctx, captured.namespace, captured.rootSnap)
				g.Expect(err).NotTo(HaveOccurred(), "root own-manifests are served only after main latches manifestCaptured")

				g.Expect(hasSubtreeIdentity(ids, "DemoVirtualMachine", srcVMName)).To(BeTrue(),
					"the child DemoVirtualMachineSnapshot subtree must contribute DemoVirtualMachine %s", srcVMName)
			}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())

			By("Asserting the child-captured DemoVirtualMachine is in the subtree set but NOT re-captured at the root")
			_, reCaptured := findManifest(own, "DemoVirtualMachine", srcVMName)
			Expect(reCaptured).To(BeFalse(),
				"DemoVirtualMachine %s is captured by its domain child subtree; the root MCR exclude (via subtree-manifest-identities) must drop it from the root own-manifests", srcVMName)

			By("Sanity: the root's own ConfigMap IS captured at the root (the exclude does not over-drop)")
			_, hasCM := findManifest(own, "ConfigMap", srcConfigMapName)
			Expect(hasCM).To(BeTrue(), "the root own-manifests must still carry the root-owned ConfigMap %s", srcConfigMapName)
		})

		It("latches commonController.manifestCaptured on every xxxSnapshot node (main-written) and reaps the MCR with no churn (decision #10)", func() {
			// Sequential waits under one ctx: the per-node latch checks + the MCR-reaped Eventually (each
			// bounded by captureReadyTO) plus the 15s no-churn Consistently — size the parent to their sum.
			ctx, cancel := context.WithTimeout(context.Background(), 3*suiteCfg.captureReadyTO+time.Minute)
			defer cancel()
			ns := captured.namespace

			By("Collecting every snapshot node in the tree (root + descendants)")
			nodes := []childRef{{kind: "Snapshot", name: captured.rootSnap}}
			descendants, err := walkSnapshotTree(ctx, ns, captured.rootSnap)
			Expect(err).NotTo(HaveOccurred())
			nodes = append(nodes, descendants...)

			By("Asserting main latched manifestCaptured=true on every node's xxxSnapshot and never on its content")
			for _, node := range nodes {
				if node.kind == "VolumeSnapshot" {
					continue // CSI visibility leaf: no owning xxxSnapshot commonController of its own
				}
				gvr, ok := gvrForSnapshotKind(node.kind)
				Expect(ok).To(BeTrue(), "unknown snapshot kind %q for %s", node.kind, node.name)

				Eventually(func(g Gomega) {
					snap, err := getResource(ctx, gvr, ns, node.name)
					g.Expect(err).NotTo(HaveOccurred())
					val, found := snapshotCommonControllerLatch(snap, "manifestCaptured")
					g.Expect(found).To(BeTrue(), "node %s/%s must carry commonController.manifestCaptured", node.kind, node.name)
					g.Expect(val).To(BeTrue(), "node %s/%s manifestCaptured must be latched true (main-written)", node.kind, node.name)

					contentName, _, _ := unstructured.NestedString(snap.Object, "status", "boundSnapshotContentName")
					g.Expect(contentName).NotTo(BeEmpty(), "node %s/%s must be bound to a content", node.kind, node.name)
					content, err := getResource(ctx, snapshotContentGVR, "", contentName)
					g.Expect(err).NotTo(HaveOccurred())
					_, onContent := snapshotCommonControllerLatch(content, "manifestCaptured")
					g.Expect(onContent).To(BeFalse(),
						"the latch is snapshot-native: SnapshotContent %s must NOT carry commonController.manifestCaptured", contentName)
				}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())
			}

			By("Waiting until every domain ManifestCaptureRequest is reaped (durable manifest handoff done)")
			Eventually(func(g Gomega) {
				list, err := suiteDyn.Resource(manifestCaptureRequestGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(list.Items).To(BeEmpty(), "main must reap each MCR after latching manifestCaptured")
			}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())

			By("Asserting no MCR is re-created (latch-before-reap => the domain suppresses re-creation => no churn)")
			Consistently(func(g Gomega) {
				list, err := suiteDyn.Resource(manifestCaptureRequestGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(list.Items).To(BeEmpty(),
					"a re-created MCR after reap means the latch was written after the delete (latch-after-reap regression / churn)")
			}).WithTimeout(15 * time.Second).WithPolling(3 * time.Second).Should(Succeed())
		})

		It("latches commonController.subtreePlanned on every xxxSnapshot node (main-written, snapshot-native) (design §8.1, decision #10)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
			defer cancel()
			ns := captured.namespace

			nodes := []childRef{{kind: "Snapshot", name: captured.rootSnap}}
			descendants, err := walkSnapshotTree(ctx, ns, captured.rootSnap)
			Expect(err).NotTo(HaveOccurred())
			nodes = append(nodes, descendants...)

			By("Asserting every node carries subtreePlanned=true on the xxxSnapshot and never on its content")
			for _, node := range nodes {
				if node.kind == "VolumeSnapshot" {
					continue
				}
				gvr, ok := gvrForSnapshotKind(node.kind)
				Expect(ok).To(BeTrue(), "unknown snapshot kind %q for %s", node.kind, node.name)

				Eventually(func(g Gomega) {
					snap, err := getResource(ctx, gvr, ns, node.name)
					g.Expect(err).NotTo(HaveOccurred())
					val, found := snapshotCommonControllerLatch(snap, "subtreePlanned")
					g.Expect(found).To(BeTrue(), "node %s/%s must carry commonController.subtreePlanned", node.kind, node.name)
					g.Expect(val).To(BeTrue(), "node %s/%s subtreePlanned must be latched true (main-computed over its direct children)", node.kind, node.name)

					contentName, _, _ := unstructured.NestedString(snap.Object, "status", "boundSnapshotContentName")
					g.Expect(contentName).NotTo(BeEmpty())
					content, err := getResource(ctx, snapshotContentGVR, "", contentName)
					g.Expect(err).NotTo(HaveOccurred())
					_, onContent := snapshotCommonControllerLatch(content, "subtreePlanned")
					g.Expect(onContent).To(BeFalse(),
						"subtreePlanned is snapshot-native (content has no phase): SnapshotContent %s must NOT carry it", contentName)
				}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())
			}

			By("Asserting the ROOT subtreePlanned is true — the orphan/residual wave gate opened only after the whole subtree finished planning (no premature root exclude)")
			root, err := getResource(ctx, snapshotGVR, ns, captured.rootSnap)
			Expect(err).NotTo(HaveOccurred())
			val, found := snapshotCommonControllerLatch(root, "subtreePlanned")
			Expect(found).To(BeTrue())
			Expect(val).To(BeTrue())
		})
	})
}
