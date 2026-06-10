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

// Failure-propagation / parent-invalidation: deleting a required child snapshot must wake the parent
// (event-driven, not only via the polling fallback) so the parent recomputes coverage. Because the
// source DemoVirtualDisk still exists and is not covered, the planner recreates the child snapshot
// (same deterministic name, NEW UID), and the tree recovers to Ready=Completed.
//
// The recreated-child UID change is the deterministic evidence that the parent reconciled in response
// to the deletion (INV-FAIL-PROP); Ready=Completed afterwards is the recovery half of the contract.
var _ = Describe("Integration: child snapshot deletion invalidates and recovers parent", Serial, func() {
	const csdName = "integration-child-delete-csd"

	BeforeEach(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.CustomSnapshotDefinition{ObjectMeta: metav1.ObjectMeta{Name: csdName}}))
	})

	AfterEach(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.CustomSnapshotDefinition{ObjectMeta: metav1.ObjectMeta{Name: csdName}}))
	})

	It("recreates the deleted child and returns the root to Ready=Completed", func() {
		testCtx := context.Background()

		csd := &ssv1alpha1.CustomSnapshotDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: csdName},
			Spec: ssv1alpha1.CustomSnapshotDefinitionSpec{
				SnapshotResourceMapping: []ssv1alpha1.SnapshotResourceMappingEntry{
					{
						Source: ssv1alpha1.SnapshotGVKRef{
							APIVersion: demov1alpha1.SchemeGroupVersion.String(),
							Kind:       "DemoVirtualDisk",
						},
						Snapshot: ssv1alpha1.SnapshotGVKRef{
							APIVersion: demov1alpha1.SchemeGroupVersion.String(),
							Kind:       "DemoVirtualDiskSnapshot",
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(testCtx, csd)).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(testCtx, &ssv1alpha1.CustomSnapshotDefinition{ObjectMeta: metav1.ObjectMeta{Name: csdName}}))
		})

		Eventually(func(g Gomega) {
			cur := &ssv1alpha1.CustomSnapshotDefinition{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: csdName}, cur)).To(Succeed())
			acc := meta.FindStatusCondition(cur.Status.Conditions, controllers.CSDConditionAccepted)
			g.Expect(acc).NotTo(BeNil())
			g.Expect(acc.Status).To(Equal(metav1.ConditionTrue))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		hook := &ssv1alpha1.CustomSnapshotDefinition{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: csdName}, hook)).To(Succeed())
		gen := hook.GetGeneration()
		meta.SetStatusCondition(&hook.Status.Conditions, metav1.Condition{
			Type:               controllers.CSDConditionRBACReady,
			Status:             metav1.ConditionTrue,
			Reason:             "IntegrationHook",
			Message:            "child delete invalidation",
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: gen,
		})
		Expect(k8sClient.Status().Update(testCtx, hook)).To(Succeed())
		integrationWaitGraphRegistryKind("DemoVirtualDiskSnapshot")

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "child-delete-",
				Labels:       map[string]string{"state-snapshotter.deckhouse.io/test": "child-delete"},
			},
		}
		Expect(k8sClient.Create(testCtx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}) })

		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "child-delete-cm", Namespace: nsName}, Data: map[string]string{"k": "v"}}
		Expect(k8sClient.Create(testCtx, cm)).To(Succeed())

		disk := &demov1alpha1.DemoVirtualDisk{ObjectMeta: metav1.ObjectMeta{Name: "disk-cd", Namespace: nsName}}
		Expect(k8sClient.Create(testCtx, disk)).To(Succeed())

		root := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(testCtx, root)).To(Succeed())

		rootKey := types.NamespacedName{Namespace: nsName, Name: "root"}

		var childName string
		Eventually(func(g Gomega) {
			r := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(testCtx, rootKey, r)).To(Succeed())
			childName = ""
			for _, ch := range r.Status.ChildrenSnapshotRefs {
				if ch.APIVersion == demov1alpha1.SchemeGroupVersion.String() && ch.Kind == "DemoVirtualDiskSnapshot" {
					childName = ch.Name
					break
				}
			}
			g.Expect(childName).NotTo(BeEmpty(), "root should list the demo disk snapshot child")
		}).WithTimeout(60 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		// Baseline: root reaches Ready=Completed and we record the child's UID.
		Eventually(func(g Gomega) {
			r := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(testCtx, rootKey, r)).To(Succeed())
			rc := meta.FindStatusCondition(r.Status.Conditions, snapshot.ConditionReady)
			g.Expect(rc).NotTo(BeNil())
			g.Expect(rc.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(rc.Reason).To(Equal(snapshot.ReasonCompleted))
		}).WithTimeout(120 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		childKey := types.NamespacedName{Namespace: nsName, Name: childName}
		child0 := &demov1alpha1.DemoVirtualDiskSnapshot{}
		Expect(k8sClient.Get(testCtx, childKey, child0)).To(Succeed())
		uidBefore := child0.GetUID()
		Expect(uidBefore).NotTo(BeEmpty())

		// Destructive: delete the required child snapshot.
		Expect(k8sClient.Delete(testCtx, child0)).To(Succeed())

		// Invalidation evidence: the parent recomputes coverage and recreates the child with the same
		// deterministic name but a NEW UID. Reaching this within the timeout depends on the parent being
		// woken by the deletion event (the relay's delete-driven wake-up).
		Eventually(func(g Gomega) {
			recreated := &demov1alpha1.DemoVirtualDiskSnapshot{}
			g.Expect(k8sClient.Get(testCtx, childKey, recreated)).To(Succeed())
			g.Expect(recreated.GetUID()).NotTo(Equal(uidBefore), "deleted child must be recreated (new UID) by the woken parent")
		}).WithTimeout(60 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		// Recovery: the tree returns to Ready=Completed once the recreated child re-publishes readiness.
		Eventually(func(g Gomega) {
			r := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(testCtx, rootKey, r)).To(Succeed())
			rc := meta.FindStatusCondition(r.Status.Conditions, snapshot.ConditionReady)
			g.Expect(rc).NotTo(BeNil())
			g.Expect(rc.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(rc.Reason).To(Equal(snapshot.ReasonCompleted))
		}).WithTimeout(120 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())
	})
})
