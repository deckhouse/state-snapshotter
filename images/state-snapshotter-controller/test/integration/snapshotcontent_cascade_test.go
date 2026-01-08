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

var _ = Describe("Integration: SnapshotContentController - Cascade Deletion", func() {
	// PHASE 2.2: Integration: SnapshotContentController - Cascade Finalizers Removal
	//
	// This test verifies that SnapshotContentController correctly handles cascade deletion:
	// - When parent SnapshotContent is deleted, children finalizers are removed first
	// - Then parent finalizer is removed
	// - GC can proceed with deletion through ownerRef
	//
	// INTERFACE: controllers.SnapshotContentController.Reconcile + cascadeRemoveFinalizersFromChildren
	//
	// PRECONDITION:
	// - Parent SnapshotContent exists
	// - Parent SnapshotContent.deletionTimestamp != nil
	// - Parent has children (status.childrenSnapshotContentRefs)
	// - Children have finalizers
	//
	// ACTIONS:
	// 1. Set deletionTimestamp on parent SnapshotContent
	// 2. SnapshotContentController.Reconcile(ctx, req)
	// 3. Check children finalizers
	// 4. Check parent finalizer
	//
	// EXPECTED BEHAVIOR:
	// - All children finalizers removed
	// - Parent finalizer removed
	// - GC can proceed with deletion
	//
	// POSTCONDITION:
	// - Children finalizers removed
	// - Parent finalizer removed
	//
	// INVARIANT:
	// - See GLOBAL INVARIANTS G1 (Controllers MUST NOT delete objects directly, ONLY remove finalizers)
	// - Controller does NOT call Delete() - only removes finalizers
	// - GC handles physical deletion through ownerRef

	var (
		ctx        context.Context
		contentGVK schema.GroupVersionKind
	)

	BeforeEach(func() {
		ctx = context.Background()

		// Define test GVK
		contentGVK = schema.GroupVersionKind{
			Group:   "test.deckhouse.io",
			Version: "v1alpha1",
			Kind:    "TestSnapshotContent",
		}
	})

	Describe("Cascade Finalizers Removal", func() {
		It("should remove finalizers from children before parent", func() {
			// PRECONDITION: Create parent SnapshotContent
			parentContentName := "test-cascade-parent-content"
			parentContentObj := &unstructured.Unstructured{}
			parentContentObj.SetGroupVersionKind(contentGVK)
			parentContentObj.SetName(parentContentName)
			parentContentObj.Object["spec"] = map[string]interface{}{
				"snapshotRef": map[string]interface{}{
					"kind":      "TestSnapshot",
					"name":      "test-cascade-snapshot",
					"namespace": "default",
				},
			}
			parentContentObj.Object["status"] = map[string]interface{}{}

			err := k8sClient.Create(ctx, parentContentObj)
			Expect(err).NotTo(HaveOccurred())

			// Create child SnapshotContent
			childContentName := "test-cascade-child-content"
			childContentObj := &unstructured.Unstructured{}
			childContentObj.SetGroupVersionKind(contentGVK)
			childContentObj.SetName(childContentName)
			childContentObj.Object["spec"] = map[string]interface{}{
				"snapshotRef": map[string]interface{}{
					"kind":      "TestSnapshot",
					"name":      "test-cascade-child-snapshot",
					"namespace": "default",
				},
			}
			childContentObj.Object["status"] = map[string]interface{}{}

			// Set ownerRef: child is owned by parent
			childContentObj.SetOwnerReferences([]metav1.OwnerReference{
				{
					APIVersion: contentGVK.GroupVersion().String(),
					Kind:       contentGVK.Kind,
					Name:       parentContentName,
					UID:        parentContentObj.GetUID(),
					Controller: func() *bool { b := true; return &b }(),
				},
			})

			err = k8sClient.Create(ctx, childContentObj)
			Expect(err).NotTo(HaveOccurred())

			// Set children refs on parent
			// Re-read parent to get fresh resourceVersion before update (avoid conflicts)
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: parentContentName,
			}, parentContentObj)
			Expect(err).NotTo(HaveOccurred())

			status, ok := parentContentObj.Object["status"].(map[string]interface{})
			if !ok || status == nil {
				status = make(map[string]interface{})
				parentContentObj.Object["status"] = status
			}
			status["childrenSnapshotContentRefs"] = []interface{}{
				map[string]interface{}{
					"kind": contentGVK.Kind,
					"name": childContentName,
				},
			}
			err = k8sClient.Status().Update(ctx, parentContentObj)
			Expect(err).NotTo(HaveOccurred())

			// Re-read parent to get updated status
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: parentContentName,
			}, parentContentObj)
			Expect(err).NotTo(HaveOccurred())

			// Create controller
			contentCtrl, err := controllers.NewSnapshotContentController(
				k8sClient,
				mgr.GetAPIReader(),
				scheme,
				testCfg,
				[]schema.GroupVersionKind{contentGVK},
			)
			Expect(err).NotTo(HaveOccurred())

			// Ensure finalizers are added to both parent and child
			parentReq := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: parentContentName,
				},
			}

			childReq := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: childContentName,
				},
			}

			// Add finalizer to parent
			Eventually(func() bool {
				_, err := contentCtrl.Reconcile(ctx, parentReq)
				if err != nil {
					return false
				}

				err = k8sClient.Get(ctx, types.NamespacedName{
					Name: parentContentName,
				}, parentContentObj)
				if err != nil {
					return false
				}

				return contains(parentContentObj.GetFinalizers(), snapshot.FinalizerParentProtect)
			}, "10s", "100ms").Should(BeTrue(), "Parent should have finalizer")

			// Add finalizer to child
			Eventually(func() bool {
				_, err := contentCtrl.Reconcile(ctx, childReq)
				if err != nil {
					return false
				}

				err = k8sClient.Get(ctx, types.NamespacedName{
					Name: childContentName,
				}, childContentObj)
				if err != nil {
					return false
				}

				return contains(childContentObj.GetFinalizers(), snapshot.FinalizerParentProtect)
			}, "10s", "100ms").Should(BeTrue(), "Child should have finalizer")

			// Verify PRECONDITION: Both have finalizers
			Expect(parentContentObj.GetFinalizers()).To(ContainElement(snapshot.FinalizerParentProtect))
			Expect(childContentObj.GetFinalizers()).To(ContainElement(snapshot.FinalizerParentProtect))

			// ACTIONS Step 1: Delete parent (sets deletionTimestamp)
			// Note: deletionTimestamp is immutable, must use Delete()
			err = k8sClient.Delete(ctx, parentContentObj)
			Expect(err).NotTo(HaveOccurred())

			// Wait for deletionTimestamp to be set
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: parentContentName,
				}, parentContentObj)
				if err != nil {
					return false
				}
				return parentContentObj.GetDeletionTimestamp() != nil
			}, "15s", "200ms").Should(BeTrue(), "Parent should have deletionTimestamp set")

			// ACTIONS Step 2-4: SnapshotContentController.Reconcile + Check finalizers
			// Use APIReader for live read and reconcile inside Eventually for stability
			// Use fresh objects in each poll to avoid stale pointers

			// ACTIONS Step 3: Check children finalizers (cascade happens first)
			Eventually(func() bool {
				// Trigger reconcile to ensure cascade happens
				_, _ = contentCtrl.Reconcile(ctx, parentReq)

				// Read fresh child object from live apiserver (not cache)
				freshChild := &unstructured.Unstructured{}
				freshChild.SetGroupVersionKind(contentGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name: childContentName,
				}, freshChild)
				if err != nil {
					return false
				}
				return !contains(freshChild.GetFinalizers(), snapshot.FinalizerParentProtect)
			}, "10s", "100ms").Should(BeTrue(), "Child finalizer should be removed first (cascade)")

			// ACTIONS Step 4: Check parent finalizer (removed after cascade)
			// Note: Parent finalizer is removed in the same reconcile as cascade,
			// but we need to wait for the Update to complete
			Eventually(func() bool {
				// Trigger reconcile to ensure parent finalizer removal
				_, _ = contentCtrl.Reconcile(ctx, parentReq)

				// Read fresh parent object from live apiserver (not cache)
				freshParent := &unstructured.Unstructured{}
				freshParent.SetGroupVersionKind(contentGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name: parentContentName,
				}, freshParent)
				if err != nil {
					// Object might be deleted by GC - that's OK, finalizer was removed
					return apierrors.IsNotFound(err)
				}
				return !contains(freshParent.GetFinalizers(), snapshot.FinalizerParentProtect)
			}, "20s", "100ms").Should(BeTrue(), "Parent finalizer should be removed after cascade")

			// EXPECTED BEHAVIOR: All finalizers removed
			// Re-read to verify final state (objects might be deleted by GC, which is OK)
			freshChildForCheck := &unstructured.Unstructured{}
			freshChildForCheck.SetGroupVersionKind(contentGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: childContentName,
			}, freshChildForCheck)
			if err == nil {
				// Object still exists - verify finalizer is removed
				Expect(freshChildForCheck.GetFinalizers()).NotTo(ContainElement(snapshot.FinalizerParentProtect), "Child finalizer should be removed")
			} else {
				// Object deleted by GC - that's OK, finalizer was removed
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Child should be deleted by GC after finalizer removal")
			}

			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: parentContentName,
			}, parentContentObj)
			if err == nil {
				// Object still exists - verify finalizer is removed
				Expect(parentContentObj.GetFinalizers()).NotTo(ContainElement(snapshot.FinalizerParentProtect), "Parent finalizer should be removed")
			} else {
				// Object deleted by GC - that's OK, finalizer was removed
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Parent should be deleted by GC after finalizer removal")
			}

			// EXPECTED BEHAVIOR: GC can proceed (no finalizers blocking deletion)
			// Note: We don't verify physical deletion here - that's GC's responsibility
			// This test only verifies that finalizers are removed, unlocking GC
		})
	})
})
