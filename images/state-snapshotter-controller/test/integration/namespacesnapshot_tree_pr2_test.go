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
	"strings"
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

var _ = Describe("Integration: NamespaceSnapshot N2b PR2 synthetic one-child tree", func() {
	It("creates synthetic child, writes graph refs, parent Ready only after child Ready", func() {
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-pr2-tree-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "namespacesnapshot-tree-pr2",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-pr2-cm", Namespace: nsName},
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
			g.Expect(ch.Labels[namespacemanifest.LabelN2bSyntheticChild]).To(Equal("true"))
			g.Expect(ch.Labels[namespacemanifest.LabelN2bParentName]).To(Equal(parentName))
			g.Expect(ch.Labels[namespacemanifest.LabelN2bParentUID]).NotTo(BeEmpty())
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

		Expect(childSnap.Status.ChildrenSnapshotRefs).To(BeEmpty())

		Expect(parentSnap.Status.ChildrenSnapshotRefs).To(HaveLen(1))
		wantContent := fmt.Sprintf("ns-%s", strings.ReplaceAll(string(parentSnap.UID), "-", ""))
		Expect(parentSnap.Status.BoundSnapshotContentName).To(Equal(wantContent))
	})
})
