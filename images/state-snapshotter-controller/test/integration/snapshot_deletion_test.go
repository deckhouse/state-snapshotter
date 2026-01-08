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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

var _ = Describe("Integration: SnapshotController - Deletion Path", func() {
	// PHASE 2.1: Integration: SnapshotController - Deletion Path
	//
	// This test verifies that SnapshotController correctly handles Snapshot deletion:
	// - SnapshotController NEVER deletes SnapshotContent directly
	// - SnapshotController does NOT break Content lifecycle
	// - SnapshotController does NOT manage Content finalizers
	// - SnapshotController only propagates Ready=False to parent (if applicable)
	//
	// INTERFACE: controllers.SnapshotController.Reconcile
	//
	// PRECONDITION:
	// - Snapshot exists
	// - SnapshotContent exists (created by SnapshotController)
	// - SnapshotContent has finalizer
	//
	// ACTIONS:
	// 1. Delete Snapshot (sets deletionTimestamp)
	// 2. SnapshotController.Reconcile(ctx, req)
	// 3. Check SnapshotContent still exists
	// 4. Check SnapshotContent finalizers unchanged
	// 5. Check SnapshotContent ownerRef unchanged
	//
	// EXPECTED BEHAVIOR:
	// - SnapshotContent still exists (NOT deleted by SnapshotController)
	// - SnapshotContent finalizers will be removed by SnapshotContentController (via orphaning logic)
	//   NOTE: SnapshotController doesn't manage finalizers, but SnapshotContentController does
	// - SnapshotContent ownerRef unchanged (lifecycle not broken)
	// - SnapshotContent can be managed by SnapshotContentController (orphaning)
	//
	// POSTCONDITION:
	// - SnapshotContent exists and is orphaned
	// - SnapshotContentController will handle finalizer removal (via orphaning logic)
	//
	// INVARIANT:
	// - See GLOBAL INVARIANTS G1 (Controllers MUST NOT delete objects directly, ONLY remove finalizers)
	// - SnapshotController responsibility: orchestration, NOT lifecycle management
	// - SnapshotContentController responsibility: lifecycle management (finalizers, deletion)

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

	Describe("Snapshot Deletion - Content Lifecycle Preserved", func() {
		It("should NOT delete SnapshotContent directly", func() {
			// PRECONDITION: Create Snapshot
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName("test-deletion-snapshot")
			snapshotObj.SetNamespace("default")
			snapshotObj.Object["spec"] = map[string]interface{}{}

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

			// Create controllers
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
				testCfg,
				[]schema.GroupVersionKind{contentGVK},
			)
			Expect(err).NotTo(HaveOccurred())

			// Create SnapshotContent via SnapshotController
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

				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				}, freshSnapshot)
				if err != nil {
					return false
				}

				snapshotLike, err = snapshot.ExtractSnapshotLike(freshSnapshot)
				if err != nil {
					return false
				}

				contentName = snapshotLike.GetStatusContentName()
				return contentName != ""
			}, "10s", "100ms").Should(BeTrue(), "SnapshotContent should be created")

			// Ensure finalizer is added to SnapshotContent
			contentReq := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: contentName,
				},
			}

			Eventually(func() bool {
				_, err := contentCtrl.Reconcile(ctx, contentReq)
				if err != nil {
					return false
				}

				freshContent := &unstructured.Unstructured{}
				freshContent.SetGroupVersionKind(contentGVK)
				err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name: contentName,
				}, freshContent)
				if err != nil {
					return false
				}

				return contains(freshContent.GetFinalizers(), snapshot.FinalizerParentProtect)
			}, "10s", "100ms").Should(BeTrue(), "Finalizer should be added")

			// Verify PRECONDITION: SnapshotContent exists and has finalizer
			contentObj := &unstructured.Unstructured{}
			contentObj.SetGroupVersionKind(contentGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: contentName,
			}, contentObj)
			Expect(err).NotTo(HaveOccurred())

			Expect(contentObj.GetFinalizers()).To(ContainElement(snapshot.FinalizerParentProtect), "SnapshotContent should have finalizer")
			originalOwnerRefs := contentObj.GetOwnerReferences()

			// ACTIONS Step 1: Delete Snapshot (sets deletionTimestamp)
			err = k8sClient.Delete(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Wait for deletionTimestamp to be set
			Eventually(func() bool {
				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				}, freshSnapshot)
				if err != nil {
					return apierrors.IsNotFound(err)
				}
				return freshSnapshot.GetDeletionTimestamp() != nil
			}, "10s", "100ms").Should(BeTrue(), "Snapshot should have deletionTimestamp set")

			// ACTIONS Step 2: SnapshotController.Reconcile
			_, err = snapshotCtrl.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			// ACTIONS Step 3: Check SnapshotContent still exists
			Eventually(func() bool {
				freshContent := &unstructured.Unstructured{}
				freshContent.SetGroupVersionKind(contentGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name: contentName,
				}, freshContent)
				return err == nil
			}, "10s", "100ms").Should(BeTrue(), "SnapshotContent should still exist (NOT deleted by SnapshotController)")

			// ACTIONS Step 4: Check SnapshotContent finalizers
			// NOTE: SnapshotController doesn't manage finalizers, but SnapshotContentController does
			// After Snapshot deletion, SnapshotContentController will remove finalizer via orphaning logic
			// This is correct behavior - we verify that SnapshotController doesn't interfere
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: contentName,
			}, contentObj)
			Expect(err).NotTo(HaveOccurred())

			// SnapshotController should NOT have removed finalizer directly
			// (SnapshotContentController will handle it via orphaning, but that's separate)
			// We verify that SnapshotContent still exists and wasn't deleted by SnapshotController
			Expect(contentObj.GetName()).To(Equal(contentName), "SnapshotContent should still exist (NOT deleted by SnapshotController)")

			// ACTIONS Step 5: Check SnapshotContent ownerRef unchanged
			Expect(contentObj.GetOwnerReferences()).To(Equal(originalOwnerRefs), "SnapshotContent ownerRef should be unchanged (lifecycle not broken)")

			// EXPECTED BEHAVIOR: SnapshotContent exists and is orphaned
			// SnapshotContentController will handle finalizer removal via orphaning logic
			Expect(contentObj.GetFinalizers()).To(ContainElement(snapshot.FinalizerParentProtect), "Finalizer should remain (SnapshotContentController will remove it via orphaning)")
		})
	})
})
