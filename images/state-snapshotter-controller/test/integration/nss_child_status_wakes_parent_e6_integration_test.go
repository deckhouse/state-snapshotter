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

// E6 + dynamic child watch: root NamespaceSnapshot reaches Ready=Completed with a referenced
// DemoVirtualDiskSnapshot child (same graph class as PR5a). A follow-up status-only patch on the
// child (Ready=True message tweak, no spec / generation bump) must not break parent convergence —
// guards against predicates or relay paths that ignore status-only updates.
var _ = Describe("Integration: NSS E6 parent woken by child snapshot status", Serial, func() {
	const dscName = "integration-nss-e6-status-dsc"

	BeforeEach(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.DomainSpecificSnapshotController{ObjectMeta: metav1.ObjectMeta{Name: dscName}}))
	})

	AfterEach(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.DomainSpecificSnapshotController{ObjectMeta: metav1.ObjectMeta{Name: dscName}}))
	})

	It("reaches parent Ready Completed and tolerates child status-only patches", func() {
		testCtx := context.Background()

		dsc := &ssv1alpha1.DomainSpecificSnapshotController{
			ObjectMeta: metav1.ObjectMeta{Name: dscName},
			Spec: ssv1alpha1.DomainSpecificSnapshotControllerSpec{
				OwnerModule: "integration-nss-e6-status",
				SnapshotResourceMapping: []ssv1alpha1.SnapshotResourceMappingEntry{
					{
						ResourceCRDName: "demovirtualdisks.demo.state-snapshotter.deckhouse.io",
						SnapshotCRDName: "demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io",
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
			Message:            "nss e6 status propagation",
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: gen,
		})
		Expect(k8sClient.Status().Update(testCtx, hook)).To(Succeed())
		integrationWaitGraphRegistryKind("DemoVirtualDiskSnapshot")

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-e6-status-",
				Labels:       map[string]string{"state-snapshotter.deckhouse.io/test": "nss-e6-status"},
			},
		}
		Expect(k8sClient.Create(testCtx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}) })

		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "nss-e6-cm", Namespace: nsName}, Data: map[string]string{"k": "v"}}
		Expect(k8sClient.Create(testCtx, cm)).To(Succeed())

		diskResource := &demov1alpha1.DemoVirtualDisk{
			ObjectMeta: metav1.ObjectMeta{Name: "disk-e6", Namespace: nsName},
		}
		Expect(k8sClient.Create(testCtx, diskResource)).To(Succeed())

		root := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: nsName},
			Spec:       storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(testCtx, root)).To(Succeed())

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

		Eventually(func(g Gomega) {
			r := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: "root"}, r)).To(Succeed())
			rc := meta.FindStatusCondition(r.Status.Conditions, snapshot.ConditionReady)
			g.Expect(rc).NotTo(BeNil())
			g.Expect(rc.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(rc.Reason).To(Equal(snapshot.ReasonCompleted))
		}).WithTimeout(120 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		diskKey := types.NamespacedName{Namespace: nsName, Name: diskSnapshotName}
		d0 := &demov1alpha1.DemoVirtualDiskSnapshot{}
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(testCtx, diskKey, d0)).To(Succeed())
			rc := meta.FindStatusCondition(d0.Status.Conditions, snapshot.ConditionReady)
			g.Expect(rc).NotTo(BeNil())
			g.Expect(rc.Status).To(Equal(metav1.ConditionTrue))
		}).WithTimeout(30 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())
		genBefore := d0.GetGeneration()
		Expect(genBefore).NotTo(BeZero())

		base := d0.DeepCopy()
		rc := meta.FindStatusCondition(d0.Status.Conditions, snapshot.ConditionReady)
		Expect(rc).NotTo(BeNil())
		Expect(rc.Status).To(Equal(metav1.ConditionTrue))
		rc.Message = rc.Message + " | status-only-bump"
		meta.SetStatusCondition(&d0.Status.Conditions, *rc)
		Expect(k8sClient.Status().Patch(testCtx, d0, client.MergeFrom(base))).To(Succeed())

		d1 := &demov1alpha1.DemoVirtualDiskSnapshot{}
		Expect(k8sClient.Get(testCtx, diskKey, d1)).To(Succeed())
		Expect(d1.GetGeneration()).To(Equal(genBefore), "status-only patches must not bump spec generation")

		Consistently(func(g Gomega) {
			r := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: "root"}, r)).To(Succeed())
			rc := meta.FindStatusCondition(r.Status.Conditions, snapshot.ConditionReady)
			g.Expect(rc).NotTo(BeNil())
			g.Expect(rc.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(rc.Reason).To(Equal(snapshot.ReasonCompleted))
		}).WithTimeout(15 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())
	})
})
