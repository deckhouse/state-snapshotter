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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

var _ = Describe("Integration: NamespaceSnapshot N2b PR3 parent aggregate on child failure", func() {
	It("sets parent Ready=False ChildSnapshotFailed when synthetic child hits terminal capture failure", func() {
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-pr3-tree-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "namespacesnapshot-tree-pr3",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-pr3-cm", Namespace: nsName},
			Data:       map[string]string{"k": "v"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		parentName := "parent-pr3"
		childName := namespacemanifest.NamespaceSnapshotSyntheticChildName(parentName)

		parent := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      parentName,
				Namespace: nsName,
				Annotations: map[string]string{
					namespacemanifest.AnnotationN2bPR2SyntheticTree: "true",
				},
			},
			Spec: storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, parent)).To(Succeed())

		parentKey := types.NamespacedName{Namespace: nsName, Name: parentName}
		childKey := types.NamespacedName{Namespace: nsName, Name: childName}

		Eventually(func(g Gomega) {
			ch := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, childKey, ch)).To(Succeed())
			g.Expect(ch.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			g.Expect(ch.UID).NotTo(BeEmpty())
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

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
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		mcr := &ssv1alpha1.ManifestCaptureRequest{}
		Expect(k8sClient.Get(ctx, mcrKey, mcr)).To(Succeed())
		mcrPatchBase := mcr.DeepCopy()
		// Extra target that does not exist in the namespace → live plan ≠ MCR spec → CapturePlanDrift on child only (parent has its own MCR).
		mcr.Spec.Targets = append(append([]ssv1alpha1.ManifestTarget(nil), mcr.Spec.Targets...), ssv1alpha1.ManifestTarget{
			APIVersion: "v1",
			Kind:       "ConfigMap",
			Name:       "nss-pr3-drift-fake-not-in-cluster",
		})
		Expect(k8sClient.Patch(ctx, mcr, client.MergeFrom(mcrPatchBase))).To(Succeed())

		childFresh := &storagev1alpha1.NamespaceSnapshot{}
		Expect(k8sClient.Get(ctx, childKey, childFresh)).To(Succeed())
		childBase := childFresh.DeepCopy()
		if childFresh.Annotations == nil {
			childFresh.Annotations = map[string]string{}
		}
		childFresh.Annotations["state-snapshotter.deckhouse.io/integration-pr3-drift-kick"] = fmt.Sprintf("%d", time.Now().UnixNano())
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
			// Re-enqueue parent each tick: a reconcile may run once while child status is still non-terminal,
			// leaving parent on ChildSnapshotPending until the next reconcile after child fails closed.
			pKick := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, pKick)).To(Succeed())
			pKickBase := pKick.DeepCopy()
			if pKick.Annotations == nil {
				pKick.Annotations = map[string]string{}
			}
			pKick.Annotations["state-snapshotter.deckhouse.io/integration-pr3-parent-kick"] = fmt.Sprintf("%d", time.Now().UnixNano())
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
