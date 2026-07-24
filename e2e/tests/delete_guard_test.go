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
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// envDeleteGuard gates the delete-guard e2e. It is opt-in because the ValidatingAdmissionPolicy must be
// installed AND switched to enforcement=Deny on the cluster (the module ships in Audit; switching to Deny
// is a deliberate operator rollout action after the backfill gate is proven — plan P4). Under Audit these
// deny-expecting specs would fail, so the suite only runs them when the operator has enabled enforcement.
const envDeleteGuard = "E2E_DELETE_GUARD"

// A non-exempt identity for the marker-immutability / direct-delete cases. Group system:masters bypasses
// RBAC authorization so the request actually reaches admission; the username is NOT in the guard's exempt
// list, so the guard (not RBAC) is what denies it. This isolates the admission behavior from RBAC.
const (
	nonExemptUser = "e2e-nonexempt-admin"
	// exemptController impersonates our controller SA (an exempt actor) to prove an exempt actor may set
	// and keep the marker.
	exemptControllerUser = "system:serviceaccount:d8-state-snapshotter:controller"
)

var mastersGroup = []string{"system:masters"}

// deleteGuardSpecs registers the unified-snapshot delete-guard e2e (plan P6). It builds its OWN manifest
// tree so its destructive cases never disturb the shared `captured` tree, then exercises: DELETE-deny of
// protected internal nodes, break-glass override (marker persists), UPDATE marker-immutability, exempt
// actor, fail-fast degradation, root free delete + cascade teardown, and (volume-data-gated) managed
// VS/VSC protection plus foreign-object non-interference.
func deleteGuardSpecs() {
	Context("Delete guard (unified snapshot delete protection)", func() {
		if !envBool(os.Getenv(envDeleteGuard)) {
			// Register a single skipped spec so the suite documents why the block did not run.
			It("is skipped unless "+envDeleteGuard+"=true (needs admission enforcement=Deny)", func() {
				Skip(envDeleteGuard + " not set: the delete-guard VAP must be installed and switched to enforcement=Deny")
			})
			return
		}

		var (
			ns          string
			rootSnap    = "dg-root"
			rootContent string
			childNode   childRef
		)

		BeforeAll(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*suiteCfg.captureReadyTO+2*time.Minute)
			defer cancel()

			ns = uniqueNS("delete-guard")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())

			By("Capturing a manifest-only tree to protect")
			Expect(applyObjects(ctx, buildManifestOnlySource(ns), ns)).To(Succeed())
			Expect(createRootSnapshot(ctx, ns, rootSnap)).To(Succeed())
			var err error
			rootContent, err = waitSnapshotReady(ctx, ns, rootSnap, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, rootContent, suiteCfg.captureReadyTO)).To(Succeed())

			nodes, err := walkSnapshotTree(ctx, ns, rootSnap)
			Expect(err).NotTo(HaveOccurred())
			var ok bool
			childNode, ok = firstNodeOfKind(nodes, "DemoVirtualMachineSnapshot")
			if !ok && len(nodes) > 0 {
				childNode, ok = nodes[0], true
			}
			Expect(ok).To(BeTrue(), "expected at least one child snapshot node in the tree")
		})

		AfterAll(func() {
			// Best-effort teardown: root is unprotected, so deleting it cascades the whole tree; then reap
			// the namespace. Honors keep-on-failure/keep-always via deleteNamespace.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			_ = suiteDyn.Resource(snapshotGVR).Namespace(ns).Delete(ctx, rootSnap, metav1.DeleteOptions{})
			deleteNamespace(ctx, ns)
		})

		It("denies direct DELETE of a protected child snapshot (scenario 1/3)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			gvr, ok := gvrForSnapshotKind(childNode.kind)
			Expect(ok).To(BeTrue())
			err := suiteDyn.Resource(gvr).Namespace(ns).Delete(ctx, childNode.name, metav1.DeleteOptions{})
			Expect(err).To(HaveOccurred(), "direct DELETE of a protected child must be denied")
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "denial must be a Forbidden admission error: %v", err)
		})

		It("denies direct DELETE of protected SnapshotContent / ObjectKeeper / MCP / chunk (scenario 4/5)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			By("SnapshotContent (cluster-scoped)")
			expectDeleteForbidden(ctx, snapshotContentGVR, "", rootContent)

			for _, tc := range []struct {
				name string
				gvr  schema.GroupVersionResource
				ns   string
			}{
				{"ObjectKeeper", objectKeeperGVR, ""},
				{"ManifestCheckpoint", manifestCheckpointGVR, ""},
				{"ManifestCheckpointContentChunk", manifestCheckpointContentChunkGVR, ""},
			} {
				name, found, err := firstProtectedName(ctx, tc.gvr, tc.ns)
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(BeTrue(), "expected at least one protected %s", tc.name)
				expectDeleteForbidden(ctx, tc.gvr, tc.ns, name)
			}
		})

		It("forbids removing or changing the delete-protected marker via UPDATE for a non-exempt user (scenario 10/11)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			dyn, err := impersonatedDyn(nonExemptUser, mastersGroup...)
			Expect(err).NotTo(HaveOccurred())

			By("Attempting to strip the marker label")
			cur, err := suiteDyn.Resource(snapshotContentGVR).Get(ctx, rootContent, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			stripped := cur.DeepCopy()
			labels := stripped.GetLabels()
			delete(labels, deleteProtectedLabel)
			stripped.SetLabels(labels)
			_, err = dyn.Resource(snapshotContentGVR).Update(ctx, stripped, metav1.UpdateOptions{})
			Expect(err).To(HaveOccurred(), "removing the marker must be denied")
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "must be a Forbidden admission error: %v", err)

			By("Attempting to change the marker value")
			cur, err = suiteDyn.Resource(snapshotContentGVR).Get(ctx, rootContent, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			changed := cur.DeepCopy()
			changed.SetLabels(map[string]string{deleteProtectedLabel: "false"})
			_, err = dyn.Resource(snapshotContentGVR).Update(ctx, changed, metav1.UpdateOptions{})
			Expect(err).To(HaveOccurred(), "changing the marker value must be denied")
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "must be a Forbidden admission error: %v", err)
		})

		It("allows an exempt controller to remove/change the marker that a non-exempt user cannot (scenario 12)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			dyn, err := impersonatedDyn(exemptControllerUser, mastersGroup...)
			Expect(err).NotTo(HaveOccurred())

			// The exemption is only meaningful for the op a non-exempt user is DENIED (removing/changing the
			// marker). Re-writing the same "true" value is allowed for everyone, so it would not exercise the
			// exempt path — strip the marker as the exempt actor, then restore it so later specs stay protected.
			By("Exempt actor strips the marker (denied for a non-exempt user in the prior spec)")
			cur, err := suiteDyn.Resource(snapshotContentGVR).Get(ctx, rootContent, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			labels := cur.GetLabels()
			delete(labels, deleteProtectedLabel)
			cur.SetLabels(labels)
			_, err = dyn.Resource(snapshotContentGVR).Update(ctx, cur, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred(), "an exempt controller must be able to remove the marker")

			By("Exempt actor restores the marker so downstream specs keep a protected content")
			cur, err = suiteDyn.Resource(snapshotContentGVR).Get(ctx, rootContent, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			labels = cur.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			labels[deleteProtectedLabel] = "true"
			cur.SetLabels(labels)
			_, err = dyn.Resource(snapshotContentGVR).Update(ctx, cur, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred(), "an exempt controller must be able to re-add the marker")
		})

		It("break-glass allows DELETE while the marker persists; the child degrades before it disappears (scenario 2/8/13)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			gvr, ok := gvrForSnapshotKind(childNode.kind)
			Expect(ok).To(BeTrue())

			By("Adding a hold finalizer so the object survives in Terminating for inspection")
			cur, err := suiteDyn.Resource(gvr).Namespace(ns).Get(ctx, childNode.name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			cur.SetFinalizers(append(cur.GetFinalizers(), holdFinalizer))
			_, err = suiteDyn.Resource(gvr).Namespace(ns).Update(ctx, cur, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
				defer ccancel()
				obj, gerr := suiteDyn.Resource(gvr).Namespace(ns).Get(cctx, childNode.name, metav1.GetOptions{})
				if gerr != nil {
					return
				}
				obj.SetFinalizers(nil)
				_, _ = suiteDyn.Resource(gvr).Namespace(ns).Update(cctx, obj, metav1.UpdateOptions{})
			})

			By("Annotating break-glass and issuing DELETE (admitted by the annotation)")
			Expect(annotateAllowDelete(ctx, gvr, ns, childNode.name)).To(Succeed())
			Expect(suiteDyn.Resource(gvr).Namespace(ns).Delete(ctx, childNode.name, metav1.DeleteOptions{})).To(Succeed())

			By("Asserting the object is Terminating AND still carries the marker (proves the annotation, not marker removal, admitted the DELETE)")
			Eventually(func(g Gomega) {
				obj, gerr := suiteDyn.Resource(gvr).Namespace(ns).Get(ctx, childNode.name, metav1.GetOptions{})
				g.Expect(gerr).NotTo(HaveOccurred())
				g.Expect(obj.GetDeletionTimestamp()).NotTo(BeNil(), "object must be Terminating")
				g.Expect(isProtected(obj)).To(BeTrue(), "marker must persist through break-glass DELETE")
			}).WithTimeout(time.Minute).WithPolling(3 * time.Second).Should(Succeed())

			By("Asserting the root fail-fast degrades to Ready=False before the child physically disappears (scenario 8)")
			Expect(waitObjectCondition(ctx, snapshotGVR, ns, rootSnap, condReady, "False", suiteCfg.captureReadyTO+time.Minute)).To(Succeed())
		})

		It("lets a user remove the break-glass annotation while DELETE has not started (reversible, scenario 15)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			// Use a protected ObjectKeeper (not slated for deletion in this spec) to prove reversibility.
			name, found, err := firstProtectedName(ctx, objectKeeperGVR, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())

			Expect(annotateAllowDelete(ctx, objectKeeperGVR, "", name)).To(Succeed())
			By("Removing the annotation again — allowed, object stays present and protected")
			cur, err := suiteDyn.Resource(objectKeeperGVR).Get(ctx, name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			ann := cur.GetAnnotations()
			delete(ann, breakGlassAnnotation)
			cur.SetAnnotations(ann)
			_, err = suiteDyn.Resource(objectKeeperGVR).Update(ctx, cur, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred(), "removing break-glass before DELETE must be allowed (reversible)")

			after, err := suiteDyn.Resource(objectKeeperGVR).Get(ctx, name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(isProtected(after)).To(BeTrue())
		})

		It("allows free update and delete of the unmarked root Snapshot, cascading the whole tree (scenario 6/14)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()

			By("Root has no marker and can be freely updated")
			root, err := suiteDyn.Resource(snapshotGVR).Namespace(ns).Get(ctx, rootSnap, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(isProtected(root)).To(BeFalse(), "root Snapshot must NOT be marked")
			ann := root.GetAnnotations()
			if ann == nil {
				ann = map[string]string{}
			}
			ann["e2e.state-snapshotter.deckhouse.io/touch"] = "1"
			root.SetAnnotations(ann)
			_, err = suiteDyn.Resource(snapshotGVR).Namespace(ns).Update(ctx, root, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred(), "unmarked root must be freely updatable")

			By("Deleting the root directly (no break-glass) tears down the tree")
			// Release the hold finalizer left by the break-glass spec so the cascade can complete.
			if gvr, ok := gvrForSnapshotKind(childNode.kind); ok {
				if obj, gerr := suiteDyn.Resource(gvr).Namespace(ns).Get(ctx, childNode.name, metav1.GetOptions{}); gerr == nil {
					obj.SetFinalizers(nil)
					_, _ = suiteDyn.Resource(gvr).Namespace(ns).Update(ctx, obj, metav1.UpdateOptions{})
				}
			}
			Expect(suiteDyn.Resource(snapshotGVR).Namespace(ns).Delete(ctx, rootSnap, metav1.DeleteOptions{})).To(Succeed())
			assertResourceGone(ctx, snapshotGVR, ns, rootSnap, 3*time.Minute)
		})

		It("protects a managed CSI VolumeSnapshot/Content but never a foreign one (scenario 5/7/9)", func() {
			if !suiteCfg.volumeData {
				Skip(envVolumeData + " not set: managed VS/VSC protection needs the volume-data tree")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			By("A managed (marked) VolumeSnapshotContent rejects direct DELETE")
			vscName, found, err := firstProtectedName(ctx, volumeSnapshotContentGVR, "")
			Expect(err).NotTo(HaveOccurred())
			if !found {
				Skip("no managed VolumeSnapshotContent present in this cluster state")
			}
			expectDeleteForbidden(ctx, volumeSnapshotContentGVR, "", vscName)

			By("A foreign/standalone VolumeSnapshot without the marker is freely deletable (no false positive)")
			foreignNS := uniqueNS("dg-foreign")
			Expect(ensureNamespace(ctx, foreignNS)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), foreignNS) })
			foreignVS := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "snapshot.storage.k8s.io/v1",
				"kind":       "VolumeSnapshot",
				"metadata":   map[string]interface{}{"name": "dg-foreign-vs", "namespace": foreignNS},
				"spec": map[string]interface{}{
					"source": map[string]interface{}{"persistentVolumeClaimName": "does-not-exist"},
				},
			}}
			_, err = suiteDyn.Resource(volumeSnapshotGVR).Namespace(foreignNS).Create(ctx, foreignVS, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(suiteDyn.Resource(volumeSnapshotGVR).Namespace(foreignNS).Delete(ctx, "dg-foreign-vs", metav1.DeleteOptions{})).
				To(Succeed(), "an unmarked foreign VolumeSnapshot must be freely deletable")
		})
	})
}

// expectDeleteForbidden asserts that a direct DELETE of a (possibly cluster-scoped) resource is rejected
// by the admission delete-guard with a Forbidden error.
func expectDeleteForbidden(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) {
	GinkgoHelper()
	var err error
	if ns == "" {
		err = suiteDyn.Resource(gvr).Delete(ctx, name, metav1.DeleteOptions{})
	} else {
		err = suiteDyn.Resource(gvr).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	}
	Expect(err).To(HaveOccurred(), "direct DELETE of protected %s/%s must be denied", gvr.Resource, name)
	Expect(apierrors.IsForbidden(err)).To(BeTrue(), "denial must be a Forbidden admission error: %v", err)
}
