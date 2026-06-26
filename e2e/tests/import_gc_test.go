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
	"k8s.io/apimachinery/pkg/types"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

const importRootSnapshotName = "import-root"

// createImportRootSnapshot creates an import-mode root Snapshot (spec.source.import: {}). The controller
// holds it pending until the per-node manifests are uploaded, then materializes its SnapshotContent.
func createImportRootSnapshot(ctx context.Context, ns, name string) error {
	snap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "storage.deckhouse.io/v1alpha1",
		"kind":       "Snapshot",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"source": map[string]interface{}{
				"import": map[string]interface{}{},
			},
		},
	}}
	_, err := suiteDyn.Resource(snapshotGVR).Namespace(ns).Create(ctx, snap, metav1.CreateOptions{})
	return err
}

// buildUploadBody assembles the manifests-and-children-refs-upload payload: the node's own manifests
// (verbatim, the manifests-download array shape) plus its direct child refs (empty for a leaf root).
func buildUploadBody(ownManifests []byte, childRefs []childRef) ([]byte, error) {
	refs := make([]map[string]string, 0, len(childRefs))
	for _, c := range childRefs {
		refs = append(refs, map[string]string{"apiVersion": c.apiVersion, "kind": c.kind, "name": c.name})
	}
	return json.Marshal(map[string]interface{}{
		"manifests": json.RawMessage(ownManifests),
		"childRefs": refs,
	})
}

// importSpecs registers the phase-2 export -> import round-trip specs. It exports the captured root's own
// manifests, then reconstructs a structural root SnapshotContent in a fresh namespace through the
// import-mode Snapshot + manifests-and-children-refs-upload path and asserts it reaches Ready.
//
// Scope note: only the structural root node is reconstructed (empty childRefs). A full multi-node demo
// tree import is not a client-drivable path — intermediate demo nodes (DemoVirtualMachineSnapshot) expose
// no client-settable import marker; their import-mode CRs would have to be created by the domain
// controller. This spec covers the upload transport + import orchestrator materialization contract.
func importSpecs() {
	Context("Phase 2: export -> import round-trip", func() {
		var importNS string

		BeforeAll(func() {
			Expect(captured.namespace).NotTo(BeEmpty(), "capture phase must have run first")
			importNS = uniqueNS("import")

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			Expect(ensureNamespace(ctx, importNS)).To(Succeed())
			DeferCleanup(func() {
				if cleanupSkippedOnFailure() {
					GinkgoWriter.Printf("E2E_KEEP_CLUSTER_ON_FAILURE: keeping import namespace %q and root Snapshot (spec failed)\n", importNS)
					return
				}
				cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer ccancel()
				_ = suiteDyn.Resource(snapshotGVR).Namespace(importNS).Delete(cctx, importRootSnapshotName, metav1.DeleteOptions{})
				deleteNamespace(cctx, importNS)
			})
		})

		It("reconstructs a structural root SnapshotContent from an uploaded manifest payload", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.snapshotReadyTO+2*time.Minute)
			defer cancel()

			By("Exporting the captured root's own manifests via manifests-download")
			dlPath := coreSnapshotSubPath(captured.namespace, captured.rootSnap, subManifestsDownload)
			ownManifests, err := aggGet(ctx, dlPath, nil)
			Expect(err).NotTo(HaveOccurred(), "GET %s", dlPath)

			By("Creating an import-mode root Snapshot in a fresh namespace")
			Expect(createImportRootSnapshot(ctx, importNS, importRootSnapshotName)).To(Succeed())

			By("Uploading the node's manifests (empty childRefs) via manifests-and-children-refs-upload")
			uploadBody, err := buildUploadBody(ownManifests, nil)
			Expect(err).NotTo(HaveOccurred())
			ulPath := coreSnapshotSubPath(importNS, importRootSnapshotName, subManifestsUpload)
			Eventually(func() error {
				_, postErr := aggPost(ctx, ulPath, uploadBody)
				return postErr
			}).WithTimeout(2*time.Minute).WithPolling(pollInterval).Should(Succeed(), "POST %s", ulPath)

			By("Waiting for the import Snapshot to become Ready (content materialized)")
			// Manifest-only import (uploaded payload, no volume-data streaming) materializes fast.
			content, err := waitSnapshotReady(ctx, importNS, importRootSnapshotName, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(content).NotTo(BeEmpty())

			By("Asserting the reconstructed SnapshotContent reaches all leg conditions")
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())
		})
	})
}

