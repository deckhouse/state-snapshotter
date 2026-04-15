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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func childNamespaceSnapshotContentOwnerRefToParent(refs []metav1.OwnerReference, parentName string, parentUID types.UID) bool {
	for i := range refs {
		ref := refs[i]
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "NamespaceSnapshotContent" &&
			ref.Name == parentName && ref.UID == parentUID {
			return true
		}
	}
	return false
}

var _ = Describe("Integration: NamespaceSnapshot content tree (synthetic child scaffold)", func() {
	It("creates synthetic child, writes graph refs, parent Ready only after child Ready", func() {
		ctx := context.Background()
		var childNSCNameForCleanup string

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-n2b-synth-tree-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "namespacesnapshot-synthetic-tree",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			if childNSCNameForCleanup != "" {
				_ = k8sClient.Delete(ctx, &storagev1alpha1.NamespaceSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: childNSCNameForCleanup}})
			}
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-synth-tree-cm", Namespace: nsName},
			Data:       map[string]string{"k": "v"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		parentName := "parent-snap"
		childName := namespacemanifest.NamespaceSnapshotSyntheticChildName(parentName)

		parent := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      parentName,
				Namespace: nsName,
				Annotations: map[string]string{
					namespacemanifest.AnnotationSyntheticChildTree: "true",
				},
			},
			Spec: storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, parent)).To(Succeed())

		parentKey := types.NamespacedName{Namespace: nsName, Name: parentName}
		childKey := types.NamespacedName{Namespace: nsName, Name: childName}

		Eventually(func(g Gomega) {
			p := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			g.Expect(p.UID).NotTo(BeEmpty())
			ch := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, childKey, ch)).To(Succeed())
			g.Expect(ch.Labels[namespacemanifest.LabelSyntheticChild]).To(Equal("true"))
			g.Expect(ch.Labels[namespacemanifest.LabelSyntheticParentName]).To(Equal(parentName))
			g.Expect(ch.Labels[namespacemanifest.LabelSyntheticParentUID]).To(Equal(string(p.UID)))
			ann := ch.Annotations
			if ann == nil {
				ann = map[string]string{}
			}
			_, hasSynthTreeAnn := ann[namespacemanifest.AnnotationSyntheticChildTree]
			g.Expect(hasSynthTreeAnn).To(BeFalse(), "synthetic child must not opt into synthetic tree (stays N2a leaf)")
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			p := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			g.Expect(p.Status.ChildrenSnapshotRefs).To(HaveLen(1))
			g.Expect(p.Status.ChildrenSnapshotRefs[0].Name).To(Equal(childName))
			g.Expect(p.Status.ChildrenSnapshotRefs[0].Namespace).To(Equal(nsName))
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			p := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			bound := meta.FindStatusCondition(p.Status.Conditions, snapshot.ConditionBound)
			g.Expect(bound).NotTo(BeNil())
			g.Expect(bound.Status).To(Equal(metav1.ConditionTrue))
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			c := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, childKey, c)).To(Succeed())
			g.Expect(c.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			cReady := meta.FindStatusCondition(c.Status.Conditions, snapshot.ConditionReady)
			g.Expect(cReady == nil || cReady.Status != metav1.ConditionTrue).To(BeTrue())
			p := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			pReady := meta.FindStatusCondition(p.Status.Conditions, snapshot.ConditionReady)
			g.Expect(pReady).NotTo(BeNil())
			g.Expect(pReady.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(pReady.Reason).To(Equal(snapshot.ReasonChildSnapshotPending))
		}).WithTimeout(120 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			c := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, childKey, c)).To(Succeed())
			cr := meta.FindStatusCondition(c.Status.Conditions, snapshot.ConditionReady)
			g.Expect(cr).NotTo(BeNil())
			g.Expect(cr.Status).To(Equal(metav1.ConditionTrue))
		}, 180*time.Second, 300*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			p := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			pr := meta.FindStatusCondition(p.Status.Conditions, snapshot.ConditionReady)
			g.Expect(pr).NotTo(BeNil())
			g.Expect(pr.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(pr.Reason).To(Equal(snapshot.ReasonCompleted))
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		var parentSnap storagev1alpha1.NamespaceSnapshot
		Expect(k8sClient.Get(ctx, parentKey, &parentSnap)).To(Succeed())
		Expect(parentSnap.Status.BoundSnapshotContentName).NotTo(BeEmpty())

		childSnap := &storagev1alpha1.NamespaceSnapshot{}
		Expect(k8sClient.Get(ctx, childKey, childSnap)).To(Succeed())
		Expect(childSnap.Status.BoundSnapshotContentName).NotTo(BeEmpty())

		parentContent := &storagev1alpha1.NamespaceSnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: parentSnap.Status.BoundSnapshotContentName}, parentContent)).To(Succeed())
		Expect(parentContent.Status.ChildrenSnapshotContentRefs).To(HaveLen(1))
		Expect(parentContent.Status.ChildrenSnapshotContentRefs[0].Name).To(Equal(childSnap.Status.BoundSnapshotContentName))

		childContent := &storagev1alpha1.NamespaceSnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: childSnap.Status.BoundSnapshotContentName}, childContent)).To(Succeed())
		Expect(childNamespaceSnapshotContentOwnerRefToParent(childContent.OwnerReferences, parentContent.Name, parentContent.UID)).To(BeTrue(),
			"child NamespaceSnapshotContent must reference parent NamespaceSnapshotContent for GC cascade")
		Expect(snapshot.HasFinalizer(childContent, snapshot.FinalizerParentProtect)).To(BeTrue())
		childNSCNameForCleanup = childContent.Name

		Expect(k8sClient.Delete(ctx, &storagev1alpha1.NamespaceSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: parentContent.Name},
		})).To(Succeed())

		Eventually(func(g Gomega) {
			ch := &storagev1alpha1.NamespaceSnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: childContent.Name}, ch)).To(Succeed())
			g.Expect(snapshot.HasFinalizer(ch, snapshot.FinalizerParentProtect)).To(BeFalse(),
				"NamespaceSnapshotContentController must strip parent-protect from children when parent content is deleting")
		}).WithTimeout(90 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		pTree := &storagev1alpha1.NamespaceSnapshot{}
		Expect(k8sClient.Get(ctx, parentKey, pTree)).To(Succeed())
		chTree := &storagev1alpha1.NamespaceSnapshot{}
		Expect(k8sClient.Get(ctx, childKey, chTree)).To(Succeed())
		Expect(chTree.Status.ChildrenSnapshotRefs).To(BeEmpty())
		Expect(pTree.Status.ChildrenSnapshotRefs).To(HaveLen(1))
	})
})
