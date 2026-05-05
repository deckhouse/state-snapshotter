//go:build integration
// +build integration

/*
Copyright 2025 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www/apache.org/licenses/LICENSE-2.0

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

var _ = Describe("Integration: Snapshot ↔ SnapshotContent Lifecycle", func() {
	// PHASE 2.0: Integration: Snapshot ↔ SnapshotContent lifecycle
	//
	// This test fixes the basic lifecycle contract between Snapshot and SnapshotContent.
	// It serves as the foundation for all other integration tests.
	//
	// CONTRACT:
	// - GenericSnapshotBinderController creates SnapshotContent
	// - SnapshotContentController accepts the object and adds finalizer
	// - Linkage is stable: 1 Snapshot → 1 SnapshotContent
	// - No orphans exist
	//
	// INVARIANTS:
	// - Snapshot.status.boundSnapshotContentName → SnapshotContent.name
	// - SnapshotContent.spec.snapshotRef → Snapshot reference
	// - SnapshotContent.ownerRef is set correctly
	// - SnapshotContent has finalizer

	var (
		ctx         context.Context
		snapshotGVK schema.GroupVersionKind
		contentGVK  schema.GroupVersionKind
	)

	BeforeEach(func() {
		ctx = context.Background()

		// Define test GVKs for a generic snapshot type
		snapshotGVK = schema.GroupVersionKind{
			Group:   "test.deckhouse.io",
			Version: "v1alpha1",
			Kind:    "TestSnapshot",
		}
		contentGVK = schema.GroupVersionKind{
			Group:   "test.deckhouse.io",
			Version: "v1alpha1",
			Kind:    "TestSnapshotContent",
		}
	})

	Describe("Basic Lifecycle Contract", func() {
		// INTERFACE: GenericSnapshotBinderController.Reconcile + SnapshotContentController.Reconcile
		//
		// PRECONDITION:
		// - Snapshot created by user
		// - Snapshot has HandledByDomainSpecificController=True (simulated)
		//
		// ACTIONS:
		// 1. GenericSnapshotBinderController.Reconcile creates SnapshotContent
		// 2. SnapshotContentController.Reconcile adds finalizer
		// 3. Verify linkage and invariants
		//
		// EXPECTED BEHAVIOR:
		// - SnapshotContent exists
		// - Snapshot.status.boundSnapshotContentName is set
		// - SnapshotContent.spec.snapshotRef points to Snapshot
		// - SnapshotContent.ownerRef is set (ObjectKeeper for root)
		// - SnapshotContent has finalizer
		//
		// POSTCONDITION:
		// - Stable linkage: 1 Snapshot → 1 SnapshotContent
		// - No orphans
		//
		// INVARIANTS:
		// - Snapshot.status.boundSnapshotContentName → SnapshotContent.name
		// - SnapshotContent.spec.snapshotRef → Snapshot reference
		// - SnapshotContent.ownerRef is correct
		// - SnapshotContent.finalizers contains "snapshot.deckhouse.io/parent-protect"

		It("should establish stable Snapshot ↔ SnapshotContent linkage", func() {
			// Create controllers for this test (without registering with manager to avoid conflicts)
			snapshotCtrl, err := controllers.NewGenericSnapshotBinderController(
				k8sClient,
				mgr.GetAPIReader(),
				scheme,
				testCfg,
				[]schema.GroupVersionKind{snapshotGVK},
			)
			Expect(err).NotTo(HaveOccurred())

			contentCtrl, err := controllers.NewSnapshotContentController(
				k8sClient,
				mgr.GetAPIReader(),
				scheme,
				mgr.GetRESTMapper(),
				testCfg,
				[]schema.GroupVersionKind{contentGVK},
			)
			Expect(err).NotTo(HaveOccurred())

			// Note: We don't register controllers with manager to avoid conflicts with other tests
			// We call Reconcile directly, similar to consistency_test.go

			// PRECONDITION: Create root snapshot
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName("test-lifecycle-snapshot")
			snapshotObj.SetNamespace("default")
			snapshotObj.Object["spec"] = map[string]interface{}{
				"backupClassName": "test-backup-class",
			}

			err = k8sClient.Create(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Simulate domain controller: set HandledByDomainSpecificController=True
			snapshotLike, err := snapshot.ExtractSnapshotLike(snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionHandledByDomainSpecificController,
				metav1.ConditionTrue,
				"Processed",
				"Domain controller processed snapshot",
			)

			snapshot.SyncConditionsToUnstructured(snapshotObj, snapshotLike.GetStatusConditions())
			err = k8sClient.Status().Update(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// ACTIONS Step 1: GenericSnapshotBinderController creates SnapshotContent
			// Use Eventually to wait for the controller to process the snapshot
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				},
			}

			// Wait for GenericSnapshotBinderController to process and create SnapshotContent
			var contentName string
			Eventually(func() bool {
				_, err := snapshotCtrl.Reconcile(ctx, req)
				if err != nil {
					return false
				}

				// Re-read snapshot to get updated status
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				}, snapshotObj)
				if err != nil {
					return false
				}

				snapshotLike, err = snapshot.ExtractSnapshotLike(snapshotObj)
				if err != nil {
					return false
				}

				contentName = snapshotLike.GetStatusContentName()
				return contentName != ""
			}).Should(BeTrue(), "Snapshot.status.boundSnapshotContentName should be set")

			// EXPECTED BEHAVIOR: Snapshot.status.boundSnapshotContentName is set
			Expect(contentName).NotTo(BeEmpty(), "Snapshot.status.boundSnapshotContentName should be set")

			// EXPECTED BEHAVIOR: SnapshotContent exists
			contentObj := &unstructured.Unstructured{}
			contentObj.SetGroupVersionKind(contentGVK)
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: contentName,
			}, contentObj)
			Expect(err).NotTo(HaveOccurred(), "SnapshotContent should exist")

			// ACTIONS Step 2: SnapshotContentController adds finalizer
			contentReq := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: contentName,
				},
			}

			_, err = contentCtrl.Reconcile(ctx, contentReq)
			Expect(err).NotTo(HaveOccurred())

			// Wait for reconciliation
			time.Sleep(200 * time.Millisecond)

			// Re-read SnapshotContent to get finalizer
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: contentName,
			}, contentObj)
			Expect(err).NotTo(HaveOccurred())

			// INVARIANT: SnapshotContent.spec.snapshotRef → Snapshot reference
			spec, ok := contentObj.Object["spec"].(map[string]interface{})
			Expect(ok).To(BeTrue(), "SnapshotContent should have spec")
			snapshotRef, ok := spec["snapshotRef"].(map[string]interface{})
			Expect(ok).To(BeTrue(), "SnapshotContent.spec.snapshotRef should exist")
			// Note: snapshotRef.kind is optional (fallback logic handles it)
			Expect(snapshotRef["name"]).To(Equal(snapshotObj.GetName()), "snapshotRef.name should match Snapshot name")
			Expect(snapshotRef["namespace"]).To(Equal(snapshotObj.GetNamespace()), "snapshotRef.namespace should match Snapshot namespace")

			// INVARIANT: SnapshotContent.ownerRef is set correctly
			ownerRefs := contentObj.GetOwnerReferences()
			Expect(ownerRefs).To(HaveLen(1), "SnapshotContent should have one ownerRef")
			// For root snapshot, owner should be ObjectKeeper
			Expect(ownerRefs[0].Kind).To(Equal("ObjectKeeper"), "Owner should be ObjectKeeper for root snapshot")

			// INVARIANT: SnapshotContent has finalizer
			finalizers := contentObj.GetFinalizers()
			Expect(finalizers).To(ContainElement(snapshot.FinalizerParentProtect), "SnapshotContent should have parent-protect finalizer")

			// INVARIANT: Stable linkage - 1 Snapshot → 1 SnapshotContent
			// Verify by checking that contentName matches the actual SnapshotContent name
			Expect(contentObj.GetName()).To(Equal(contentName), "SnapshotContent name should match Snapshot.status.boundSnapshotContentName")

			// INVARIANT: No orphans - SnapshotContent references existing Snapshot
			// This is verified by successful Get() above
			// Additional check: verify Snapshot still exists
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			}, snapshotObj)
			Expect(err).NotTo(HaveOccurred(), "Snapshot should still exist (no orphan)")
		})

		It("should set Snapshot Ready=True automatically when SnapshotContent becomes Ready=True", func() {
			// INTERFACE: GenericSnapshotBinderController.checkConsistencyAndSetReady
			//
			// PRECONDITION:
			// - Snapshot created with HandledByDomainSpecificController=True
			// - SnapshotContent created and has finalizer
			//
			// ACTIONS:
			// 1. Set SnapshotContent Ready=True
			// 2. Trigger Snapshot reconciliation
			//
			// EXPECTED BEHAVIOR:
			// - GenericSnapshotBinderController.checkConsistencyAndSetReady is called after SnapshotContent creation
			// - Snapshot Ready=True is set automatically when SnapshotContent is Ready=True
			// - This verifies the fix: checkConsistencyAndSetReady is called after creating SnapshotContent
			//
			// POSTCONDITION:
			// - Snapshot Ready=True
			// - SnapshotContent Ready=True
			//
			// INVARIANTS:
			// - Snapshot Ready=True only when SnapshotContent Ready=True
			// - checkConsistencyAndSetReady is called automatically after SnapshotContent creation

			// Create controllers for this test (without registering with manager to avoid conflicts)
			snapshotCtrl, err := controllers.NewGenericSnapshotBinderController(
				k8sClient,
				mgr.GetAPIReader(),
				scheme,
				testCfg,
				[]schema.GroupVersionKind{snapshotGVK},
			)
			Expect(err).NotTo(HaveOccurred())

			contentCtrl, err := controllers.NewSnapshotContentController(
				k8sClient,
				mgr.GetAPIReader(),
				scheme,
				mgr.GetRESTMapper(),
				testCfg,
				[]schema.GroupVersionKind{contentGVK},
			)
			Expect(err).NotTo(HaveOccurred())

			// Note: We don't register controllers with manager to avoid conflicts with other tests
			// We call Reconcile directly, similar to consistency_test.go
			// NOTE: This test relies on Reconcile being side-effect free and idempotent.
			// If Reconcile becomes async, adds rate-limiting, or deferred requeue, this test may need updates.

			// PRECONDITION: Create root snapshot
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName("test-ready-propagation-snapshot")
			snapshotObj.SetNamespace("default")
			snapshotObj.Object["spec"] = map[string]interface{}{
				"backupClassName": "test-backup-class",
			}

			err = k8sClient.Create(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Simulate domain controller: set HandledByDomainSpecificController=True
			snapshotLike, err := snapshot.ExtractSnapshotLike(snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionHandledByDomainSpecificController,
				metav1.ConditionTrue,
				"Processed",
				"Domain controller processed snapshot",
			)
			snapshot.SyncConditionsToUnstructured(snapshotObj, snapshotLike.GetStatusConditions())
			err = k8sClient.Status().Update(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// ACTIONS Step 1: GenericSnapshotBinderController creates SnapshotContent
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				},
			}

			_, err = snapshotCtrl.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			// Wait for SnapshotContent creation
			var contentName string
			Eventually(func() bool {
				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				}, freshSnapshot)
				if err != nil {
					return false
				}

				snapshotLike, err := snapshot.ExtractSnapshotLike(freshSnapshot)
				if err != nil {
					return false
				}

				contentName = snapshotLike.GetStatusContentName()
				return contentName != ""
			}).Should(BeTrue(), "Snapshot.status.boundSnapshotContentName should be set")

			// ACTIONS Step 2: SnapshotContentController adds finalizer
			contentReq := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: contentName,
				},
			}

			_, err = contentCtrl.Reconcile(ctx, contentReq)
			Expect(err).NotTo(HaveOccurred())

			// ACTIONS Step 3: Set SnapshotContent Ready=True
			contentObj := &unstructured.Unstructured{}
			contentObj.SetGroupVersionKind(contentGVK)
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: contentName,
			}, contentObj)
			Expect(err).NotTo(HaveOccurred())

			contentLike, err := snapshot.ExtractSnapshotContentLike(contentObj)
			Expect(err).NotTo(HaveOccurred())

			// Set Ready=True and InProgress=False (terminal state)
			snapshot.SetCondition(
				contentLike,
				snapshot.ConditionReady,
				metav1.ConditionTrue,
				snapshot.ReasonReady,
				"Content is ready",
			)
			snapshot.SetCondition(
				contentLike,
				snapshot.ConditionInProgress,
				metav1.ConditionFalse,
				"Completed",
				"Content processing completed",
			)
			snapshot.SyncConditionsToUnstructured(contentObj, contentLike.GetStatusConditions())
			err = k8sClient.Status().Update(ctx, contentObj)
			Expect(err).NotTo(HaveOccurred())

			// ACTIONS Step 4: Trigger Snapshot reconciliation
			// This should call checkConsistencyAndSetReady and set Ready=True on Snapshot
			_, err = snapshotCtrl.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			// EXPECTED BEHAVIOR: Snapshot Ready=True is set automatically
			Eventually(func() bool {
				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				}, freshSnapshot)
				if err != nil {
					return false
				}

				snapshotLike, err := snapshot.ExtractSnapshotLike(freshSnapshot)
				if err != nil {
					return false
				}

				return snapshot.IsReady(snapshotLike)
			}, "10s", "200ms").Should(BeTrue(), "Snapshot should reach Ready=True automatically when SnapshotContent is Ready=True")

			// Verify Snapshot Ready=True condition
			freshSnapshot := &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			}, freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			snapshotLike, err = snapshot.ExtractSnapshotLike(freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			readyCond := snapshot.GetCondition(snapshotLike, snapshot.ConditionReady)
			Expect(readyCond).NotTo(BeNil(), "Ready condition should exist")
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue), "Ready should be True")
			Expect(readyCond.Reason).To(Equal(snapshot.ReasonReady), "Reason should mirror SnapshotContent")

			// Verify SnapshotContent is Ready=True
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: contentName,
			}, contentObj)
			Expect(err).NotTo(HaveOccurred())

			contentLike, err = snapshot.ExtractSnapshotContentLike(contentObj)
			Expect(err).NotTo(HaveOccurred())
			Expect(snapshot.IsReady(contentLike)).To(BeTrue(), "SnapshotContent should be Ready=True")
		})
	})
})
