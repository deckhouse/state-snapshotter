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

var _ = Describe("Integration: SnapshotContentController - Orphaning", func() {
	// PHASE 2.2: Integration: SnapshotContentController - Orphaning
	//
	// This test verifies that SnapshotContentController correctly handles orphaning:
	// - When Snapshot is deleted, SnapshotContent becomes orphaned
	// - Finalizer is removed from SnapshotContent
	// - SnapshotContent can be managed by ObjectKeeper TTL
	//
	// INTERFACE: controllers.SnapshotContentController.Reconcile
	//
	// PRECONDITION:
	// - SnapshotContent exists
	// - SnapshotContent has finalizer
	// - Snapshot exists
	//
	// ACTIONS:
	// 1. Delete Snapshot
	// 2. SnapshotContentController.Reconcile(ctx, req)
	// 3. Check SnapshotContent finalizers
	//
	// EXPECTED BEHAVIOR:
	// - SnapshotContent.finalizers does NOT contain "snapshot.deckhouse.io/parent-protect"
	// - SnapshotContent becomes orphaned
	//
	// POSTCONDITION:
	// - Finalizer removed
	// - SnapshotContent orphaned
	//
	// INVARIANT:
	// - See GLOBAL INVARIANTS G5 (ObjectKeeper manages TTL ONLY for orphaned SnapshotContent)
	//
	// NOTE: Orphaned SnapshotContent MUST remain in cluster.
	// Physical deletion is responsibility of ObjectKeeper + GC.
	// This test only verifies finalizer removal, not physical deletion.

	var (
		ctx          context.Context
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

	Describe("Orphaning - Snapshot Deleted", func() {
		It("should remove finalizer when Snapshot is deleted", func() {
			// PRECONDITION: Create Snapshot
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName("test-orphaning-snapshot")
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

			// Ensure finalizer is added
			contentReq := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: contentName,
				},
			}

			// Wait for finalizer to be added
			Eventually(func() bool {
				_, err := contentCtrl.Reconcile(ctx, contentReq)
				if err != nil {
					return false
				}

				contentObj := &unstructured.Unstructured{}
				contentObj.SetGroupVersionKind(contentGVK)
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name: contentName,
				}, contentObj)
				if err != nil {
					return false
				}

				finalizers := contentObj.GetFinalizers()
				return contains(finalizers, snapshot.FinalizerParentProtect)
			}).Should(BeTrue(), "Finalizer should be added")

			// Verify PRECONDITION: SnapshotContent has finalizer
			contentObj := &unstructured.Unstructured{}
			contentObj.SetGroupVersionKind(contentGVK)
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: contentName,
			}, contentObj)
			Expect(err).NotTo(HaveOccurred())

			finalizers := contentObj.GetFinalizers()
			Expect(finalizers).To(ContainElement(snapshot.FinalizerParentProtect), "SnapshotContent should have finalizer before Snapshot deletion")

			// ACTIONS Step 1: Delete Snapshot
			err = k8sClient.Delete(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Wait for Snapshot to be deleted (use APIReader for live check)
			Eventually(func() bool {
				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				}, freshSnapshot)
				return apierrors.IsNotFound(err)
			}, "10s", "100ms").Should(BeTrue(), "Snapshot should be deleted")

			// ACTIONS Step 2 & 3: SnapshotContentController.Reconcile + Check finalizers
			// Use APIReader for live read and reconcile inside Eventually for stability
			Eventually(func() []string {
				// Trigger reconcile to ensure finalizer removal
				_, _ = contentCtrl.Reconcile(ctx, contentReq)

				// Read fresh object from live apiserver (not cache)
				freshContent := &unstructured.Unstructured{}
				freshContent.SetGroupVersionKind(contentGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name: contentName,
				}, freshContent)
				if err != nil {
					return nil
				}
				return freshContent.GetFinalizers()
			}, "10s", "100ms").ShouldNot(ContainElement(snapshot.FinalizerParentProtect), "Finalizer should be removed after Snapshot deletion")

			// EXPECTED BEHAVIOR: SnapshotContent is orphaned (no finalizer, can be managed by ObjectKeeper)
			// Re-read to verify final state
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: contentName,
			}, contentObj)
			Expect(err).NotTo(HaveOccurred())
			finalizers = contentObj.GetFinalizers()
			Expect(len(finalizers)).To(Equal(0), "SnapshotContent should have no finalizers (orphaned)")
		})

		It("should handle orphaning when snapshotRef.kind is empty (backward compatibility)", func() {
			// This test verifies backward compatibility fallback:
			// - SnapshotContent with empty snapshotRef.kind (old/broken objects)
			// - Controller should derive Snapshot Kind from SnapshotContent GVK
			// - Orphaning should work correctly even without explicit Kind

			// PRECONDITION: Create Snapshot
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName("test-orphaning-empty-kind-snapshot")
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

			// Simulate old/broken SnapshotContent: remove kind from snapshotRef
			// This simulates backward compatibility scenario
			contentObj := &unstructured.Unstructured{}
			contentObj.SetGroupVersionKind(contentGVK)
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: contentName,
			}, contentObj)
			Expect(err).NotTo(HaveOccurred())

			// Remove kind from snapshotRef to simulate old/broken object
			spec, ok := contentObj.Object["spec"].(map[string]interface{})
			Expect(ok).To(BeTrue(), "spec should exist")
			snapshotRef, ok := spec["snapshotRef"].(map[string]interface{})
			Expect(ok).To(BeTrue(), "snapshotRef should exist")
			delete(snapshotRef, "kind") // Remove kind to trigger fallback
			err = k8sClient.Update(ctx, contentObj)
			Expect(err).NotTo(HaveOccurred(), "Should be able to update SnapshotContent to remove kind")

			// Ensure finalizer is added
			contentReq := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: contentName,
				},
			}

			// Wait for finalizer to be added
			Eventually(func() bool {
				_, err := contentCtrl.Reconcile(ctx, contentReq)
				if err != nil {
					return false
				}

				freshContent := &unstructured.Unstructured{}
				freshContent.SetGroupVersionKind(contentGVK)
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name: contentName,
				}, freshContent)
				if err != nil {
					return false
				}

				finalizers := freshContent.GetFinalizers()
				return contains(finalizers, snapshot.FinalizerParentProtect)
			}).Should(BeTrue(), "Finalizer should be added")

			// Verify PRECONDITION: SnapshotContent has finalizer and empty snapshotRef.kind
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: contentName,
			}, contentObj)
			Expect(err).NotTo(HaveOccurred())

			finalizers := contentObj.GetFinalizers()
			Expect(finalizers).To(ContainElement(snapshot.FinalizerParentProtect), "SnapshotContent should have finalizer")

			// Verify snapshotRef.kind is empty (simulating old object)
			spec, ok = contentObj.Object["spec"].(map[string]interface{})
			Expect(ok).To(BeTrue())
			snapshotRef, ok = spec["snapshotRef"].(map[string]interface{})
			Expect(ok).To(BeTrue())
			_, hasKind := snapshotRef["kind"]
			Expect(hasKind).To(BeFalse(), "snapshotRef.kind should be empty (simulating old object)")

			// ACTIONS Step 1: Delete Snapshot
			err = k8sClient.Delete(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Wait for Snapshot to be deleted
			Eventually(func() bool {
				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				}, freshSnapshot)
				return apierrors.IsNotFound(err)
			}, "10s", "100ms").Should(BeTrue(), "Snapshot should be deleted")

			// ACTIONS Step 2 & 3: SnapshotContentController.Reconcile + Check finalizers
			// Fallback should derive Snapshot Kind from SnapshotContent GVK (TestSnapshotContent -> TestSnapshot)
			Eventually(func() []string {
				// Trigger reconcile - fallback should derive Kind from Content GVK
				_, _ = contentCtrl.Reconcile(ctx, contentReq)

				// Read fresh object from live apiserver
				freshContent := &unstructured.Unstructured{}
				freshContent.SetGroupVersionKind(contentGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name: contentName,
				}, freshContent)
				if err != nil {
					return nil
				}
				return freshContent.GetFinalizers()
			}, "20s", "500ms").ShouldNot(ContainElement(snapshot.FinalizerParentProtect), "Finalizer should be removed after Snapshot deletion (fallback should work)")

			// EXPECTED BEHAVIOR: SnapshotContent is orphaned (no finalizer)
			// Fallback successfully derived Snapshot Kind from SnapshotContent GVK
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: contentName,
			}, contentObj)
			Expect(err).NotTo(HaveOccurred())
			finalizers = contentObj.GetFinalizers()
			Expect(len(finalizers)).To(Equal(0), "SnapshotContent should have no finalizers (orphaned, fallback worked)")
		})
	})
})

