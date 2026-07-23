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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
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

// findManifestOfKind returns the first object of the given kind (any name) in a decoded manifest array.
// Used where the object's name is not known up front (e.g. the VolumeSnapshot-connector restore emits a
// single PVC whose name is discovered from the output before filtering by it).
func findManifestOfKind(objs []unstructured.Unstructured, kind string) (*unstructured.Unstructured, bool) {
	for i := range objs {
		if objs[i].GetKind() == kind {
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

// subtreeIdentityKey renders the exclude/de-dup key (apiVersion|kind|namespace|name). UID is optional
// diagnostic metadata only; it does not distinguish a recreated object for plan matching.
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

		It("does NOT serve a demo node's own manifests via the core generic group (404 for both read subresources)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			// The core group serves manifests-download / manifests-with-data-restoration ONLY for the core
			// Snapshot kind. A domain kind must be addressed through its own aggregated group, so both generic
			// reads under the core group are 404 (the domain group and the VS connector are the valid entrypoints).
			for _, sub := range []string{subManifestsDownload, subManifestsRestore} {
				path := coreGenericSubPath(captured.namespace, resDemoVMSnapshots, vmSnapshot.name, sub)
				_, err := aggGet(ctx, path, nil)
				Expect(err).To(HaveOccurred(), "core group must not serve %s for domain kinds: %s", sub, path)
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected 404 NotFound for %s, got %v", path, err)
			}
		})

		It("serves a demo node's own manifests via the demo group manifests-download path", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			path := demoSubPath(captured.namespace, resDemoVMSnapshots, vmSnapshot.name, subManifestsDownload)
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

	// scope=node and the object filter narrow manifests-with-data-restoration to a single node (children
	// not read) or a single object, without introducing a new subresource (same GET, same JSON array).
	Context("scope=node and object filter (manifests-with-data-restoration)", func() {
		BeforeAll(func() {
			Expect(captured.namespace).NotTo(BeEmpty(), "capture phase must have run first")
			Expect(captured.rootSnap).NotTo(BeEmpty(), "root Snapshot must be resolved")
		})

		It("scope=node returns only the root's own manifests, not the child subtree", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			path := coreSnapshotSubPath(captured.namespace, captured.rootSnap, subManifestsRestore)
			body, err := aggGet(ctx, path, map[string]string{"scope": "node"})
			Expect(err).NotTo(HaveOccurred(), "GET %s?scope=node", path)

			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			_, hasCM := findManifest(objs, "ConfigMap", srcConfigMapName)
			Expect(hasCM).To(BeTrue(), "scope=node must return the root's own ConfigMap %s", srcConfigMapName)
			_, hasVM := findManifest(objs, "DemoVirtualMachine", srcVMName)
			Expect(hasVM).To(BeFalse(), "scope=node must NOT descend into the child subtree (DemoVirtualMachine %s belongs to a child node)", srcVMName)
		})

		It("scope=node with a kind/name filter returns exactly the addressed object, namespace-rewritten", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			const targetNS = "restore-read-probe"
			path := coreSnapshotSubPath(captured.namespace, captured.rootSnap, subManifestsRestore)
			body, err := aggGet(ctx, path, map[string]string{"scope": "node", "kind": "ConfigMap", "name": srcConfigMapName, "targetNamespace": targetNS})
			Expect(err).NotTo(HaveOccurred(), "GET %s?scope=node&kind=ConfigMap&name=%s", path, srcConfigMapName)

			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			Expect(objs).To(HaveLen(1), "the object filter must return exactly one object")
			Expect(objs[0].GetKind()).To(Equal("ConfigMap"))
			Expect(objs[0].GetName()).To(Equal(srcConfigMapName))
			Expect(objs[0].GetNamespace()).To(Equal(targetNS), "the single filtered object must be namespace-rewritten")
		})

		It("rejects an object filter without scope=node (subtree) with 400 BadRequest", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			path := coreSnapshotSubPath(captured.namespace, captured.rootSnap, subManifestsRestore)
			_, err := aggGet(ctx, path, map[string]string{"kind": "ConfigMap", "name": srcConfigMapName})
			Expect(err).To(HaveOccurred(), "an object filter at scope=subtree must be rejected")
			Expect(apierrors.IsBadRequest(err)).To(BeTrue(), "expected 400 BadRequest, got %v", err)
		})

		It("explicit scope=subtree is backward-compatible: it returns the root's own manifests just like the default", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			// The default (no scope) already serves the whole subtree; an EXPLICIT scope=subtree must parse to
			// the identical historical behavior. The root's own ConfigMap is present under both — this is the
			// regression guard that adding the scope parameter did not change the zero-parameter contract.
			path := coreSnapshotSubPath(captured.namespace, captured.rootSnap, subManifestsRestore)
			for _, params := range []map[string]string{nil, {"scope": "subtree"}} {
				body, err := aggGet(ctx, path, params)
				Expect(err).NotTo(HaveOccurred(), "GET %s (params=%v)", path, params)
				objs, err := decodeManifestArray(body)
				Expect(err).NotTo(HaveOccurred())
				_, hasCM := findManifest(objs, "ConfigMap", srcConfigMapName)
				Expect(hasCM).To(BeTrue(), "scope=%v must still return the root's own ConfigMap %s", params, srcConfigMapName)
			}
		})

		It("scope=node with a non-matching object name returns 404 NotFound", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			path := coreSnapshotSubPath(captured.namespace, captured.rootSnap, subManifestsRestore)
			_, err := aggGet(ctx, path, map[string]string{"scope": "node", "kind": "ConfigMap", "name": srcConfigMapName + "-does-not-exist"})
			Expect(err).To(HaveOccurred(), "a filter that matches nothing must not return 200")
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected 404 NotFound, got %v", err)
		})

		It("scope=node object filter is node-scoped: a kind captured only in a child subtree is 404 at the root", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			// DemoVirtualMachine vm-1 is captured by the child DemoVirtualMachineSnapshot node, NOT by the
			// root's own manifests (proven by the subtree-manifest-identities exclude). scope=node compiles ONLY
			// the root node, so a filter for the child-owned object must 404 — proving the filter is scoped to
			// the addressed node, not the whole subtree (a subtree walk would otherwise surface the VM).
			path := coreSnapshotSubPath(captured.namespace, captured.rootSnap, subManifestsRestore)
			_, err := aggGet(ctx, path, map[string]string{"scope": "node", "kind": "DemoVirtualMachine", "name": srcVMName})
			Expect(err).To(HaveOccurred(), "a child-owned object must not be restorable from the root node")
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected 404 NotFound, got %v", err)
		})

		It("scope=node object filter honors apiVersion: a matching apiVersion resolves the object, a mismatching one is 404", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			path := coreSnapshotSubPath(captured.namespace, captured.rootSnap, subManifestsRestore)

			By("a matching apiVersion (core v1) resolves the ConfigMap")
			body, err := aggGet(ctx, path, map[string]string{"scope": "node", "kind": "ConfigMap", "name": srcConfigMapName, "apiVersion": "v1"})
			Expect(err).NotTo(HaveOccurred(), "GET %s?scope=node&kind=ConfigMap&name=%s&apiVersion=v1", path, srcConfigMapName)
			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			Expect(objs).To(HaveLen(1), "an apiVersion-qualified filter must still return exactly one object")
			Expect(objs[0].GetKind()).To(Equal("ConfigMap"))
			Expect(objs[0].GetName()).To(Equal(srcConfigMapName))

			By("a mismatching apiVersion for the same kind+name is a 404 (the object is not restorable under that group)")
			_, err = aggGet(ctx, path, map[string]string{"scope": "node", "kind": "ConfigMap", "name": srcConfigMapName, "apiVersion": "apps/v1"})
			Expect(err).To(HaveOccurred(), "a kind+name present only in a different apiVersion must not match")
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected 404 NotFound for a mismatching apiVersion, got %v", err)
		})

		It("scope=node object filter output is sanitized (no status/managedFields) and namespace-rewritten", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			const targetNS = "restore-read-probe"
			path := coreSnapshotSubPath(captured.namespace, captured.rootSnap, subManifestsRestore)
			body, err := aggGet(ctx, path, map[string]string{"scope": "node", "kind": "ConfigMap", "name": srcConfigMapName, "targetNamespace": targetNS})
			Expect(err).NotTo(HaveOccurred(), "GET %s?scope=node&kind=ConfigMap&name=%s", path, srcConfigMapName)
			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			Expect(objs).To(HaveLen(1))

			cm := objs[0]
			Expect(cm.GetNamespace()).To(Equal(targetNS), "the single filtered object must be namespace-rewritten")
			_, hasStatus := cm.Object["status"]
			Expect(hasStatus).To(BeFalse(), "the filtered restore output must strip status")
			_, hasManagedFields, _ := unstructured.NestedFieldNoCopy(cm.Object, "metadata", "managedFields")
			Expect(hasManagedFields).To(BeFalse(), "the filtered restore output must strip metadata.managedFields")
		})

		It("rejects invalid scope/filter parameter combinations with 400 BadRequest", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			path := coreSnapshotSubPath(captured.namespace, captured.rootSnap, subManifestsRestore)
			// Every row is a malformed query the shared parser must reject with ErrBadRequest (400) BEFORE any
			// compilation: an unknown scope, a half-specified object filter, an apiVersion without a kind, and
			// an object filter at an explicit scope=subtree (the object filter is valid only with scope=node).
			badCases := []struct {
				name   string
				params map[string]string
			}{
				{"unknown scope", map[string]string{"scope": "bogus"}},
				{"kind without name", map[string]string{"scope": "node", "kind": "ConfigMap"}},
				{"name without kind", map[string]string{"scope": "node", "name": srcConfigMapName}},
				{"apiVersion without kind", map[string]string{"scope": "node", "apiVersion": "v1"}},
				{"object filter at explicit scope=subtree", map[string]string{"scope": "subtree", "kind": "ConfigMap", "name": srcConfigMapName}},
			}
			for _, tc := range badCases {
				_, err := aggGet(ctx, path, tc.params)
				Expect(err).To(HaveOccurred(), "%s must be rejected (params=%v)", tc.name, tc.params)
				Expect(apierrors.IsBadRequest(err)).To(BeTrue(), "%s: expected 400 BadRequest, got %v", tc.name, err)
			}
		})
	})

	// Degraded root + scope=node (ADR degraded-relax): a user-addressed root whose Ready=False carries a
	// recoverable DegradedReadyReasons reason (ChildSnapshotDeleted — a child snapshot CR was deleted while
	// its content survives in the recycle bin) still serves its OWN manifests at scope=node, while the
	// default subtree stays fail-closed (409). Runs on its OWN isolated tree so it never degrades the shared
	// `captured` tree the other specs assert Ready on (the global Ready-consistency invariant tolerates a
	// Ready=False root: it only enforces that a Ready=True parent has Ready children).
	Context("Degraded root + scope=node (manifests-with-data-restoration)", func() {
		It("serves the degraded root's own manifests at scope=node but fails the subtree closed with 409", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 4*suiteCfg.captureReadyTO+3*time.Minute)
			defer cancel()

			ns := uniqueNS("p1-degraded-node")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			By("Capturing an isolated manifest-only tree (root ConfigMap leg + a DemoVirtualMachineSnapshot child)")
			Expect(applyObjects(ctx, buildManifestOnlySource(ns), ns)).To(Succeed())
			const degradedRoot = "degraded-node-snap"
			Expect(createRootSnapshot(ctx, ns, degradedRoot)).To(Succeed())
			rootContent, err := waitSnapshotReady(ctx, ns, degradedRoot, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, rootContent, suiteCfg.captureReadyTO)).To(Succeed())

			By("Degrading the root: delete the child DemoVirtualMachineSnapshot CR while its content survives (ChildSnapshotDeleted)")
			nodes, err := walkSnapshotTree(ctx, ns, degradedRoot)
			Expect(err).NotTo(HaveOccurred())
			child, ok := firstNodeOfKind(nodes, "DemoVirtualMachineSnapshot")
			Expect(ok).To(BeTrue(), "expected a DemoVirtualMachineSnapshot child node")
			Expect(suiteDyn.Resource(demoVMSnapshotGVR).Namespace(ns).Delete(ctx, child.name, metav1.DeleteOptions{})).To(Succeed())
			Expect(waitSnapshotReadyFalseReason(ctx, ns, degradedRoot, storagev1alpha1.ReasonChildSnapshotDeleted, 2*suiteCfg.captureReadyTO+time.Minute)).
				To(Succeed(), "root must fold to Ready=False/ChildSnapshotDeleted after the child CR is deleted")

			path := coreSnapshotSubPath(ns, degradedRoot, subManifestsRestore)

			By("scope=node returns 200 with only the degraded root's own manifests (the child subtree is not read)")
			body, err := aggGet(ctx, path, map[string]string{"scope": "node"})
			Expect(err).NotTo(HaveOccurred(), "GET %s?scope=node on a degraded root must succeed", path)
			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			_, hasCM := findManifest(objs, "ConfigMap", srcConfigMapName)
			Expect(hasCM).To(BeTrue(), "scope=node must return the degraded root's own ConfigMap %s", srcConfigMapName)
			_, hasVM := findManifest(objs, "DemoVirtualMachine", srcVMName)
			Expect(hasVM).To(BeFalse(), "scope=node must NOT descend into the child subtree (DemoVirtualMachine %s belongs to the deleted child node)", srcVMName)

			By("the default (subtree) stays fail-closed with 409 Conflict on the degraded root")
			_, err = aggGet(ctx, path, nil)
			Expect(err).To(HaveOccurred(), "a degraded root must fail the whole-subtree compile closed")
			Expect(apierrors.IsConflict(err)).To(BeTrue(), "expected 409 Conflict for the subtree on a degraded root, got %v", err)
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

		It("latches commonController.childrenSettled on every node WITH children once its direct children are terminal, snapshot-native, nil on leaves (childrenSettled contract)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
			defer cancel()
			ns := captured.namespace

			nodes := []childRef{{kind: "Snapshot", name: captured.rootSnap}}
			descendants, err := walkSnapshotTree(ctx, ns, captured.rootSnap)
			Expect(err).NotTo(HaveOccurred())
			nodes = append(nodes, descendants...)

			By("Asserting childrenSettled is present+true on nodes WITH children, absent (nil) on leaves, and never on content")
			for _, node := range nodes {
				if node.kind == "VolumeSnapshot" {
					continue // CSI visibility leaf: no owning xxxSnapshot commonController of its own
				}
				gvr, ok := gvrForSnapshotKind(node.kind)
				Expect(ok).To(BeTrue(), "unknown snapshot kind %q for %s", node.kind, node.name)

				Eventually(func(g Gomega) {
					snap, err := getResource(ctx, gvr, ns, node.name)
					g.Expect(err).NotTo(HaveOccurred())
					// childrenSettled is computed over the node's DIRECT children: an aggregator (root, VM) with
					// children latches it true once every direct child is terminal (captured-OK or failed); a
					// leaf (a disk with no children) never declares it. This is the "childrenSettled appears on
					// the aggregator after its children go terminal" assertion, keyed on the actual tree shape.
					hasChildren := len(childSnapshotRefs(snap)) > 0
					val, found := snapshotCommonControllerLatch(snap, "childrenSettled")
					if hasChildren {
						g.Expect(found).To(BeTrue(), "node %s/%s has children, so it must carry commonController.childrenSettled", node.kind, node.name)
						g.Expect(val).To(BeTrue(), "node %s/%s childrenSettled must latch true once every direct child is terminal", node.kind, node.name)
					} else {
						g.Expect(found).To(BeFalse(), "leaf node %s/%s (no children) must NOT declare childrenSettled (nil)", node.kind, node.name)
					}

					contentName, _, _ := unstructured.NestedString(snap.Object, "status", "boundSnapshotContentName")
					g.Expect(contentName).NotTo(BeEmpty())
					content, err := getResource(ctx, snapshotContentGVR, "", contentName)
					g.Expect(err).NotTo(HaveOccurred())
					_, onContent := snapshotCommonControllerLatch(content, "childrenSettled")
					g.Expect(onContent).To(BeFalse(),
						"childrenSettled is snapshot-native (content has no phase): SnapshotContent %s must NOT carry it", contentName)
				}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())
			}

			By("Asserting the ROOT (always has the VM child) carries childrenSettled=true")
			root, err := getResource(ctx, snapshotGVR, ns, captured.rootSnap)
			Expect(err).NotTo(HaveOccurred())
			Expect(childSnapshotRefs(root)).NotTo(BeEmpty(), "the root must have at least the VM child so this assertion is non-vacuous")
			rval, rfound := snapshotCommonControllerLatch(root, "childrenSettled")
			Expect(rfound).To(BeTrue(), "the root has children, so it must carry childrenSettled")
			Expect(rval).To(BeTrue(), "the root childrenSettled must be true once every direct child is terminal")

			By("Asserting the DemoVirtualMachineSnapshot aggregator, when it has disk children, shows childrenSettled=true after the disks go terminal")
			if vm, ok := firstNodeOfKind(descendants, "DemoVirtualMachineSnapshot"); ok {
				vmGVR, gok := gvrForSnapshotKind(vm.kind)
				Expect(gok).To(BeTrue())
				vmSnap, err := getResource(ctx, vmGVR, ns, vm.name)
				Expect(err).NotTo(HaveOccurred())
				if len(childSnapshotRefs(vmSnap)) > 0 {
					vval, vfound := snapshotCommonControllerLatch(vmSnap, "childrenSettled")
					Expect(vfound).To(BeTrue(), "the VM snapshot has disk children, so it must carry childrenSettled")
					Expect(vval).To(BeTrue(), "the VM snapshot childrenSettled must be true once its disks are terminal")
				}
			}
		})

		It("latches commonController.childSubtreesManifestsPersisted=true on every xxxSnapshot node (main-written, snapshot-native, vacuously true on childless leaves), never on content (childSubtreesManifestsPersisted contract)", func() {
			// childSubtreesManifestsPersisted (capture_legs.go): main writes this children-only aggregate
			// SIDEWAYS onto every xxxSnapshot — "the subtrees of ALL declared direct children are fully
			// persisted" — DECLARED false at capture barrier 1 and monotonically flipped to true, never left
			// nil (a nil field silently disables the SDK manifest-exclude pre-gate). Because it deliberately
			// excludes the node's OWN manifests (those are manifestCaptured), a childless node is vacuously
			// true and latches on the first pass. On a fully Ready tree every node therefore carries it true.
			// It is snapshot-native: the content keeps its own subtreeManifestsPersisted latch and must NEVER
			// carry this mirror.
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
			defer cancel()
			ns := captured.namespace

			nodes := []childRef{{kind: "Snapshot", name: captured.rootSnap}}
			descendants, err := walkSnapshotTree(ctx, ns, captured.rootSnap)
			Expect(err).NotTo(HaveOccurred())
			nodes = append(nodes, descendants...)

			By("Asserting every node carries childSubtreesManifestsPersisted=true on its xxxSnapshot and never on its content")
			for _, node := range nodes {
				if node.kind == "VolumeSnapshot" {
					continue // CSI visibility leaf: no owning xxxSnapshot commonController of its own
				}
				gvr, ok := gvrForSnapshotKind(node.kind)
				Expect(ok).To(BeTrue(), "unknown snapshot kind %q for %s", node.kind, node.name)

				Eventually(func(g Gomega) {
					snap, err := getResource(ctx, gvr, ns, node.name)
					g.Expect(err).NotTo(HaveOccurred())
					val, found := snapshotCommonControllerLatch(snap, "childSubtreesManifestsPersisted")
					g.Expect(found).To(BeTrue(), "node %s/%s must carry commonController.childSubtreesManifestsPersisted (declared false then latched true; never left nil past barrier 1)", node.kind, node.name)
					g.Expect(val).To(BeTrue(), "node %s/%s childSubtreesManifestsPersisted must latch true once every declared child subtree is persisted (a childless node is vacuously true)", node.kind, node.name)

					// Contrast with childrenSettled (nil on a leaf): childSubtreesManifestsPersisted IS declared
					// on a childless node too, latching vacuously true — this is what keeps it usable as the SDK
					// manifest-exclude pre-gate. Pin that difference on every leaf node in the tree.
					if len(childSnapshotRefs(snap)) == 0 {
						_, settledFound := snapshotCommonControllerLatch(snap, "childrenSettled")
						g.Expect(settledFound).To(BeFalse(),
							"leaf node %s/%s must NOT declare childrenSettled (nil) while it DOES declare childSubtreesManifestsPersisted — the two latches differ on leaves", node.kind, node.name)
					}

					contentName, _, _ := unstructured.NestedString(snap.Object, "status", "boundSnapshotContentName")
					g.Expect(contentName).NotTo(BeEmpty())
					content, err := getResource(ctx, snapshotContentGVR, "", contentName)
					g.Expect(err).NotTo(HaveOccurred())
					_, onContent := snapshotCommonControllerLatch(content, "childSubtreesManifestsPersisted")
					g.Expect(onContent).To(BeFalse(),
						"childSubtreesManifestsPersisted is snapshot-native: SnapshotContent %s must NOT carry it (the content keeps its own subtreeManifestsPersisted latch)", contentName)
				}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())
			}

			By("Asserting the ROOT (always has the VM child) carries childSubtreesManifestsPersisted=true so the aggregate is non-vacuous")
			root, err := getResource(ctx, snapshotGVR, ns, captured.rootSnap)
			Expect(err).NotTo(HaveOccurred())
			Expect(childSnapshotRefs(root)).NotTo(BeEmpty(), "the root must have at least the VM child so this assertion is non-vacuous")
			rval, rfound := snapshotCommonControllerLatch(root, "childSubtreesManifestsPersisted")
			Expect(rfound).To(BeTrue(), "the root has children, so it must carry childSubtreesManifestsPersisted")
			Expect(rval).To(BeTrue(), "the root childSubtreesManifestsPersisted must be true once every declared child subtree is persisted")
		})

		It("composes whole-subtree-persisted as manifestCaptured && childSubtreesManifestsPersisted, and gates the root manifest-exclude on it (design §8.3, SDK pre-gate)", func() {
			// The two core-owned latches decompose "the whole node subtree is durably persisted":
			// manifestCaptured (this node's OWN manifests handed off) AND childSubtreesManifestsPersisted (every
			// declared child subtree persisted). On a Ready tree both are true on every node. The children-only
			// half is the SDK manifest-exclude pre-gate: SubtreeManifestIdentities returns 409 without a REST
			// call while it is false, so the root only computes its exclude once its children's subtrees are
			// persisted — the black-box effect is that, once the root latch is true, the fail-closed
			// subtree-manifest-identities endpoint is servable and the child-captured VM is dropped from the
			// root own-manifests (no double capture).
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
			defer cancel()
			ns := captured.namespace

			nodes := []childRef{{kind: "Snapshot", name: captured.rootSnap}}
			descendants, err := walkSnapshotTree(ctx, ns, captured.rootSnap)
			Expect(err).NotTo(HaveOccurred())
			nodes = append(nodes, descendants...)

			By("Asserting every node satisfies the decomposition manifestCaptured && childSubtreesManifestsPersisted == true")
			for _, node := range nodes {
				if node.kind == "VolumeSnapshot" {
					continue // CSI visibility leaf: no owning xxxSnapshot commonController of its own
				}
				gvr, ok := gvrForSnapshotKind(node.kind)
				Expect(ok).To(BeTrue(), "unknown snapshot kind %q for %s", node.kind, node.name)
				Eventually(func(g Gomega) {
					snap, err := getResource(ctx, gvr, ns, node.name)
					g.Expect(err).NotTo(HaveOccurred())
					own, ownFound := snapshotCommonControllerLatch(snap, "manifestCaptured")
					children, childrenFound := snapshotCommonControllerLatch(snap, "childSubtreesManifestsPersisted")
					g.Expect(ownFound).To(BeTrue(), "node %s/%s must carry manifestCaptured", node.kind, node.name)
					g.Expect(childrenFound).To(BeTrue(), "node %s/%s must carry childSubtreesManifestsPersisted", node.kind, node.name)
					g.Expect(own && children).To(BeTrue(),
						"node %s/%s whole subtree persisted == manifestCaptured(%v) && childSubtreesManifestsPersisted(%v)", node.kind, node.name, own, children)
				}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())
			}

			By("Asserting the root latch gates the manifest-exclude: once childSubtreesManifestsPersisted=true, subtree-manifest-identities is servable and carries the child-captured VM")
			var own []unstructured.Unstructured
			Eventually(func(g Gomega) {
				root, err := getResource(ctx, snapshotGVR, ns, captured.rootSnap)
				g.Expect(err).NotTo(HaveOccurred())
				val, found := snapshotCommonControllerLatch(root, "childSubtreesManifestsPersisted")
				g.Expect(found && val).To(BeTrue(), "root childSubtreesManifestsPersisted must be true (the manifest-exclude pre-gate is open)")

				body, err := aggGet(ctx, coreContentSubPath(captured.rootContent, subManifestsIdentities), nil)
				g.Expect(err).NotTo(HaveOccurred(),
					"with the pre-gate open the fail-closed subtree-manifest-identities endpoint must be servable (409 => a child subtree is not persisted yet)")
				ids, err := decodeSubtreeIdentities(body)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(hasSubtreeIdentity(ids, "DemoVirtualMachine", srcVMName)).To(BeTrue(),
					"the child DemoVirtualMachineSnapshot subtree must contribute DemoVirtualMachine %s to the exclude set", srcVMName)

				own, err = getRootOwnManifests(ctx, ns, captured.rootSnap)
				g.Expect(err).NotTo(HaveOccurred(), "root own-manifests are served only after main latches manifestCaptured")
			}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())

			By("Asserting the pre-gated exclude dropped the child-captured VM from the root own-manifests without over-dropping the root's own ConfigMap")
			_, reCaptured := findManifest(own, "DemoVirtualMachine", srcVMName)
			Expect(reCaptured).To(BeFalse(),
				"the child-captured DemoVirtualMachine %s must be dropped from the root own-manifests by the pre-gated exclude (no double capture)", srcVMName)
			_, hasCM := findManifest(own, "ConfigMap", srcConfigMapName)
			Expect(hasCM).To(BeTrue(),
				"the pre-gated exclude must not over-drop: the root-owned ConfigMap %s must remain in the root own-manifests", srcConfigMapName)
		})
	})
}
