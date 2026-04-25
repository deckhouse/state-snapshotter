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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// PR5a: one demo kind, merge-safe refs on root NamespaceSnapshot + root NamespaceSnapshotContent, plus one DSC row for registration smoke.
var _ = Describe("Integration: PR5a DemoVirtualDiskSnapshot graph wiring", Serial, func() {
	const dscName = "integration-pr5a-demo-disk-dsc"

	BeforeEach(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.DomainSpecificSnapshotController{ObjectMeta: metav1.ObjectMeta{Name: dscName}}))
	})

	AfterEach(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.DomainSpecificSnapshotController{ObjectMeta: metav1.ObjectMeta{Name: dscName}}))
	})

	It("registers DSC and merges demo disk snapshot into root children*Refs", func() {
		testCtx := context.Background()

		dsc := &ssv1alpha1.DomainSpecificSnapshotController{
			ObjectMeta: metav1.ObjectMeta{Name: dscName},
			Spec: ssv1alpha1.DomainSpecificSnapshotControllerSpec{
				OwnerModule: "integration-pr5a",
				SnapshotResourceMapping: []ssv1alpha1.SnapshotResourceMappingEntry{
					{
						ResourceCRDName: "demovirtualdisks.demo.state-snapshotter.deckhouse.io",
						SnapshotCRDName: "demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io",
						ContentCRDName:  "demovirtualdisksnapshotcontents.demo.state-snapshotter.deckhouse.io",
					},
				},
			},
		}
		Expect(k8sClient.Create(testCtx, dsc)).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(testCtx, &ssv1alpha1.DomainSpecificSnapshotController{ObjectMeta: metav1.ObjectMeta{Name: dscName}}))
		})

		Eventually(func(g Gomega) {
			cur := &ssv1alpha1.DomainSpecificSnapshotController{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: dscName}, cur)).To(Succeed())
			acc := meta.FindStatusCondition(cur.Status.Conditions, controllers.DSCConditionAccepted)
			g.Expect(acc).NotTo(BeNil())
			g.Expect(acc.Status).To(Equal(metav1.ConditionTrue))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		hook := &ssv1alpha1.DomainSpecificSnapshotController{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: dscName}, hook)).To(Succeed())
		gen := hook.GetGeneration()
		meta.SetStatusCondition(&hook.Status.Conditions, metav1.Condition{
			Type:               controllers.DSCConditionRBACReady,
			Status:             metav1.ConditionTrue,
			Reason:             "IntegrationHook",
			Message:            "pr5a demo",
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: gen,
		})
		Expect(k8sClient.Status().Update(testCtx, hook)).To(Succeed())

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "pr5a-demo-disk-",
				Labels:       map[string]string{"state-snapshotter.deckhouse.io/test": "pr5a-demo-disk"},
			},
		}
		Expect(k8sClient.Create(testCtx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}) })

		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "pr5a-cm", Namespace: nsName}, Data: map[string]string{"k": "v"}}
		Expect(k8sClient.Create(testCtx, cm)).To(Succeed())

		diskResource := &demov1alpha1.DemoVirtualDisk{
			ObjectMeta: metav1.ObjectMeta{Name: "disk-a", Namespace: nsName},
		}
		Expect(k8sClient.Create(testCtx, diskResource)).To(Succeed())

		root := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: nsName},
			Spec:       storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(testCtx, root)).To(Succeed())

		var rootNSC string
		Eventually(func(g Gomega) {
			r := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: "root"}, r)).To(Succeed())
			rc := meta.FindStatusCondition(r.Status.Conditions, snapshot.ConditionReady)
			g.Expect(rc).NotTo(BeNil())
			g.Expect(rc.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(r.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			rootNSC = r.Status.BoundSnapshotContentName
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		var diskSnapshotName string
		Eventually(func(g Gomega) {
			r := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: "root"}, r)).To(Succeed())
			diskSnapshotName = ""
			for _, ch := range r.Status.ChildrenSnapshotRefs {
				if ch.APIVersion == demov1alpha1.SchemeGroupVersion.String() && ch.Kind == "DemoVirtualDiskSnapshot" {
					diskSnapshotName = ch.Name
					break
				}
			}
			g.Expect(diskSnapshotName).NotTo(BeEmpty(), "root NamespaceSnapshot should list demo disk snapshot in childrenSnapshotRefs")
		}).WithTimeout(45 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		var contentName string
		Eventually(func(g Gomega) {
			d := &demov1alpha1.DemoVirtualDiskSnapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: diskSnapshotName}, d)).To(Succeed())
			g.Expect(d.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			contentName = d.Status.BoundSnapshotContentName
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			nsc := &storagev1alpha1.NamespaceSnapshotContent{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: rootNSC}, nsc)).To(Succeed())
			var found bool
			for _, ch := range nsc.Status.ChildrenSnapshotContentRefs {
				if ch.Name == contentName {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "root NamespaceSnapshotContent should reference demo disk content")
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		var nscVisited []string
		var demoVisited []string
		hooks := &usecase.DedicatedContentVisitHooks{
			Visit: func(_ context.Context, gvk schema.GroupVersionKind, contentName string, _ *unstructured.Unstructured, _ bool) error {
				if gvk.Kind == "DemoVirtualDiskSnapshotContent" {
					demoVisited = append(demoVisited, contentName)
				}
				return nil
			},
		}
		err := usecase.WalkNamespaceSnapshotContentSubtreeWithRegistry(testCtx, k8sClient, rootNSC,
			func(_ context.Context, nsc *storagev1alpha1.NamespaceSnapshotContent) error {
				nscVisited = append(nscVisited, nsc.Name)
				return nil
			},
			integrationGraphRegProvider.Current(), hooks,
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(nscVisited).NotTo(BeEmpty())
		Expect(demoVisited).To(ContainElement(contentName), "ref-only walk should visit DemoVirtualDiskSnapshotContent leaf via same childrenSnapshotContentRefs graph")
	})
})
