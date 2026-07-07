//go:build integration
// +build integration

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

package integration

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// Label("isolated"): these root-MCR residual-convergence specs run in their own `go test` pass (fresh
// envtest + manager) via the `isolated` label filter wired in the Makefile, because they install the
// cluster-scoped CSI VolumeSnapshotClass/VolumeSnapshotContent CRDs (see pr7InstallCSIClassAndContentCRDs)
// that the shared !isolated suite deliberately omits — several !isolated specs rely on
// VolumeSnapshotContent being absent. Keep Serial too: even within their own pass they must not interleave.
//
// wave7 orphan model: a residual/loose PVC in a namespace-root capture (one not covered by a domain child
// subtree) is captured as its OWN standalone child volume node (own SnapshotContent + dataRef + own
// ManifestCheckpoint holding that PVC's manifest), not appended to the root aggregator MCR. The root MCR
// therefore never carries a PVC manifest. envtest ships no external-snapshotter, so these specs run a fake
// CSI sidecar (pr7StartFakeExternalSnapshotter) that binds the orphan VolumeSnapshots the controller
// creates, letting the orphan wave complete end to end.
var _ = Describe("Integration: N5 PR-7 orphan-PVC child volume nodes", Serial, Ordered, Label("isolated"), func() {
	BeforeAll(func() {
		ctx := context.Background()
		pr7InstallCSIClassAndContentCRDs(ctx)
		pr7EnsureSharedCSIClasses(ctx)
	})

	It("residual CSI PVCs each become their own child volume node; root MCR carries no PVC manifest", func() {
		ctx := context.Background()
		reactorCtx, cancel := context.WithCancel(ctx)
		DeferCleanup(cancel)

		ns := pr7CreateNamespace(ctx, "n5-pr7-orphan")
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})
		pr7StartFakeExternalSnapshotter(reactorCtx, nsName)

		// Both PVCs are loose/residual (no domain child covers them), so under Variant A each becomes its
		// own standalone child volume node and neither is planned into the root aggregator MCR.
		pvcA := pr7CreateCSIPVC(ctx, nsName, "pvc-a")
		pvcB := pr7CreateCSIPVC(ctx, nsName, "pvc-b")

		rootName := "pr7-root"
		Expect(k8sClient.Create(ctx, &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: rootName, Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		})).To(Succeed())
		rootKey := types.NamespacedName{Namespace: nsName, Name: rootName}
		rootSnap := pr7WaitSnapshotBound(ctx, rootKey)

		// The root MCR is created only after the orphan wave completes (both child volume nodes linked and
		// ready); when it exists it carries no PVC manifest — both residual PVCs are excluded up front.
		Eventually(func(g Gomega) {
			mcr, err := pr7GetMCR(ctx, nsName, rootSnap)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pr7MCRHasPVCTarget(mcr, pvcA)).To(BeFalse(), "Variant A: residual pvc-a becomes its own child volume node, not in the root MCR")
			g.Expect(pr7MCRHasPVCTarget(mcr, pvcB)).To(BeFalse(), "Variant A: residual pvc-b becomes its own child volume node, not in the root MCR")
			for _, t := range mcr.Spec.Targets {
				g.Expect(t.Kind).NotTo(Equal("PersistentVolumeClaim"), "root MCR must not carry any PVC manifest under Variant A")
			}
		}, 150*time.Second, 500*time.Millisecond).Should(Succeed())

		// Each residual PVC is observable as its own child volume node (dedicated SnapshotContent carrying a
		// single dataRef for that PVC).
		Eventually(func(g Gomega) {
			for _, pvc := range []*corev1.PersistentVolumeClaim{pvcA, pvcB} {
				found, err := pr7ChildVolumeNodeForPVC(ctx, pvc)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue(), "residual PVC %s must have its own child volume node", pvc.Name)
			}
		}, 150*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	// Pending: the pending-VCR coverage mechanism (pvcUIDsFromPendingVCR) is deterministically covered by
	// the unit test TestCollectSubtreeCoveredPVCUIDs_pendingVCRTargets. Reproducing it at the integration
	// level under wave7 is inherently racy: the only way to obtain a subtree child whose bound content has a
	// pending (dataRef-less) VCR is a synthetic empty-spec namespace child, and once the controller observes
	// that owned VCR the child content's own volume-leg readiness races the fixture (and collides on the
	// ObjectKeeper lifecycle ownerRef), so the root MCR sometimes never advances. A faithful, stable version
	// needs a registered domain snapshot kind that legitimately carries an in-flight VCR — out of scope here.
	PIt("pending VCR spec.targets count as subtree coverage before dataRefs publish", func() {
		pr7RequireVolumeCaptureRequestAPI()
		ctx := context.Background()
		ns := pr7CreateNamespace(ctx, "n5-pr7-pending-vcr")
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		// Synthetic covered PVC identity (NOT a live namespace PVC): under wave7 a namespace-root capture
		// owns residual PVC discovery itself, so a live PVC would be orphan-captured by the root AND covered
		// by the child's VCR — a self-inflicted DuplicateCoveredPVCUID. The pending-VCR coverage mechanism
		// (pvcUIDsFromPendingVCR) keys purely on the VCR target UID, so a synthetic identity exercises it
		// faithfully without that conflict. The child has NO dataRefs yet (only the in-flight VCR), which is
		// the "before dataRefs publish" condition this spec exercises.
		coveredPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-pending", Namespace: nsName, UID: types.UID("pr7-pending-covered-pvc-uid")},
		}

		childName := "pr7-pending-child"
		Expect(k8sClient.Create(ctx, &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: childName, Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		})).To(Succeed())
		childKey := types.NamespacedName{Namespace: nsName, Name: childName}
		childSnap := pr7WaitSnapshotBound(ctx, childKey)
		childContent := &storagev1alpha1.SnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: childSnap.Status.BoundSnapshotContentName}, childContent)).To(Succeed())

		// Ready MCP with no PVC objects and no dataRefs, then a pending (dataRef-less) VCR covering the PVC.
		pr7InstallReadyChildSubtreeFixture(ctx, childContent.Name, nsName, nil, nil)
		pr7InstallPendingVCR(ctx, nsName, childContent, coveredPVC)
		pr7SeedSubtreeManifestsPersisted(ctx, childContent.Name)
		freshChildContent := &storagev1alpha1.SnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: childContent.Name}, freshChildContent)).To(Succeed())
		Expect(freshChildContent.Status.Data).To(BeNil())

		rootName := "pr7-pending-root"
		Expect(k8sClient.Create(ctx, &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: rootName, Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		})).To(Succeed())
		rootKey := types.NamespacedName{Namespace: nsName, Name: rootName}
		_ = pr7WaitSnapshotBound(ctx, rootKey)
		Expect(mergeChildGraphIntoRoot(ctx, k8sClient, nsName, rootName, childName, childContent.Name)).To(Succeed())
		rootSnap := pr7WaitSnapshotBound(ctx, rootKey)
		pr7KickSnapshot(ctx, rootKey)

		Eventually(func(g Gomega) {
			// The pending VCR contributes coveredPVC to the subtree-covered set (pvcUIDsFromPendingVCR), so
			// the root coverage walk consumes it cleanly: the root MCR is created (not stalled on
			// ErrSubtreeManifestCapturePending) and the covered PVC is never planned into it.
			mcr, err := pr7GetMCR(ctx, nsName, rootSnap)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pr7MCRHasPVCTarget(mcr, coveredPVC)).To(BeFalse(), "pending-VCR-covered PVC must not appear in the root MCR")
			// And the pending-VCR coverage must not be mis-detected as a duplicate against itself.
			root := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, rootKey, root)).To(Succeed())
			rc := meta.FindStatusCondition(root.Status.Conditions, snapshot.ConditionReady)
			if rc != nil {
				g.Expect(rc.Reason).NotTo(Equal("DuplicateCoveredPVCUID"))
			}
		}, 120*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	It("duplicate pvcUID in subtree fails closed with DuplicateCoveredPVCUID", func() {
		ctx := context.Background()
		ns := pr7CreateNamespace(ctx, "n5-pr7-duplicate")
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		// Duplicate detection keys purely on the descendant SnapshotContents' dataRef target UID. Use a
		// synthetic PVC identity that is NOT a live namespace PVC: a real PVC would be discovered as a
		// residual/orphan PVC and its volume capture would fail closed first (no storageClassName, and no CSI
		// VolumeSnapshotClass chain exists in envtest), so the child would surface VolumeCaptureFailed and the
		// root would mirror ChildrenFailed before ever reaching the duplicate guard. With no live PVC nothing
		// residual-captures, so both child contents simply claim the same UID and the root fails closed with
		// DuplicateCoveredPVCUID (the invariant this spec exists to prove).
		dupPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-dup", Namespace: nsName, UID: types.UID("pr7-dup-covered-pvc-uid")},
		}
		// The colliding dataRef points at a real ready VolumeSnapshotContent: under wave7 a dataRef whose
		// artifact VSC is absent makes each child Ready=False/ArtifactMissing, so the root would mirror
		// ChildrenFailed before reaching the duplicate guard. A ready VSC lets both children go Ready=True so
		// the root actually walks subtree coverage and hits the DuplicateCoveredPVCUID guard.
		pr7CreateReadyVSC(ctx, "vsc-dup-a")
		dupBinding := pr7PVCDataBinding(dupPVC, "vsc-dup-a")

		child1Name, child2Name := "pr7-dup-child-1", "pr7-dup-child-2"
		for _, name := range []string{child1Name, child2Name} {
			Expect(k8sClient.Create(ctx, &storagev1alpha1.Snapshot{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName},
				Spec:       storagev1alpha1.SnapshotSpec{},
			})).To(Succeed())
		}
		child1Snap := pr7WaitSnapshotBound(ctx, types.NamespacedName{Namespace: nsName, Name: child1Name})
		child2Snap := pr7WaitSnapshotBound(ctx, types.NamespacedName{Namespace: nsName, Name: child2Name})
		// pvc=nil: do not install a live PVC (and no PVC manifest in the child MCP), only publish the
		// colliding dataRef on each child content so the duplicate guard is exercised in isolation.
		pr7InstallReadyChildSubtreeFixture(ctx, child1Snap.Status.BoundSnapshotContentName, nsName, nil, []storagev1alpha1.SnapshotDataBinding{dupBinding})
		pr7InstallReadyChildSubtreeFixture(ctx, child2Snap.Status.BoundSnapshotContentName, nsName, nil, []storagev1alpha1.SnapshotDataBinding{dupBinding})
		// Latch the manifest-capture wave barrier on both direct children so the root advances past
		// ManifestCapturePending to the PVC exclude computation, where the duplicate covered-PVC-UID guard runs.
		pr7SeedSubtreeManifestsPersisted(ctx, child1Snap.Status.BoundSnapshotContentName)
		pr7SeedSubtreeManifestsPersisted(ctx, child2Snap.Status.BoundSnapshotContentName)

		rootName := "pr7-dup-root"
		Expect(k8sClient.Create(ctx, &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: rootName, Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		})).To(Succeed())
		rootKey := types.NamespacedName{Namespace: nsName, Name: rootName}
		_ = pr7WaitSnapshotBound(ctx, rootKey)
		Expect(mergeChildGraphIntoRoot(ctx, k8sClient, nsName, rootName, child1Name, child1Snap.Status.BoundSnapshotContentName)).To(Succeed())
		Expect(mergeChildGraphIntoRoot(ctx, k8sClient, nsName, rootName, child2Name, child2Snap.Status.BoundSnapshotContentName)).To(Succeed())
		pr7KickSnapshot(ctx, rootKey)

		Eventually(func(g Gomega) {
			root := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, rootKey, root)).To(Succeed())
			rc := meta.FindStatusCondition(root.Status.Conditions, snapshot.ConditionReady)
			g.Expect(rc).NotTo(BeNil())
			g.Expect(rc.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(rc.Reason).To(Equal("DuplicateCoveredPVCUID"))
			g.Expect(rc.Message).To(ContainSubstring(string(dupPVC.UID)))
			mcr, err := pr7GetMCR(ctx, nsName, root)
			if err == nil {
				g.Expect(pr7MCRHasPVCTarget(mcr, dupPVC)).To(BeFalse(), "invalid root MCR must not plan the duplicated PVC after duplicate failure")
			} else {
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
		}, 120*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	It("root MCR captures non-PVC namespace objects while residual CSI PVCs are excluded (own child volume nodes)", func() {
		ctx := context.Background()
		reactorCtx, cancel := context.WithCancel(ctx)
		DeferCleanup(cancel)

		ns := pr7CreateNamespace(ctx, "n5-pr7-manifest-only")
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})
		pr7StartFakeExternalSnapshotter(reactorCtx, nsName)

		// A plain namespaced object (ConfigMap) belongs in the root aggregator MCR; a residual CSI PVC does
		// not (it becomes its own child volume node). This proves the residual-PVC exclude does not suppress
		// ordinary manifest capture: the root MCR is created, carries the ConfigMap, and omits the PVC.
		cmName := "pr7-cm"
		Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: nsName},
			Data:       map[string]string{"k": "v"},
		})).To(Succeed())
		pvcA := pr7CreateCSIPVC(ctx, nsName, "pvc-a")

		rootName := "pr7-manifest-root"
		Expect(k8sClient.Create(ctx, &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: rootName, Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		})).To(Succeed())
		rootKey := types.NamespacedName{Namespace: nsName, Name: rootName}
		rootSnap := pr7WaitSnapshotBound(ctx, rootKey)

		Eventually(func(g Gomega) {
			mcr, err := pr7GetMCR(ctx, nsName, rootSnap)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pr7MCRHasPVCTarget(mcr, pvcA)).To(BeFalse(), "Variant A: residual pvc-a becomes its own child volume node, not in the root MCR")
			hasCM := false
			for _, t := range mcr.Spec.Targets {
				if t.Kind == "ConfigMap" && t.Name == cmName {
					hasCM = true
				}
			}
			g.Expect(hasCM).To(BeTrue(), "root MCR must capture the plain ConfigMap manifest")
		}, 120*time.Second, 500*time.Millisecond).Should(Succeed())
	})
})
