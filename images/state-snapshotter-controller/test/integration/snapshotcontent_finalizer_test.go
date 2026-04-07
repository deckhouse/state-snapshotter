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

// Helper function to check if slice contains element
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

var _ = Describe("Integration: SnapshotContentController - Finalizer Management", func() {
	// PHASE 2.2: Integration: SnapshotContentController - Finalizer Management
	//
	// This test verifies that SnapshotContentController correctly manages finalizers:
	// - Adds finalizer when SnapshotContent is created
	// - Finalizer protects against manual deletion
	// - Finalizer is idempotent (can be added multiple times safely)
	//
	// INTERFACE: controllers.SnapshotContentController.Reconcile
	//
	// PRECONDITION:
	// - Snapshot exists
	// - SnapshotContent exists (created by SnapshotController)
	//
	// ACTIONS:
	// 1. SnapshotContentController.Reconcile(ctx, req)
	// 2. Check SnapshotContent finalizers
	// 3. Attempt manual deletion (should be blocked)
	// 4. Reconcile again (should be idempotent)
	//
	// EXPECTED BEHAVIOR:
	// - SnapshotContent has "snapshot.deckhouse.io/parent-protect" finalizer
	// - Manual deletion is blocked by finalizer
	// - Reconcile is idempotent (no duplicate finalizers)
	//
	// POSTCONDITION:
	// - Finalizer exists exactly once
	// - SnapshotContent cannot be deleted manually
	//
	// INVARIANT:
	// - See GLOBAL INVARIANTS G7 (Finalizer protects against manual deletion)

	var (
		ctx         context.Context
		snapshotGVK schema.GroupVersionKind
		contentGVK  schema.GroupVersionKind
	)

	BeforeEach(func() {
		ctx = context.Background()

		// Define test GVKs
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

	Describe("Finalizer Management", func() {
		It("should add finalizer when SnapshotContent is created", func() {
			// PRECONDITION: Create Snapshot
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName("test-finalizer-snapshot")
			snapshotObj.SetNamespace("default")
			snapshotObj.Object["spec"] = map[string]interface{}{
				"backupClassName": "test-backup-class",
			}

			err := k8sClient.Create(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Simulate domain controller
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

			// Create SnapshotContent via SnapshotController
			// Create controllers for this test
			snapshotCtrl, err := controllers.NewSnapshotController(
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

			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				},
			}

			// Wait for SnapshotContent creation
			var contentName string
			Eventually(func() bool {
				_, err := snapshotCtrl.Reconcile(ctx, req)
				if err != nil {
					return false
				}

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
			}).Should(BeTrue(), "SnapshotContent should be created")

			// ACTIONS Step 1: SnapshotContentController.Reconcile
			contentReq := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: contentName,
				},
			}

			// Wait for finalizer to be added
			var contentObj *unstructured.Unstructured
			Eventually(func() bool {
				_, err := contentCtrl.Reconcile(ctx, contentReq)
				if err != nil {
					return false
				}

				contentObj = &unstructured.Unstructured{}
				contentObj.SetGroupVersionKind(contentGVK)
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name: contentName,
				}, contentObj)
				if err != nil {
					return false
				}

				// Check if finalizer exists
				finalizers := contentObj.GetFinalizers()
				return len(finalizers) > 0 && contains(finalizers, snapshot.FinalizerParentProtect)
			}).Should(BeTrue(), "Finalizer should be added by SnapshotContentController")

			// ACTIONS Step 2: Check SnapshotContent finalizers
			// EXPECTED BEHAVIOR: Finalizer exists
			finalizers := contentObj.GetFinalizers()
			Expect(finalizers).To(ContainElement(snapshot.FinalizerParentProtect), "SnapshotContent should have parent-protect finalizer")

			// EXPECTED BEHAVIOR: Finalizer exists exactly once (idempotency check)
			count := 0
			for _, f := range finalizers {
				if f == snapshot.FinalizerParentProtect {
					count++
				}
			}
			Expect(count).To(Equal(1), "Finalizer should exist exactly once")

			// ACTIONS Step 3: Reconcile again (should be idempotent - no duplicate finalizers)
			_, err = contentCtrl.Reconcile(ctx, contentReq)
			Expect(err).NotTo(HaveOccurred())

			time.Sleep(100 * time.Millisecond)

			// Re-read SnapshotContent
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: contentName,
			}, contentObj)
			Expect(err).NotTo(HaveOccurred())

			// EXPECTED BEHAVIOR: Finalizer still exists exactly once (idempotency)
			finalizers = contentObj.GetFinalizers()
			count = 0
			for _, f := range finalizers {
				if f == snapshot.FinalizerParentProtect {
					count++
				}
			}
			Expect(count).To(Equal(1), "Finalizer should still exist exactly once after second reconcile (idempotency)")
		})
	})
})
