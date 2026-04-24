//go:build integration
// +build integration

/*
Copyright 2025 Flant JSC

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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// mergeChildGraphIntoRoot wires status.childrenSnapshotRefs on the root NamespaceSnapshot and the matching
// childrenSnapshotContentRefs entry on the root NamespaceSnapshotContent (integration seed only).
func mergeChildGraphIntoRoot(ctx context.Context, c client.Client, rootNS, rootName, childNSSName, childNSCName string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		p := &storagev1alpha1.NamespaceSnapshot{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: rootNS, Name: rootName}, p); err != nil {
			return err
		}
		want := storagev1alpha1.NamespaceSnapshotChildRef{
			APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
			Kind:       "NamespaceSnapshot",
			Namespace:  rootNS,
			Name:       childNSSName,
		}
		found := false
		for _, r := range p.Status.ChildrenSnapshotRefs {
			ns := r.Namespace
			if ns == "" {
				ns = rootNS
			}
			if r.APIVersion == want.APIVersion && r.Kind == want.Kind && ns == want.Namespace && r.Name == want.Name {
				found = true
				break
			}
		}
		if !found {
			p.Status.ChildrenSnapshotRefs = append(append([]storagev1alpha1.NamespaceSnapshotChildRef(nil), p.Status.ChildrenSnapshotRefs...), want)
			if err := c.Status().Update(ctx, p); err != nil {
				return err
			}
		}
		if err := c.Get(ctx, types.NamespacedName{Namespace: rootNS, Name: rootName}, p); err != nil {
			return err
		}
		rootNSC := p.Status.BoundSnapshotContentName
		if rootNSC == "" {
			return fmt.Errorf("root has no bound NamespaceSnapshotContent yet")
		}
		pc := &storagev1alpha1.NamespaceSnapshotContent{}
		if err := c.Get(ctx, client.ObjectKey{Name: rootNSC}, pc); err != nil {
			return err
		}
		foundC := false
		for _, r := range pc.Status.ChildrenSnapshotContentRefs {
			if r.Name == childNSCName {
				foundC = true
				break
			}
		}
		if !foundC {
			pc.Status.ChildrenSnapshotContentRefs = append(append([]storagev1alpha1.NamespaceSnapshotContentChildRef(nil), pc.Status.ChildrenSnapshotContentRefs...),
				storagev1alpha1.NamespaceSnapshotContentChildRef{Name: childNSCName})
			if err := c.Status().Update(ctx, pc); err != nil {
				return err
			}
		}
		return nil
	})
}

var _ = Describe("Integration: E5 subtree root MCR gate (real child NamespaceSnapshot graph)", func() {
	It("does not create root MCR until child NamespaceSnapshotContent subtree MCP is Ready", func() {
		ctx := context.Background()
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-e5-graph-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "namespacesnapshot-e5-graph",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "e5-gate-cm", Namespace: nsName},
			Data:       map[string]string{"k": "v"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		childName := "e5-subtree-child"
		child := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: childName, Namespace: nsName},
			Spec:       storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, child)).To(Succeed())

		childKey := types.NamespacedName{Namespace: nsName, Name: childName}
		var childNSC string
		Eventually(func(g Gomega) {
			ch := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, childKey, ch)).To(Succeed())
			b := meta.FindStatusCondition(ch.Status.Conditions, snapshot.ConditionBound)
			g.Expect(b).NotTo(BeNil())
			g.Expect(b.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ch.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			childNSC = ch.Status.BoundSnapshotContentName
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			ch := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, childKey, ch)).To(Succeed())
			rc := meta.FindStatusCondition(ch.Status.Conditions, snapshot.ConditionReady)
			g.Expect(rc).NotTo(BeNil())
			g.Expect(rc.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(rc.Reason).To(Equal(snapshot.ReasonCompleted))
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		parentName := "e5-subtree-root"
		parent := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: parentName, Namespace: nsName},
			Spec:       storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, parent)).To(Succeed())
		parentKey := types.NamespacedName{Namespace: nsName, Name: parentName}

		Eventually(func(g Gomega) {
			p := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			b := meta.FindStatusCondition(p.Status.Conditions, snapshot.ConditionBound)
			g.Expect(b).NotTo(BeNil())
			g.Expect(b.Status).To(Equal(metav1.ConditionTrue))
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		Expect(mergeChildGraphIntoRoot(ctx, k8sClient, nsName, parentName, childName, childNSC)).To(Succeed())

		Eventually(func(g Gomega) {
			p := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			g.Expect(p.Status.BoundSnapshotContentName).NotTo(BeEmpty())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		// E5: while the child subtree MCP is not Ready, root must not run a completed first MCR plan (gate or absent MCR).
		Eventually(func(g Gomega) {
			p := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			pc := &storagev1alpha1.NamespaceSnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: p.Status.BoundSnapshotContentName}, pc)).To(Succeed())
			g.Expect(pc.Status.ChildrenSnapshotContentRefs).NotTo(BeEmpty())
			chName := pc.Status.ChildrenSnapshotContentRefs[0].Name
			chNSC := &storagev1alpha1.NamespaceSnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: chName}, chNSC)).To(Succeed())

			mcrName := namespacemanifest.NamespaceSnapshotMCRName(p.UID)
			if chNSC.Status.ManifestCheckpointName == "" {
				err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: mcrName}, &ssv1alpha1.ManifestCaptureRequest{})
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "root MCR must not exist before child subtree has manifestCheckpointName")
				g.Expect(false).To(BeTrue(), "waiting for child NamespaceSnapshotContent.manifestCheckpointName")
			}
			mcp := &ssv1alpha1.ManifestCheckpoint{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: chNSC.Status.ManifestCheckpointName}, mcp)).To(Succeed())
			rc := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
			if rc == nil || rc.Status != metav1.ConditionTrue {
				err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: mcrName}, &ssv1alpha1.ManifestCaptureRequest{})
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "root MCR must not exist until child subtree MCP is Ready")
				g.Expect(false).To(BeTrue(), "waiting for child subtree ManifestCheckpoint Ready")
			}
			g.Expect(true).To(BeTrue())
		}, 240*time.Second, 150*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			p := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			pr := meta.FindStatusCondition(p.Status.Conditions, snapshot.ConditionReady)
			g.Expect(pr).NotTo(BeNil())
			g.Expect(pr.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(pr.Reason).To(Equal(snapshot.ReasonCompleted))
		}, 180*time.Second, 200*time.Millisecond).Should(Succeed())

		// If root MCR still exists briefly, it must not list the ConfigMap already captured under the child subtree.
		pFinal := &storagev1alpha1.NamespaceSnapshot{}
		Expect(k8sClient.Get(ctx, parentKey, pFinal)).To(Succeed())
		mcrName := namespacemanifest.NamespaceSnapshotMCRName(pFinal.UID)
		mcr := &ssv1alpha1.ManifestCaptureRequest{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: mcrName}, mcr); err == nil {
			for _, tgt := range mcr.Spec.Targets {
				isGateCM := tgt.APIVersion == "v1" && tgt.Kind == "ConfigMap" && tgt.Name == "e5-gate-cm"
				Expect(isGateCM).To(BeFalse(), "if root MCR still exists, it must exclude ConfigMap captured under child subtree MCP")
			}
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
	})
})

var _ = Describe("Integration: E6 parent Ready when child NamespaceSnapshot fails capture", func() {
	It("sets parent Ready=False ChildSnapshotFailed when child hits terminal capture failure", func() {
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-e6-child-fail-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "namespacesnapshot-e6-child-fail",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-e6-fail-cm", Namespace: nsName},
			Data:       map[string]string{"k": "v"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		parentName := "parent-e6-fail"
		childName := "child-e6-fail"

		parent := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: parentName, Namespace: nsName},
			Spec:       storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, parent)).To(Succeed())

		child := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: childName, Namespace: nsName},
			Spec:       storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, child)).To(Succeed())

		parentKey := types.NamespacedName{Namespace: nsName, Name: parentName}
		childKey := types.NamespacedName{Namespace: nsName, Name: childName}

		Eventually(func(g Gomega) {
			ch := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, childKey, ch)).To(Succeed())
			g.Expect(ch.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			g.Expect(ch.UID).NotTo(BeEmpty())
		}, 180*time.Second, 200*time.Millisecond).Should(Succeed())

		childSnap := &storagev1alpha1.NamespaceSnapshot{}
		Expect(k8sClient.Get(ctx, childKey, childSnap)).To(Succeed())
		mcrKey := types.NamespacedName{
			Namespace: nsName,
			Name:      namespacemanifest.NamespaceSnapshotMCRName(childSnap.UID),
		}

		Eventually(func(g Gomega) {
			mcr := &ssv1alpha1.ManifestCaptureRequest{}
			g.Expect(k8sClient.Get(ctx, mcrKey, mcr)).To(Succeed())
			g.Expect(mcr.Spec.Targets).NotTo(BeEmpty())
		}, 180*time.Second, 200*time.Millisecond).Should(Succeed())

		Expect(mergeChildGraphIntoRoot(ctx, k8sClient, nsName, parentName, childName, childSnap.Status.BoundSnapshotContentName)).To(Succeed())

		mcr := &ssv1alpha1.ManifestCaptureRequest{}
		Expect(k8sClient.Get(ctx, mcrKey, mcr)).To(Succeed())
		mcrPatchBase := mcr.DeepCopy()
		mcr.Spec.Targets = append(append([]ssv1alpha1.ManifestTarget(nil), mcr.Spec.Targets...), ssv1alpha1.ManifestTarget{
			APIVersion: "v1",
			Kind:       "ConfigMap",
			Name:       "nss-e6-fail-drift-fake-not-in-cluster",
		})
		Expect(k8sClient.Patch(ctx, mcr, client.MergeFrom(mcrPatchBase))).To(Succeed())

		childFresh := &storagev1alpha1.NamespaceSnapshot{}
		Expect(k8sClient.Get(ctx, childKey, childFresh)).To(Succeed())
		childBase := childFresh.DeepCopy()
		if childFresh.Annotations == nil {
			childFresh.Annotations = map[string]string{}
		}
		childFresh.Annotations["state-snapshotter.deckhouse.io/integration-child-drift-kick"] = fmt.Sprintf("%d", time.Now().UnixNano())
		Expect(k8sClient.Patch(ctx, childFresh, client.MergeFrom(childBase))).To(Succeed())

		Eventually(func(g Gomega) {
			ch := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, childKey, ch)).To(Succeed())
			cr := meta.FindStatusCondition(ch.Status.Conditions, snapshot.ConditionReady)
			g.Expect(cr).NotTo(BeNil())
			g.Expect(cr.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cr.Reason).To(Equal("CapturePlanDrift"))
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			pKick := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, pKick)).To(Succeed())
			pKickBase := pKick.DeepCopy()
			if pKick.Annotations == nil {
				pKick.Annotations = map[string]string{}
			}
			pKick.Annotations["state-snapshotter.deckhouse.io/integration-parent-kick"] = fmt.Sprintf("%d", time.Now().UnixNano())
			g.Expect(k8sClient.Patch(ctx, pKick, client.MergeFrom(pKickBase))).To(Succeed())

			p := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			pr := meta.FindStatusCondition(p.Status.Conditions, snapshot.ConditionReady)
			g.Expect(pr).NotTo(BeNil())
			g.Expect(pr.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(pr.Reason).To(Equal(snapshot.ReasonChildSnapshotFailed))
			g.Expect(pr.Message).To(ContainSubstring(childKey.String()))
			g.Expect(pr.Message).To(ContainSubstring("CapturePlanDrift"))
		}, 180*time.Second, 300*time.Millisecond).Should(Succeed())
	})
})
