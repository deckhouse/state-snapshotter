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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

var _ = Describe("Integration: GenericSnapshotBinderController - Deletion Path", func() {
	// PHASE 2.1: Integration: GenericSnapshotBinderController - Deletion Path
	//
	// This test verifies that GenericSnapshotBinderController correctly handles Snapshot deletion:
	// - GenericSnapshotBinderController NEVER deletes SnapshotContent directly
	// - GenericSnapshotBinderController does NOT break Content lifecycle
	// - GenericSnapshotBinderController removes SnapshotContent finalizer on Snapshot deletion
	// - GenericSnapshotBinderController only propagates Ready=False to parent (if applicable)
	//
	// INTERFACE: controllers.GenericSnapshotBinderController.Reconcile
	//
	// PRECONDITION:
	// - Snapshot exists
	// - SnapshotContent exists (created by GenericSnapshotBinderController)
	// - SnapshotContent has finalizer
	//
	// ACTIONS:
	// 1. Delete Snapshot (sets deletionTimestamp)
	// 2. GenericSnapshotBinderController.Reconcile(ctx, req)
	// 3. Check SnapshotContent still exists
	// 4. Check SnapshotContent finalizer removed
	// 5. Check SnapshotContent ownerRef unchanged
	//
	// EXPECTED BEHAVIOR:
	// - SnapshotContent still exists (NOT deleted by GenericSnapshotBinderController)
	// - SnapshotContent finalizer is removed by GenericSnapshotBinderController on parent deletion
	// - SnapshotContent ownerRef unchanged (lifecycle not broken)
	// - SnapshotContent can be managed by SnapshotContentController (orphaning)
	//
	// POSTCONDITION:
	// - SnapshotContent exists and is unblocked for GC by finalizer removal
	//
	// INVARIANT:
	// - See GLOBAL INVARIANTS G1 (Controllers MUST NOT delete objects directly, ONLY remove finalizers)
	// - GenericSnapshotBinderController responsibility: orchestration, NOT lifecycle management
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
		It("should remove SnapshotContent finalizer on Snapshot deletion", func() {
			// PRECONDITION: Create Snapshot
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName("test-deletion-snapshot")
			snapshotObj.SetNamespace("default")
			snapshotObj.Object["spec"] = map[string]interface{}{}

			err := k8sClient.Create(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Simulate domain controller: publish phase=Planned.
			setSnapshotDomainPlannedCurrent(ctx, snapshotObj)

			// Create controllers
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

			// Create SnapshotContent via GenericSnapshotBinderController
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

				snapshotLike, err := snapshot.ExtractSnapshotLike(freshSnapshot)
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

			// Add test finalizer to keep Snapshot around while deletionTimestamp is set
			freshSnapshot := &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			}, freshSnapshot)
			Expect(err).NotTo(HaveOccurred())
			freshSnapshot.SetFinalizers(append(freshSnapshot.GetFinalizers(), "test.finalizer"))
			err = k8sClient.Update(ctx, freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

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

			// ACTIONS Step 2: GenericSnapshotBinderController.Reconcile
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
			}, "10s", "100ms").Should(BeTrue(), "SnapshotContent should still exist (NOT deleted by GenericSnapshotBinderController)")

			// ACTIONS Step 4: Check SnapshotContent finalizers
			// GenericSnapshotBinderController removes parent-protect finalizer on Snapshot deletion
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: contentName,
			}, contentObj)
			Expect(err).NotTo(HaveOccurred())

			// GenericSnapshotBinderController should NOT have removed finalizer directly
			// (SnapshotContentController will handle it via orphaning, but that's separate)
			// We verify that SnapshotContent still exists and wasn't deleted by GenericSnapshotBinderController
			Expect(contentObj.GetName()).To(Equal(contentName), "SnapshotContent should still exist (NOT deleted by GenericSnapshotBinderController)")

			// ACTIONS Step 5: Check SnapshotContent ownerRef unchanged
			Expect(contentObj.GetOwnerReferences()).To(Equal(originalOwnerRefs), "SnapshotContent ownerRef should be unchanged (lifecycle not broken)")

			// EXPECTED BEHAVIOR: SnapshotContent exists and finalizer is removed
			Expect(contentObj.GetFinalizers()).NotTo(ContainElement(snapshot.FinalizerParentProtect), "Finalizer should be removed on Snapshot deletion")

			// Cleanup: remove test finalizer to allow snapshot deletion
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			}, snapshotObj)
			if err == nil {
				snapshotObj.SetFinalizers([]string{})
				_ = k8sClient.Update(ctx, snapshotObj)
			}
		})

		It("should remove SnapshotContent finalizer even if boundSnapshotContentName is missing", func() {
			// PRECONDITION: Create Snapshot
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName("test-deletion-snapshot-missing-content-name")
			snapshotObj.SetNamespace("default")
			snapshotObj.Object["spec"] = map[string]interface{}{}

			err := k8sClient.Create(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Simulate domain controller: publish phase=Planned.
			setSnapshotDomainPlannedCurrent(ctx, snapshotObj)

			// Create SnapshotContent manually with deterministic name (status.boundSnapshotContentName is missing)
			contentName := snapshot.GenerateSnapshotContentName(snapshotObj.GetName(), string(snapshotObj.GetUID()))
			contentObj := &unstructured.Unstructured{}
			contentObj.SetGroupVersionKind(contentGVK)
			contentObj.SetName(contentName)
			contentObj.SetFinalizers([]string{snapshot.FinalizerParentProtect})
			contentObj.Object["spec"] = map[string]interface{}{
				"deletionPolicy": "Retain",
				"snapshotRef":    integrationContentSnapshotRefMap(),
			}
			err = k8sClient.Create(ctx, contentObj)
			Expect(err).NotTo(HaveOccurred())

			// Create controller
			snapshotCtrl, err := controllers.NewGenericSnapshotBinderController(
				k8sClient,
				mgr.GetAPIReader(),
				scheme,
				testCfg,
				[]schema.GroupVersionKind{snapshotGVK},
			)
			Expect(err).NotTo(HaveOccurred())

			// Add test finalizer to keep Snapshot around while deletionTimestamp is set
			freshSnapshot := &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			}, freshSnapshot)
			Expect(err).NotTo(HaveOccurred())
			freshSnapshot.SetFinalizers(append(freshSnapshot.GetFinalizers(), "test.finalizer"))
			err = k8sClient.Update(ctx, freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			// Delete Snapshot to set deletionTimestamp
			err = k8sClient.Delete(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Trigger reconcile (deletion path)
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				},
			}
			_, err = snapshotCtrl.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			// Finalizer should be removed from SnapshotContent
			contentObj2 := &unstructured.Unstructured{}
			contentObj2.SetGroupVersionKind(contentGVK)
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: contentName,
			}, contentObj2)
			Expect(err).NotTo(HaveOccurred())
			Expect(contentObj2.GetFinalizers()).NotTo(ContainElement(snapshot.FinalizerParentProtect))

			// Cleanup: remove test finalizer
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			}, snapshotObj)
			if err == nil {
				snapshotObj.SetFinalizers([]string{})
				_ = k8sClient.Update(ctx, snapshotObj)
			}
		})

		// Block 0 (eager shell): the content object is created AND bound as soon as the Snapshot exists,
		// decoupled from the domain phase>=Planned barrier (content-single-writer design §9, the deadlock
		// fix). A Snapshot deleted BEFORE Planned must still have its content parent-protect finalizer
		// removed by the binder deletion path — the eager Retain shell lingers (recycle-bin clutter) but
		// never wedges the Snapshot's deletion (hazard H7).
		It("creates+binds the content shell before Planned and removes its finalizer on pre-Planned deletion", func() {
			// PRECONDITION: Snapshot exists but the domain has NOT reached phase=Planned.
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName("test-eager-shell-pre-planned")
			snapshotObj.SetNamespace("default")
			snapshotObj.Object["spec"] = map[string]interface{}{}

			err := k8sClient.Create(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			snapshotCtrl, err := controllers.NewGenericSnapshotBinderController(
				k8sClient,
				mgr.GetAPIReader(),
				scheme,
				testCfg,
				[]schema.GroupVersionKind{snapshotGVK},
			)
			Expect(err).NotTo(HaveOccurred())

			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				},
			}

			// Eager create+bind must complete WITHOUT the domain reaching Planned (before Block 0 the
			// content did not exist until Planned, and that wait was the deadlock).
			var contentName string
			Eventually(func() bool {
				_, err := snapshotCtrl.Reconcile(ctx, req)
				if err != nil {
					return false
				}
				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				if err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				}, freshSnapshot); err != nil {
					return false
				}
				// Invariant: the binder must not have advanced the domain phase — this proves the shell was
				// created on the eager (pre-Planned) path, not after a Planned transition.
				phase, _, _ := unstructured.NestedString(freshSnapshot.Object,
					"status", "captureState", "domainSpecificController", "phase")
				Expect(phase).NotTo(Equal(string(storagev1alpha1.SnapshotCapturePhasePlanned)))
				snapshotLike, err := snapshot.ExtractSnapshotLike(freshSnapshot)
				if err != nil {
					return false
				}
				contentName = snapshotLike.GetStatusContentName()
				return contentName != ""
			}, "10s", "100ms").Should(BeTrue(), "content shell should be created+bound eagerly before Planned")

			// The eager shell is a durable (Retain) empty content object.
			contentObj := &unstructured.Unstructured{}
			contentObj.SetGroupVersionKind(contentGVK)
			Expect(mgr.GetAPIReader().Get(ctx, types.NamespacedName{Name: contentName}, contentObj)).To(Succeed())
			policy, _, _ := unstructured.NestedString(contentObj.Object, "spec", "deletionPolicy")
			Expect(policy).To(Equal("Retain"), "eager shell must be created with deletionPolicy=Retain")

			// Simulate the content controller having finalized the shell (parent-protect finalizer), so the
			// deletion path has something to remove — otherwise the Snapshot would wedge on GC.
			Expect(mgr.GetAPIReader().Get(ctx, types.NamespacedName{Name: contentName}, contentObj)).To(Succeed())
			contentObj.SetFinalizers(append(contentObj.GetFinalizers(), snapshot.FinalizerParentProtect))
			Expect(k8sClient.Update(ctx, contentObj)).To(Succeed())

			// Keep the Snapshot around after deletion (test finalizer) so the deletion-path reconcile runs.
			freshSnapshot := &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			}, freshSnapshot)).To(Succeed())
			freshSnapshot.SetFinalizers(append(freshSnapshot.GetFinalizers(), "test.finalizer"))
			Expect(k8sClient.Update(ctx, freshSnapshot)).To(Succeed())

			// ACTION: delete the Snapshot while it is STILL pre-Planned.
			Expect(k8sClient.Delete(ctx, snapshotObj)).To(Succeed())
			Eventually(func() bool {
				fresh := &unstructured.Unstructured{}
				fresh.SetGroupVersionKind(snapshotGVK)
				if err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				}, fresh); err != nil {
					return apierrors.IsNotFound(err)
				}
				return fresh.GetDeletionTimestamp() != nil
			}, "10s", "100ms").Should(BeTrue(), "Snapshot should have deletionTimestamp set")

			// The binder deletion path must remove the content parent-protect finalizer regardless of phase.
			Eventually(func() bool {
				if _, err := snapshotCtrl.Reconcile(ctx, req); err != nil {
					return false
				}
				fresh := &unstructured.Unstructured{}
				fresh.SetGroupVersionKind(contentGVK)
				if err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{Name: contentName}, fresh); err != nil {
					return false
				}
				return !contains(fresh.GetFinalizers(), snapshot.FinalizerParentProtect)
			}, "10s", "100ms").Should(BeTrue(), "pre-Planned deletion must remove the content finalizer (no wedge)")

			// The Retain shell survives (recycle-bin clutter, not a wedge).
			Expect(mgr.GetAPIReader().Get(ctx, types.NamespacedName{Name: contentName}, contentObj)).To(Succeed())

			// Cleanup: drop the test finalizer so the Snapshot can be GC'd.
			if err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			}, snapshotObj); err == nil {
				snapshotObj.SetFinalizers([]string{})
				_ = k8sClient.Update(ctx, snapshotObj)
			}
		})
	})
})
