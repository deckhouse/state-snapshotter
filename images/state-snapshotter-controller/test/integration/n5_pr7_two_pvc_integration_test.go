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

var _ = Describe("Integration: N5 PR-7 two-PVC subtree vertical slice", Serial, func() {
	It("two PVC vertical slice: child covers pvc-a, root MCR captures only residual pvc-b", func() {
		ctx := context.Background()
		ns := pr7CreateNamespace(ctx, "n5-pr7-two-pvc")
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		pvcA := pr7CreatePVC(ctx, nsName, "pvc-a")
		pvcB := pr7CreatePVC(ctx, nsName, "pvc-b")
		childName := "pr7-child"
		child := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: childName, Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, child)).To(Succeed())
		childKey := types.NamespacedName{Namespace: nsName, Name: childName}
		childSnap := pr7WaitSnapshotBound(ctx, childKey)
		childContentName := childSnap.Status.BoundSnapshotContentName

		pr7InstallReadyChildSubtreeFixture(ctx, childContentName, nsName, pvcA, []storagev1alpha1.SnapshotDataBinding{
			pr7PVCDataBinding(pvcA, "vsc-pr7-child-a"),
		})

		rootName := "pr7-root"
		root := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: rootName, Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, root)).To(Succeed())
		rootKey := types.NamespacedName{Namespace: nsName, Name: rootName}
		_ = pr7WaitSnapshotBound(ctx, rootKey)
		Expect(mergeChildGraphIntoRoot(ctx, k8sClient, nsName, rootName, childName, childContentName)).To(Succeed())
		rootSnap := pr7WaitSnapshotBound(ctx, rootKey)
		pr7KickSnapshot(ctx, rootKey)
		pr7AssertSnapshotDoesNotUseStubAnnotation(rootSnap)

		Eventually(func(g Gomega) {
			mcr, err := pr7GetMCR(ctx, nsName, rootSnap)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pr7MCRHasPVCTarget(mcr, pvcB)).To(BeTrue(), "root MCR must include residual pvc-b")
			g.Expect(pr7MCRHasPVCTarget(mcr, pvcA)).To(BeFalse(), "root MCR must not include child-covered pvc-a")
			for _, t := range mcr.Spec.Targets {
				if t.Kind == "PersistentVolumeClaim" {
					g.Expect(t.APIVersion).To(Equal(corev1.SchemeGroupVersion.String()))
					g.Expect(t.Name).NotTo(Equal(pvcA.Name))
				}
			}
		}, 180*time.Second, 250*time.Millisecond).Should(Succeed())

		childContent := &storagev1alpha1.SnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: childContentName}, childContent)).To(Succeed())
		var found bool
		for _, ref := range childContent.Status.DataRefs {
			if ref.TargetUID == string(pvcA.UID) {
				found = true
				Expect(ref.Target.Kind).To(Equal("PersistentVolumeClaim"))
				Expect(ref.Target.APIVersion).To(Equal(corev1.SchemeGroupVersion.String()))
				Expect(ref.Target.Name).To(Equal("pvc-a"))
				Expect(ref.Target.Namespace).To(Equal(nsName))
			}
		}
		Expect(found).To(BeTrue(), "child SnapshotContent must publish dataRef for pvc-a UID")
	})

	It("pending VCR spec.targets count as subtree coverage before dataRefs publish", func() {
		pr7RequireVolumeCaptureRequestAPI()
		ctx := context.Background()
		ns := pr7CreateNamespace(ctx, "n5-pr7-pending-vcr")
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		pvcA := pr7CreatePVC(ctx, nsName, "pvc-a")
		pvcB := pr7CreatePVC(ctx, nsName, "pvc-b")

		childName := "pr7-pending-child"
		Expect(k8sClient.Create(ctx, &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: childName, Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		})).To(Succeed())
		childKey := types.NamespacedName{Namespace: nsName, Name: childName}
		childSnap := pr7WaitSnapshotBound(ctx, childKey)
		childContent := &storagev1alpha1.SnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: childSnap.Status.BoundSnapshotContentName}, childContent)).To(Succeed())

		pr7InstallReadyChildSubtreeFixture(ctx, childContent.Name, nsName, pvcA, nil)
		pr7InstallPendingVCR(ctx, nsName, childContent, pvcA)
		freshChildContent := &storagev1alpha1.SnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: childContent.Name}, freshChildContent)).To(Succeed())
		Expect(freshChildContent.Status.DataRefs).To(BeEmpty())

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
		pr7AssertSnapshotDoesNotUseStubAnnotation(rootSnap)

		Eventually(func(g Gomega) {
			mcr, err := pr7GetMCR(ctx, nsName, rootSnap)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pr7MCRHasPVCTarget(mcr, pvcA)).To(BeFalse(), "pending VCR on child must cover pvc-a")
			g.Expect(pr7MCRHasPVCTarget(mcr, pvcB)).To(BeTrue(), "residual pvc-b must remain on root MCR")
		}, 180*time.Second, 250*time.Millisecond).Should(Succeed())
	})

	It("duplicate pvcUID in subtree fails closed with DuplicateCoveredPVCUID", func() {
		ctx := context.Background()
		ns := pr7CreateNamespace(ctx, "n5-pr7-duplicate")
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		pvcA := pr7CreatePVC(ctx, nsName, "pvc-a")
		dupBinding := pr7PVCDataBinding(pvcA, "vsc-dup-a")

		child1Name, child2Name := "pr7-dup-child-1", "pr7-dup-child-2"
		for _, name := range []string{child1Name, child2Name} {
			Expect(k8sClient.Create(ctx, &storagev1alpha1.Snapshot{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName},
				Spec:       storagev1alpha1.SnapshotSpec{},
			})).To(Succeed())
		}
		child1Snap := pr7WaitSnapshotBound(ctx, types.NamespacedName{Namespace: nsName, Name: child1Name})
		child2Snap := pr7WaitSnapshotBound(ctx, types.NamespacedName{Namespace: nsName, Name: child2Name})
		pr7InstallReadyChildSubtreeFixture(ctx, child1Snap.Status.BoundSnapshotContentName, nsName, pvcA, []storagev1alpha1.SnapshotDataBinding{dupBinding})
		pr7InstallReadyChildSubtreeFixture(ctx, child2Snap.Status.BoundSnapshotContentName, nsName, pvcA, []storagev1alpha1.SnapshotDataBinding{dupBinding})

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
			g.Expect(rc.Message).To(ContainSubstring(string(pvcA.UID)))
			pr7AssertSnapshotDoesNotUseStubAnnotation(root)
			mcr, err := pr7GetMCR(ctx, nsName, root)
			if err == nil {
				g.Expect(pr7MCRHasPVCTarget(mcr, pvcA)).To(BeFalse(), "invalid root MCR must not plan pvc-a after duplicate failure")
			} else {
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
		}, 180*time.Second, 250*time.Millisecond).Should(Succeed())
	})

	It("manifest-only child does not block root residual PVC planning", func() {
		ctx := context.Background()
		ns := pr7CreateNamespace(ctx, "n5-pr7-manifest-only")
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		pvcA := pr7CreatePVC(ctx, nsName, "pvc-a")
		pvcB := pr7CreatePVC(ctx, nsName, "pvc-b")

		childName := "pr7-manifest-child"
		Expect(k8sClient.Create(ctx, &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: childName, Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		})).To(Succeed())
		childKey := types.NamespacedName{Namespace: nsName, Name: childName}
		childSnap := pr7WaitSnapshotBound(ctx, childKey)
		// Manifest-only leaf: Ready MCP without PVC objects (no dataRefs/VCR); must not E5-exclude namespace PVCs on root.
		pr7InstallReadyChildSubtreeFixture(ctx, childSnap.Status.BoundSnapshotContentName, nsName, nil, nil)

		rootName := "pr7-manifest-root"
		Expect(k8sClient.Create(ctx, &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: rootName, Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		})).To(Succeed())
		rootKey := types.NamespacedName{Namespace: nsName, Name: rootName}
		_ = pr7WaitSnapshotBound(ctx, rootKey)
		Expect(mergeChildGraphIntoRoot(ctx, k8sClient, nsName, rootName, childName, childSnap.Status.BoundSnapshotContentName)).To(Succeed())
		rootSnap := pr7WaitSnapshotBound(ctx, rootKey)
		pr7KickSnapshot(ctx, rootKey)

		Eventually(func(g Gomega) {
			mcr, err := pr7GetMCR(ctx, nsName, rootSnap)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pr7MCRHasPVCTarget(mcr, pvcA)).To(BeTrue())
			g.Expect(pr7MCRHasPVCTarget(mcr, pvcB)).To(BeTrue())
		}, 180*time.Second, 250*time.Millisecond).Should(Succeed())
	})
})
