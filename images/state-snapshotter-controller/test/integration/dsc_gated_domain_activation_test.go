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

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

var _ = Describe("Integration: DSC-gated demo domain activation", Serial, func() {
	const dscName = "integration-dsc-gated-demo-disk"

	BeforeEach(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.DomainSpecificSnapshotController{ObjectMeta: metav1.ObjectMeta{Name: dscName}}))
		Expect(integrationSnapshotGraphRegistryRefresh(context.Background())).To(Succeed())
	})

	AfterEach(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.DomainSpecificSnapshotController{ObjectMeta: metav1.ObjectMeta{Name: dscName}}))
		Expect(integrationSnapshotGraphRegistryRefresh(context.Background())).To(Succeed())
	})

	It("ignores demo resources in NamespaceSnapshot discovery without DSC", func() {
		testCtx := context.Background()
		nsName := createIntegrationNamespace(testCtx, "dsc-gated-no-dsc-")

		Expect(k8sClient.Create(testCtx, &demov1alpha1.DemoVirtualDisk{
			ObjectMeta: metav1.ObjectMeta{Name: "disk-a", Namespace: nsName},
		})).To(Succeed())
		Expect(k8sClient.Create(testCtx, &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: nsName},
			Spec:       storagev1alpha1.NamespaceSnapshotSpec{},
		})).To(Succeed())

		var rootContentName string
		Eventually(func(g Gomega) {
			root := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: "root"}, root)).To(Succeed())
			ready := meta.FindStatusCondition(root.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(root.Status.ChildrenSnapshotRefs).To(BeEmpty())
			g.Expect(root.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			rootContentName = root.Status.BoundSnapshotContentName
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		content := &storagev1alpha1.NamespaceSnapshotContent{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: rootContentName}, content)).To(Succeed())
		Expect(content.Status.ChildrenSnapshotContentRefs).To(BeEmpty())

		diskSnapshots := &demov1alpha1.DemoVirtualDiskSnapshotList{}
		Expect(k8sClient.List(testCtx, diskSnapshots, client.InNamespace(nsName))).To(Succeed())
		Expect(diskSnapshots.Items).To(BeEmpty())
	})

	It("hot-adds demo discovery for new NamespaceSnapshots after DSC eligibility", func() {
		testCtx := context.Background()
		nsName := createIntegrationNamespace(testCtx, "dsc-gated-hot-add-")

		Expect(k8sClient.Create(testCtx, &demov1alpha1.DemoVirtualDisk{
			ObjectMeta: metav1.ObjectMeta{Name: "disk-a", Namespace: nsName},
		})).To(Succeed())
		Expect(k8sClient.Create(testCtx, &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "before-dsc", Namespace: nsName},
			Spec:       storagev1alpha1.NamespaceSnapshotSpec{},
		})).To(Succeed())
		waitNamespaceSnapshotReadyWithoutDemoChildren(testCtx, nsName, "before-dsc")

		createEligibleDemoDiskDSC(testCtx, dscName)
		integrationWaitGraphRegistryKind("DemoVirtualDiskSnapshot")

		Expect(k8sClient.Create(testCtx, &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "after-dsc", Namespace: nsName},
			Spec:       storagev1alpha1.NamespaceSnapshotSpec{},
		})).To(Succeed())

		Eventually(func(g Gomega) {
			root := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: "after-dsc"}, root)).To(Succeed())
			ready := meta.FindStatusCondition(root.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(root.Status.ChildrenSnapshotRefs).To(ContainElement(SatisfyAll(
				HaveField("APIVersion", demov1alpha1.SchemeGroupVersion.String()),
				HaveField("Kind", "DemoVirtualDiskSnapshot"),
			)))
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())
	})

	It("reconciles a manual demo snapshot without DSC", func() {
		testCtx := context.Background()
		nsName := createIntegrationNamespace(testCtx, "dsc-gated-manual-")

		Expect(k8sClient.Create(testCtx, &demov1alpha1.DemoVirtualDiskSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "manual-disk-snapshot", Namespace: nsName},
			Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
				ParentSnapshotRef: demov1alpha1.SnapshotParentRef{
					APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
					Kind:       "NamespaceSnapshot",
					Name:       "manual-parent",
				},
			},
		})).To(Succeed())

		var contentName string
		Eventually(func(g Gomega) {
			snap := &demov1alpha1.DemoVirtualDiskSnapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: "manual-disk-snapshot"}, snap)).To(Succeed())
			ready := meta.FindStatusCondition(snap.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(snap.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			contentName = snap.Status.BoundSnapshotContentName
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		content := &demov1alpha1.DemoVirtualDiskSnapshotContent{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: contentName}, content)).To(Succeed())
		Expect(content.Status.ManifestCheckpointName).NotTo(BeEmpty())
		Expect(integrationGraphRegProvider.Current().RegisteredSnapshotKinds()).NotTo(ContainElement("DemoVirtualDiskSnapshot"))
	})
})

func createIntegrationNamespace(ctx context.Context, generateName string) string {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: generateName}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	nsName := ns.Name
	DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}) })
	return nsName
}

func waitNamespaceSnapshotReadyWithoutDemoChildren(ctx context.Context, namespace, name string) {
	Eventually(func(g Gomega) {
		root := &storagev1alpha1.NamespaceSnapshot{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, root)).To(Succeed())
		ready := meta.FindStatusCondition(root.Status.Conditions, snapshot.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(root.Status.ChildrenSnapshotRefs).To(BeEmpty())
	}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())
}

func createEligibleDemoDiskDSC(ctx context.Context, name string) {
	dsc := &ssv1alpha1.DomainSpecificSnapshotController{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ssv1alpha1.DomainSpecificSnapshotControllerSpec{
			OwnerModule: "integration-dsc-gated-demo",
			SnapshotResourceMapping: []ssv1alpha1.SnapshotResourceMappingEntry{
				{
					ResourceCRDName: "demovirtualdisks.demo.state-snapshotter.deckhouse.io",
					SnapshotCRDName: "demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io",
					ContentCRDName:  "demovirtualdisksnapshotcontents.demo.state-snapshotter.deckhouse.io",
				},
			},
		},
	}
	Expect(k8sClient.Create(ctx, dsc)).To(Succeed())

	Eventually(func(g Gomega) {
		cur := &ssv1alpha1.DomainSpecificSnapshotController{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, cur)).To(Succeed())
		acc := meta.FindStatusCondition(cur.Status.Conditions, controllers.DSCConditionAccepted)
		g.Expect(acc).NotTo(BeNil())
		g.Expect(acc.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

	hook := &ssv1alpha1.DomainSpecificSnapshotController{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, hook)).To(Succeed())
	meta.SetStatusCondition(&hook.Status.Conditions, metav1.Condition{
		Type:               controllers.DSCConditionRBACReady,
		Status:             metav1.ConditionTrue,
		Reason:             "IntegrationHook",
		Message:            "dsc-gated demo activation",
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: hook.GetGeneration(),
	})
	Expect(k8sClient.Status().Update(ctx, hook)).To(Succeed())
}
