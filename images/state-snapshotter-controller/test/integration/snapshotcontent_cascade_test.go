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
	// PHASE 2.2: Integration: SnapshotContentController - Ensure Child Finalizers For Cascade
	//
	// Corrected teardown contract: parent teardown must ENSURE parent-protect on every live direct child and
	// remove ONLY the parent's own finalizer. It must NOT reclaim a child or strip a child finalizer: each
	// child is a durable node that runs its OWN deletion handler once ownerRef GC reaches it after the parent
	// is gone (recursively finalizing its subtree). Stripping a child finalizer here would let a deeper
	// descendant be GC'd without ever running its handler (the depth-N teardown race this change fixes).
	//
	// INTERFACE: controllers.SnapshotContentController.Reconcile + ensureFinalizersOnChildrenForCascade
	//
	// PRECONDITION:
	// - Parent SnapshotContent exists, deletionTimestamp != nil
	// - Parent has children (status.childrenSnapshotContentRefs); children have finalizers
	//
	// ACTIONS:
	// 1. Set deletionTimestamp on parent SnapshotContent
	// 2. SnapshotContentController.Reconcile(ctx, req)
	// 3. Check children finalizers RETAINED (ensured, not removed)
	// 4. Check parent's own finalizer removed
	//
	// EXPECTED BEHAVIOR:
	// - Every live child RETAINS parent-protect (the parent never strips it)
	// - Parent's own finalizer removed -> parent unblocked for GC
	// - Each child is later GC'd via ownerRef and runs its own teardown (not observable in envtest, which
	//   does not run the garbage collector, so the child simply survives here with its finalizer intact)
	//
	// POSTCONDITION:
	// - Children keep parent-protect (await their own ownerRef-driven teardown)
	// - Parent finalizer removed
	//
	// INVARIANT:
	// - See GLOBAL INVARIANTS G1 (Controllers MUST NOT delete objects directly, ONLY remove finalizers)
	// - Controller does NOT call Delete() on children - only ensures/removes its OWN finalizer
	// - GC handles physical child deletion through ownerRef; each child self-finalizes

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

	Describe("Ensure Child Finalizers For Cascade", func() {
		It("should ensure/retain child finalizers and remove only the parent's own finalizer", func() {
			// PRECONDITION: Create Snapshots (required for controllers to add finalizers)
			// Controller checks Snapshot existence before adding finalizer (prevents infinite loop)
			parentSnapshotObj := &unstructured.Unstructured{}
			parentSnapshotObj.SetGroupVersionKind(snapshotGVK)
			parentSnapshotObj.SetName("test-cascade-snapshot")
			parentSnapshotObj.SetNamespace("default")
			parentSnapshotObj.Object["spec"] = map[string]interface{}{}
			err := k8sClient.Create(ctx, parentSnapshotObj)
			Expect(err).NotTo(HaveOccurred())

			childSnapshotObj := &unstructured.Unstructured{}
			childSnapshotObj.SetGroupVersionKind(snapshotGVK)
			childSnapshotObj.SetName("test-cascade-child-snapshot")
			childSnapshotObj.SetNamespace("default")
			childSnapshotObj.Object["spec"] = map[string]interface{}{}
			err = k8sClient.Create(ctx, childSnapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// PRECONDITION: Create parent SnapshotContent
			parentContentName := "test-cascade-parent-content"
			parentContentObj := &unstructured.Unstructured{}
			parentContentObj.SetGroupVersionKind(contentGVK)
			parentContentObj.SetName(parentContentName)
			parentContentObj.Object["spec"] = map[string]interface{}{
				"deletionPolicy": "Retain",
				"snapshotRef":    integrationContentSnapshotRefMap(),
			}
			parentContentObj.Object["status"] = map[string]interface{}{}

			err = k8sClient.Create(ctx, parentContentObj)
			Expect(err).NotTo(HaveOccurred())

			// Create child SnapshotContent
			childContentName := "test-cascade-child-content"
			childContentObj := &unstructured.Unstructured{}
			childContentObj.SetGroupVersionKind(contentGVK)
			childContentObj.SetName(childContentName)
			childContentObj.Object["spec"] = map[string]interface{}{
				"deletionPolicy": "Retain",
				"snapshotRef":    integrationContentSnapshotRefMap(),
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
			// Use Eventually to handle race conditions with controller updates
			// Controller may add finalizers/update status concurrently, causing resourceVersion conflicts
			Eventually(func() error {
				// Re-read parent to get fresh resourceVersion before update (avoid conflicts)
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: parentContentName,
				}, parentContentObj)
				if err != nil {
					return err
				}

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
				return err
			}, "10s", "200ms").ShouldNot(HaveOccurred(), "Should successfully update parent status with children refs")

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
				mgr.GetRESTMapper(),
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

			// ACTIONS Step 2-4: SnapshotContentController.Reconcile + check finalizers. One deletion-path
			// reconcile ensures the child finalizer AND removes only the parent's own finalizer
			// (ensure-children runs before the parent's own removal within the same pass). Use the APIReader
			// for live reads and reconcile inside Eventually for stability.

			// ACTIONS Step 4: the parent's OWN finalizer is removed -> parent unblocked for GC / gone.
			Eventually(func() bool {
				_, _ = contentCtrl.Reconcile(ctx, parentReq)

				freshParent := &unstructured.Unstructured{}
				freshParent.SetGroupVersionKind(contentGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name: parentContentName,
				}, freshParent)
				if err != nil {
					// Removed once its last finalizer went (deletionTimestamp + no finalizers).
					return apierrors.IsNotFound(err)
				}
				return !contains(freshParent.GetFinalizers(), snapshot.FinalizerParentProtect)
			}, "20s", "100ms").Should(BeTrue(), "Parent's own finalizer should be removed")

			// ACTIONS Step 3: the child finalizer is RETAINED (ensured, never stripped by the parent). The
			// child awaits its own ownerRef-driven teardown; envtest runs no GC, so it simply survives here
			// with its finalizer and no deletionTimestamp.
			freshChild := &unstructured.Unstructured{}
			freshChild.SetGroupVersionKind(contentGVK)
			Expect(mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: childContentName,
			}, freshChild)).To(Succeed(), "child must survive the parent teardown (no GC in envtest)")
			Expect(freshChild.GetFinalizers()).To(ContainElement(snapshot.FinalizerParentProtect), "child parent-protect must be RETAINED by parent teardown, never stripped")
			Expect(freshChild.GetDeletionTimestamp()).To(BeNil(), "parent teardown must not delete the child directly")

			// Cleanup: strip the child finalizer and delete it (no GC in envtest to reclaim it via ownerRef).
			Expect(mgr.GetAPIReader().Get(ctx, types.NamespacedName{Name: childContentName}, freshChild)).To(Succeed())
			freshChild.SetFinalizers(nil)
			Expect(k8sClient.Update(ctx, freshChild)).To(Succeed())
			_ = k8sClient.Delete(ctx, freshChild)
		})
	})
})