const gcRootSnapshotName = "gc-tree"

// patchModuleSnapshotRootOkTtl sets (value != nil) or clears (value == nil) the module's
// settings.snapshotRootOkTtl via a merge patch. Clearing uses a JSON null to drop the key.
func patchModuleSnapshotRootOkTtl(ctx context.Context, value *string) error {
	var raw string
	if value == nil {
		raw = `{"spec":{"settings":{"snapshotRootOkTtl":null}}}`
	} else {
		raw = fmt.Sprintf(`{"spec":{"settings":{"snapshotRootOkTtl":%q}}}`, *value)
	}
	_, err := suiteDyn.Resource(moduleConfigGVR).Patch(ctx, moduleName, types.MergePatchType, []byte(raw), metav1.PatchOptions{})
	return err
}

// waitRootOkTTL waits until the root ObjectKeeper for ns/snap reports spec.ttl == want. The controller
// re-aligns the OK TTL to the live config on every reconcile, so this confirms the new snapshotRootOkTtl
// has actually propagated to a running controller before the GC timing assertions run.
func waitRootOkTTL(ctx context.Context, ns, snap string, want, timeout time.Duration) error {
	okName := fmt.Sprintf("ret-snap-%s-%s", ns, snap)
	deadline := time.Now().Add(timeout)
	var last string
	for {
		ok, err := getResource(ctx, objectKeeperGVR, "", okName)
		if err == nil {
			if ttlStr, found, _ := unstructured.NestedString(ok.Object, "spec", "ttl"); found {
				if d, perr := time.ParseDuration(ttlStr); perr == nil && d == want {
					return nil
				} else {
					last = fmt.Sprintf("ttl=%q", ttlStr)
				}
			} else {
				last = "no spec.ttl"
			}
		} else {
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for root ObjectKeeper %s ttl==%s; last: %s", okName, want, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// childContentNames returns the direct child SnapshotContent names from a content's
// status.childrenSnapshotContentRefs.
func childContentNames(content *unstructured.Unstructured) []string {
	refs, _, _ := unstructured.NestedSlice(content.Object, "status", "childrenSnapshotContentRefs")
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if m, ok := r.(map[string]interface{}); ok {
			if n, _, _ := unstructured.NestedString(m, "name"); n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}

// gcSpecs registers the phase-2 TTL/GC cascade specs: with a short snapshotRootOkTtl, deleting the root
// Snapshot must retain its (Retain-policy) SnapshotContent for the TTL window, then the ObjectKeeper GCs
// the root content and the ownerRef chain cascades to the child contents and the root ManifestCheckpoint.
func gcSpecs() {
	Context("Phase 2: TTL / GC cascade", func() {
		var (
			gcNS       string
			prevTTL    string
			prevTTLSet bool
		)

		BeforeAll(func() {
			gcNS = uniqueNS("gc")

			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.moduleReadyTO+5*time.Minute)
			defer cancel()

			mc, err := getResource(ctx, moduleConfigGVR, "", moduleName)
			Expect(err).NotTo(HaveOccurred(), "read ModuleConfig")
			prevTTL, prevTTLSet, _ = unstructured.NestedString(mc.Object, "spec", "settings", "snapshotRootOkTtl")

			// Register the rollback BEFORE mutating the ModuleConfig so a failed patch or readiness wait
			// below still restores the original snapshotRootOkTtl instead of leaking the short TTL.
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 2*suiteCfg.moduleReadyTO+5*time.Minute)
				defer ccancel()
				if prevTTLSet {
					_ = patchModuleSnapshotRootOkTtl(cctx, &prevTTL)
				} else {
					_ = patchModuleSnapshotRootOkTtl(cctx, nil)
				}
				_ = storagekube.WaitForModuleReady(cctx, suiteRestCfg, moduleName, suiteCfg.moduleReadyTO)
				deleteNamespace(cctx, gcNS)
			})

			By("Setting a short snapshotRootOkTtl (" + suiteCfg.gcTTL + ") and waiting for re-converge")
			ttl := suiteCfg.gcTTL
			Expect(patchModuleSnapshotRootOkTtl(ctx, &ttl)).To(Succeed())
			Expect(storagekube.WaitForModuleReady(ctx, suiteRestCfg, moduleName, suiteCfg.moduleReadyTO)).To(Succeed())
		})

		It("retains the root SnapshotContent then TTL-GCs it and cascades to children + checkpoint", func() {
			gcTTLDur, perr := time.ParseDuration(suiteCfg.gcTTL)
			Expect(perr).NotTo(HaveOccurred(), "E2E_GC_TTL must be a Go duration")

			// Budget for three sequential snapshotReadyTO-bounded waits (Snapshot Ready, content legs, OK
			// TTL propagation), the retain window, and the post-deletion GC deadline (gcTTLDur+4m) with a
			// buffer for the cascading child/checkpoint assertions.
			ctx, cancel := context.WithTimeout(context.Background(), gcTTLDur+3*suiteCfg.snapshotReadyTO+8*time.Minute)
			defer cancel()

			By("Capturing a dedicated GC tree under the short TTL")
			Expect(ensureNamespace(ctx, gcNS)).To(Succeed())
			Expect(applyObjects(ctx, buildManifestOnlySource(gcNS), gcNS)).To(Succeed())
			Expect(createRootSnapshot(ctx, gcNS, gcRootSnapshotName)).To(Succeed())

			content, err := waitSnapshotReady(ctx, gcNS, gcRootSnapshotName, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())

			By("Recording the content graph (children + ManifestCheckpoint) before deletion")
			co, err := getResource(ctx, snapshotContentGVR, "", content)
			Expect(err).NotTo(HaveOccurred())
			children := childContentNames(co)
			checkpoint, _, _ := unstructured.NestedString(co.Object, "status", "manifestCheckpointName")
			GinkgoWriter.Printf("GC root content=%s children=%v checkpoint=%s\n", content, children, checkpoint)

			By("Confirming the short TTL has propagated to the root ObjectKeeper")
			Expect(waitRootOkTTL(ctx, gcNS, gcRootSnapshotName, gcTTLDur, suiteCfg.captureReadyTO)).To(Succeed())

			By("Deleting the root Snapshot")
			Expect(suiteDyn.Resource(snapshotGVR).Namespace(gcNS).Delete(ctx, gcRootSnapshotName, metav1.DeleteOptions{})).To(Succeed())

			// Retain window: a fraction of the TTL, capped so the spec stays quick. Skipped entirely for very
			// short TTLs where the OK countdown could elapse before a meaningful observation window. Only an
			// early NotFound fails the check; transient get errors are tolerated (the content may still exist).
			retainWindow := gcTTLDur / 2
			if retainWindow > 15*time.Second {
				retainWindow = 15 * time.Second
			}
			if retainWindow >= 2*time.Second {
				By("Asserting the SnapshotContent is RETAINED right after Snapshot deletion")
				Consistently(func() error {
					_, e := getResource(ctx, snapshotContentGVR, "", content)
					if errIsNotFound(e) {
						return e
					}
					return nil
				}).WithContext(ctx).WithTimeout(retainWindow).WithPolling(time.Second).Should(Succeed(), "content must survive the detach (TTL not yet elapsed)")
			} else {
				GinkgoWriter.Printf("skipping retain-window assertion: gcTTL %s too short for a stable observation window\n", gcTTLDur)
			}

			By("Asserting the root SnapshotContent is GC'd after the TTL")
			gcDeadline := gcTTLDur + 4*time.Minute
			assertResourceGone(ctx, snapshotContentGVR, "", content, gcDeadline)

			By("Asserting the ownerRef cascade removed the child contents")
			for _, cc := range children {
				assertResourceGone(ctx, snapshotContentGVR, "", cc, gcDeadline)
			}

			if checkpoint != "" {
				By("Asserting the root ManifestCheckpoint is GC'd")
				assertResourceGone(ctx, manifestCheckpointGVR, "", checkpoint, gcDeadline)
			}
		})
	})
}
