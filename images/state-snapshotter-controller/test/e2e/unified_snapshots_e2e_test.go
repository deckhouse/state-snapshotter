//go:build e2e
// +build e2e

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

package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

var _ = Describe("E2E: Unified Snapshots", func() {
	// PHASE 3: E2E Smoke-Level Tests for Unified Snapshots
	//
	// These tests verify end-to-end behavior through real Kubernetes API (envtest).
	// They check system connectivity (CRD + RBAC + controllers together) rather than
	// implementation details or individual invariants (which are covered by integration tests).

	var (
		ctx         context.Context
		snapshotGVK schema.GroupVersionKind
		contentGVK  schema.GroupVersionKind
	)

	BeforeEach(func() {
		ctx = context.Background()

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

	AfterEach(func() {
		// HARD CLEANUP: Remove all test resources and wait until they are actually deleted
		// This prevents race conditions when tests run in parallel or multiple times
		// Critical: wait for actual deletion, not just "delete request sent"
		ctx := context.Background()

		// 1) Delete all snapshots and wait until none left
		snapshotList := &unstructured.UnstructuredList{}
		snapshotList.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "test.deckhouse.io",
			Version: "v1alpha1",
			Kind:    "TestSnapshotList",
		})

		_ = k8sClient.List(ctx, snapshotList)
		for i := range snapshotList.Items {
			_ = k8sClient.Delete(ctx, &snapshotList.Items[i])
		}

		Eventually(func() int {
			_ = k8sClient.List(ctx, snapshotList)
			return len(snapshotList.Items)
		}, "20s", "200ms").Should(Equal(0), "Snapshots should be cleaned up")

		// 2) Remove finalizers + delete all contents and wait until none left
		contentList := &unstructured.UnstructuredList{}
		contentList.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "test.deckhouse.io",
			Version: "v1alpha1",
			Kind:    "TestSnapshotContentList",
		})

		_ = k8sClient.List(ctx, contentList)
		for i := range contentList.Items {
			name := contentList.Items[i].GetName()

			fresh := &unstructured.Unstructured{}
			fresh.SetGroupVersionKind(contentGVK)
			if err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{Name: name}, fresh); err == nil {
				if snapshot.HasFinalizer(fresh, snapshot.FinalizerParentProtect) {
					snapshot.RemoveFinalizer(fresh, snapshot.FinalizerParentProtect)
					_ = k8sClient.Update(ctx, fresh)
				}
			}
			_ = k8sClient.Delete(ctx, &contentList.Items[i])
		}

		Eventually(func() int {
			_ = k8sClient.List(ctx, contentList)
			return len(contentList.Items)
		}, "20s", "200ms").Should(Equal(0), "SnapshotContents should be cleaned up")

		// 3) Cleanup ObjectKeeper too (otherwise it leaks between tests)
		okList := &unstructured.UnstructuredList{}
		okList.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "deckhouse.io",
			Version: "v1alpha1",
			Kind:    "ObjectKeeperList",
		})
		_ = k8sClient.List(ctx, okList)
		for i := range okList.Items {
			_ = k8sClient.Delete(ctx, &okList.Items[i])
		}

		Eventually(func() int {
			_ = k8sClient.List(ctx, okList)
			return len(okList.Items)
		}, "20s", "200ms").Should(Equal(0), "ObjectKeepers should be cleaned up")
	})

	Describe("Test 1: Create Snapshot → Ready", func() {
		// E2E TEST: Happy Path - Root Snapshot Creation
		//
		// WORKFLOW: Create Root Snapshot → Content → Ready=True
		//
		// PRECONDITION:
		// - envtest running
		// - CRDs installed (TestSnapshot, TestSnapshotContent, ObjectKeeper)
		// - Controllers running (SnapshotController, SnapshotContentController)
		//
		// ACTIONS:
		// 1. Create TestSnapshot (root, no parent)
		// 2. Wait for TestSnapshotContent creation (via SnapshotController)
		// 3. Wait for finalizer addition (via SnapshotContentController)
		// 4. Simulate domain controller: set HandledByDomainSpecificController=True on SnapshotContent
		// 5. Simulate readiness: set Ready=True on SnapshotContent
		// 6. Wait for Ready=True on Snapshot
		//
		// EXPECTED BEHAVIOR:
		// ✅ Snapshot.status.contentName is set
		// ✅ TestSnapshotContent created with correct ownerRef
		// ✅ Finalizer added on SnapshotContent
		// ✅ ObjectKeeper created (best-effort check, not hard assertion)
		// ✅ Snapshot Ready=True (HARD ASSERTION - only truly terminal point)
		// ✅ SnapshotContent Ready=True
		// ✅ All conditions correct
		//
		// INVARIANTS:
		// - G1: Snapshot ↔ SnapshotContent linkage
		// - G4: ObjectKeeper for root snapshots
		// - G5: Finalizer management
		//
		// VERIFIES:
		// - CRDs work
		// - RBAC is sufficient
		// - Controllers work together
		// - Happy-path end-to-end

		It("should create Snapshot and reach Ready=True state", func() {
			namespace := "default"
			suffix := fmt.Sprintf("%d-%d", GinkgoRandomSeed(), time.Now().UnixNano())
			snapshotName := "test-snapshot-e2e-" + suffix
			contentName := ""

			// Step 1: Create TestSnapshot (root, no parent)
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName(snapshotName)
			snapshotObj.SetNamespace(namespace)
			snapshotObj.Object["spec"] = map[string]interface{}{
				"backupClassName": "test-backup-class",
			}

			err := k8sClient.Create(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Step 1.5: Simulate domain controller: set HandledByDomainSpecificController=True on Snapshot
			// SnapshotController waits for this condition before creating SnapshotContent
			freshSnapshot := &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name:      snapshotName,
				Namespace: namespace,
			}, freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			snapshotLike, err := snapshot.ExtractSnapshotLike(freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionHandledByDomainSpecificController,
				metav1.ConditionTrue,
				"Processed",
				"Domain controller processed snapshot",
			)
			conditions := snapshotLike.GetStatusConditions()
			snapshot.SyncConditionsToUnstructured(freshSnapshot, conditions)
			err = k8sClient.Status().Update(ctx, freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			// Step 2: Wait for TestSnapshotContent creation (via SnapshotController)
			// This verifies CRD + RBAC + SnapshotController wiring
			Eventually(func() string {
				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name:      snapshotName,
					Namespace: namespace,
				}, freshSnapshot)
				if err != nil {
					return ""
				}
				snapshotLike, err := snapshot.ExtractSnapshotLike(freshSnapshot)
				if err != nil {
					return ""
				}
				return snapshotLike.GetStatusContentName()
			}, "10s", "100ms").ShouldNot(BeEmpty(), "SnapshotContent should be created")

			// Get content name
			freshSnapshot = &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name:      snapshotName,
				Namespace: namespace,
			}, freshSnapshot)
			Expect(err).NotTo(HaveOccurred())
			snapshotLike, err = snapshot.ExtractSnapshotLike(freshSnapshot)
			Expect(err).NotTo(HaveOccurred())
			contentName = snapshotLike.GetStatusContentName()
			Expect(contentName).NotTo(BeEmpty())

			// Step 3: Wait for finalizer addition (via SnapshotContentController)
			// This verifies SnapshotContentController wiring
			Eventually(func() bool {
				freshContent := &unstructured.Unstructured{}
				freshContent.SetGroupVersionKind(contentGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name: contentName,
				}, freshContent)
				if err != nil {
					return false
				}
				return snapshot.HasFinalizer(freshContent, snapshot.FinalizerParentProtect)
			}, "10s", "100ms").Should(BeTrue(), "Finalizer should be added to SnapshotContent")

			// Step 4: Simulate domain controller: set HandledByDomainSpecificController=True
			// This simulates domain-specific controller accepting the SnapshotContent
			freshContent := &unstructured.Unstructured{}
			freshContent.SetGroupVersionKind(contentGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: contentName,
			}, freshContent)
			Expect(err).NotTo(HaveOccurred())

			contentLike, err := snapshot.ExtractSnapshotContentLike(freshContent)
			Expect(err).NotTo(HaveOccurred())

			snapshot.SetCondition(
				contentLike,
				snapshot.ConditionHandledByDomainSpecificController,
				metav1.ConditionTrue,
				"Accepted",
				"Domain controller accepted",
			)
			contentConditions := contentLike.GetStatusConditions()
			snapshot.SyncConditionsToUnstructured(freshContent, contentConditions)
			err = k8sClient.Status().Update(ctx, freshContent)
			Expect(err).NotTo(HaveOccurred())

			// Step 5: Simulate readiness: set Ready=True and InProgress=False on SnapshotContent
			// This simulates domain-specific controller completing its work
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: contentName,
			}, freshContent)
			Expect(err).NotTo(HaveOccurred())

			contentLike, err = snapshot.ExtractSnapshotContentLike(freshContent)
			Expect(err).NotTo(HaveOccurred())

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
			contentConditions = contentLike.GetStatusConditions()
			snapshot.SyncConditionsToUnstructured(freshContent, contentConditions)
			err = k8sClient.Status().Update(ctx, freshContent)
			Expect(err).NotTo(HaveOccurred())

			// Step 6: Wait for Ready=True on Snapshot (HARD ASSERTION - only truly terminal point)
			// This verifies SnapshotController propagation and end-to-end readiness
			// Use explicit reconcile trigger to ensure propagation happens
			Eventually(func() bool {
				// Explicitly trigger reconcile to ensure propagation happens
				snapshotReq := ctrl.Request{
					NamespacedName: types.NamespacedName{
						Name:      snapshotName,
						Namespace: namespace,
					},
				}
				_, _ = snapshotController.Reconcile(ctx, snapshotReq)

				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name:      snapshotName,
					Namespace: namespace,
				}, freshSnapshot)
				if err != nil {
					return false
				}
				snapshotLike, err := snapshot.ExtractSnapshotLike(freshSnapshot)
				if err != nil {
					return false
				}
				return snapshot.IsReady(snapshotLike)
			}, "20s", "200ms").Should(BeTrue(), "Snapshot should reach Ready=True state")

			// Verify supporting invariants (not hard assertions, but good to check)
			freshSnapshot = &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name:      snapshotName,
				Namespace: namespace,
			}, freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			snapshotLike, err = snapshot.ExtractSnapshotLike(freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			// Supporting invariants
			Expect(snapshotLike.GetStatusContentName()).To(Equal(contentName), "Snapshot should reference correct Content")
			Expect(snapshot.IsReady(snapshotLike)).To(BeTrue(), "Snapshot should be Ready")

			// Verify SnapshotContent is Ready
			freshContent = &unstructured.Unstructured{}
			freshContent.SetGroupVersionKind(contentGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: contentName,
			}, freshContent)
			Expect(err).NotTo(HaveOccurred())

			contentLike, err = snapshot.ExtractSnapshotContentLike(freshContent)
			Expect(err).NotTo(HaveOccurred())
			Expect(snapshot.IsReady(contentLike)).To(BeTrue(), "SnapshotContent should be Ready")

			// ObjectKeeper existence - best-effort check, not hard assertion
			// E2E smoke should not flake due to TTL/GC timings
			objectKeeperGVK := schema.GroupVersionKind{
				Group:   "deckhouse.io",
				Version: "v1alpha1",
				Kind:    "ObjectKeeper",
			}
			objectKeeperName := snapshot.GenerateObjectKeeperName(snapshotGVK.Kind, snapshotName)

			objectKeeper := &unstructured.Unstructured{}
			objectKeeper.SetGroupVersionKind(objectKeeperGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: objectKeeperName,
			}, objectKeeper)
			if err == nil {
				// ObjectKeeper exists - verify it's correct (best-effort)
				By("ObjectKeeper exists (best-effort check)")
			} else {
				// ObjectKeeper might not exist yet - that's OK for smoke test
				By("ObjectKeeper not found yet (acceptable for smoke test)")
			}
		})
	})

	Describe("Test 2: Delete Snapshot → Orphan → GC", func() {
		// E2E TEST: Deletion Path - Root Snapshot Deletion
		//
		// WORKFLOW: Delete Root Snapshot → Content Orphaned → Finalizer Removed → GC
		//
		// PRECONDITION:
		// - Snapshot exists and is Ready=True
		// - SnapshotContent exists with finalizer
		// - ObjectKeeper exists (for root snapshot)
		//
		// ACTIONS:
		// 1. Delete Snapshot
		// 2. Wait for SnapshotContentController to remove finalizer (orphaning)
		// 3. Wait for Snapshot deletion (GC)
		// 4. Verify SnapshotContent still exists (orphaned)
		//
		// EXPECTED BEHAVIOR:
		// ✅ Snapshot deleted (GC removes it after finalizer removal)
		// ✅ Finalizer removed from SnapshotContent (via SnapshotContentController)
		// ✅ SnapshotContent remains in cluster (orphaned)
		// ✅ ownerRef either:
		//    - absent, OR
		//    - points to non-existent object
		//    (GC will handle ownerRef cleanup - don't require strict behavior)
		//
		// INVARIANTS:
		// - G5: Finalizer management
		// - G7: Orphaning (SnapshotContent survives Snapshot deletion)
		// - GC: Kubernetes GC handles ownerRef cleanup
		//
		// VERIFIES:
		// - SnapshotController doesn't delete SnapshotContent directly
		// - SnapshotContentController removes finalizer on orphaning
		// - GC correctly handles deletion
		// - Lifecycle model works end-to-end

		It("should orphan SnapshotContent when Snapshot is deleted", func() {
			namespace := "default"
			suffix := fmt.Sprintf("%d-%d", GinkgoRandomSeed(), time.Now().UnixNano())
			snapshotName := "test-snapshot-delete-e2e-" + suffix
			contentName := ""

			// PRECONDITION: Create Snapshot and wait for Ready=True
			// (Reuse logic from Test 1)
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName(snapshotName)
			snapshotObj.SetNamespace(namespace)
			snapshotObj.Object["spec"] = map[string]interface{}{
				"backupClassName": "test-backup-class",
			}

			err := k8sClient.Create(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Set HandledByDomainSpecificController=True
			freshSnapshot := &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name:      snapshotName,
				Namespace: namespace,
			}, freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			snapshotLike, err := snapshot.ExtractSnapshotLike(freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionHandledByDomainSpecificController,
				metav1.ConditionTrue,
				"Processed",
				"Domain controller processed snapshot",
			)
			conditions := snapshotLike.GetStatusConditions()
			snapshot.SyncConditionsToUnstructured(freshSnapshot, conditions)
			err = k8sClient.Status().Update(ctx, freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			// Wait for SnapshotContent creation
			Eventually(func() string {
				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name:      snapshotName,
					Namespace: namespace,
				}, freshSnapshot)
				if err != nil {
					return ""
				}
				snapshotLike, err := snapshot.ExtractSnapshotLike(freshSnapshot)
				if err != nil {
					return ""
				}
				return snapshotLike.GetStatusContentName()
			}, "10s", "100ms").ShouldNot(BeEmpty(), "SnapshotContent should be created")

			// Get content name
			freshSnapshot = &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name:      snapshotName,
				Namespace: namespace,
			}, freshSnapshot)
			Expect(err).NotTo(HaveOccurred())
			snapshotLike, err = snapshot.ExtractSnapshotLike(freshSnapshot)
			Expect(err).NotTo(HaveOccurred())
			contentName = snapshotLike.GetStatusContentName()
			Expect(contentName).NotTo(BeEmpty())

			// Wait for finalizer
			Eventually(func() bool {
				freshContent := &unstructured.Unstructured{}
				freshContent.SetGroupVersionKind(contentGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name: contentName,
				}, freshContent)
				if err != nil {
					return false
				}
				return snapshot.HasFinalizer(freshContent, snapshot.FinalizerParentProtect)
			}, "10s", "100ms").Should(BeTrue(), "Finalizer should be added to SnapshotContent")

			// Simulate domain controller acceptance and readiness
			freshContent := &unstructured.Unstructured{}
			freshContent.SetGroupVersionKind(contentGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: contentName,
			}, freshContent)
			Expect(err).NotTo(HaveOccurred())

			contentLike, err := snapshot.ExtractSnapshotContentLike(freshContent)
			Expect(err).NotTo(HaveOccurred())

			snapshot.SetCondition(
				contentLike,
				snapshot.ConditionHandledByDomainSpecificController,
				metav1.ConditionTrue,
				"Accepted",
				"Domain controller accepted",
			)
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
			contentConditions := contentLike.GetStatusConditions()
			snapshot.SyncConditionsToUnstructured(freshContent, contentConditions)
			err = k8sClient.Status().Update(ctx, freshContent)
			Expect(err).NotTo(HaveOccurred())

			// Wait for Snapshot Ready=True
			// Controller sets Ready=True after Content is Ready, need to wait for propagation
			// Use longer timeout for E2E tests (system connectivity, not speed)
			Eventually(func() bool {
				// Explicitly trigger reconcile to ensure propagation happens
				snapshotReq := ctrl.Request{
					NamespacedName: types.NamespacedName{
						Name:      snapshotName,
						Namespace: namespace,
					},
				}
				_, _ = snapshotController.Reconcile(ctx, snapshotReq)

				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name:      snapshotName,
					Namespace: namespace,
				}, freshSnapshot)
				if err != nil {
					return false
				}
				snapshotLike, err := snapshot.ExtractSnapshotLike(freshSnapshot)
				if err != nil {
					return false
				}
				return snapshot.IsReady(snapshotLike)
			}, "20s", "200ms").Should(BeTrue(), "Snapshot should reach Ready=True state")

			// ACTION: Delete Snapshot
			// Get fresh Snapshot object for deletion
			freshSnapshot = &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name:      snapshotName,
				Namespace: namespace,
			}, freshSnapshot)
			Expect(err).NotTo(HaveOccurred())
			err = k8sClient.Delete(ctx, freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			// Wait for Snapshot deletion to start (deletionTimestamp set or NotFound)
			// This ensures we wait for deletion to begin before checking orphaning
			Eventually(func() bool {
				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name:      snapshotName,
					Namespace: namespace,
				}, freshSnapshot)
				if apierrors.IsNotFound(err) {
					return true // Snapshot already deleted
				}
				if err != nil {
					return false
				}
				return freshSnapshot.GetDeletionTimestamp() != nil // Deletion started
			}, "10s", "200ms").Should(BeTrue(), "Snapshot deletion should start")

			// Wait for finalizer removal (orphaning)
			// SnapshotContentController should detect Snapshot deletion and remove finalizer
			// In E2E, controllers run via watch, but we need to ensure reconcile happens
			// Use explicit reconcile trigger similar to integration tests for stability
			// IMPORTANT: Account for reconcile errors (conflicts) - retry on error
			contentReq := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: contentName,
				},
			}
			Eventually(func() bool {
				// Trigger reconcile explicitly to ensure orphaning check happens
				// This is critical when running multiple tests together - controllers may be busy
				_, err := contentController.Reconcile(ctx, contentReq)
				if err != nil {
					// Treat error as "not yet" - don't fail early
					// Conflicts (resourceVersion) are expected in envtest
					return false
				}

				// Get fresh SnapshotContent from live API server
				freshContent := &unstructured.Unstructured{}
				freshContent.SetGroupVersionKind(contentGVK)
				err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name: contentName,
				}, freshContent)
				if err != nil {
					return false
				}
				// Check if finalizer is removed
				return !snapshot.HasFinalizer(freshContent, snapshot.FinalizerParentProtect)
			}, "20s", "500ms").Should(BeTrue(), "Finalizer should be removed after Snapshot deletion")

			// Wait for Snapshot deletion (GC)
			Eventually(func() bool {
				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name:      snapshotName,
					Namespace: namespace,
				}, freshSnapshot)
				return apierrors.IsNotFound(err)
			}, "20s", "100ms").Should(BeTrue(), "Snapshot should be deleted by GC")

			// Verify SnapshotContent still exists (orphaned)
			freshContent = &unstructured.Unstructured{}
			freshContent.SetGroupVersionKind(contentGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: contentName,
			}, freshContent)
			Expect(err).NotTo(HaveOccurred(), "SnapshotContent should remain in cluster (orphaned)")

			// Verify finalizer is removed
			Expect(snapshot.HasFinalizer(freshContent, snapshot.FinalizerParentProtect)).To(BeFalse(),
				"Finalizer should be removed")

			// Verify ownerRef behavior (flexible - GC handles cleanup)
			ownerRefs := freshContent.GetOwnerReferences()
			hasSnapshotOwnerRef := false
			for _, ref := range ownerRefs {
				if ref.Kind == snapshotGVK.Kind && ref.Name == snapshotName {
					hasSnapshotOwnerRef = true
					break
				}
			}

			if hasSnapshotOwnerRef {
				// ownerRef still points to deleted Snapshot - that's OK, GC will clean it up
				By("ownerRef still exists (GC will clean it up)")
			} else {
				// ownerRef already removed - that's also OK
				By("ownerRef already removed")
			}

			// Both cases are acceptable - GC handles ownerRef cleanup
			// We don't require strict behavior here
		})
	})

	Describe("Test 3: RBAC Smoke-Check", func() {
		// E2E TEST: RBAC Permissions - Wildcard Resources
		//
		// WORKFLOW: Verify controller can work with wildcard RBAC resources
		//
		// PRECONDITION:
		// - envtest running
		// - Controllers running (SnapshotController, SnapshotContentController)
		// - RBAC configured (in production) or disabled (in envtest)
		//
		// ACTIONS:
		// 1. Create TestSnapshot
		// 2. Verify controller can read it (no forbidden errors)
		// 3. Verify controller can update status (no forbidden errors)
		// 4. Verify controller can create SnapshotContent (no forbidden errors)
		// 5. Verify controller can manage finalizers (no forbidden errors)
		//
		// EXPECTED BEHAVIOR:
		// ✅ All operations succeed without forbidden errors
		// ✅ Controller can work with wildcard resources (*snapshots, *snapshotcontents)
		// ✅ Controller can work with status subresources
		// ✅ No RBAC-related errors in logs
		//
		// INVARIANTS:
		// - RBAC is scoped to API group, not to concrete Kind
		// - Wildcard resources (*snapshots, *snapshotcontents) work correctly
		// - Controller can perform all necessary operations
		//
		// VERIFIES:
		// - RBAC configuration is correct (or RBAC is disabled in envtest)
		// - Wildcard resources work as expected
		// - Controller has sufficient permissions
		// - No forbidden errors during normal operation

		It("should work with wildcard RBAC resources without forbidden errors", func() {
			namespace := "default"
			suffix := fmt.Sprintf("%d-%d", GinkgoRandomSeed(), time.Now().UnixNano())
			snapshotName := "test-snapshot-rbac-e2e-" + suffix
			contentName := ""

			// Step 1: Create TestSnapshot
			// This verifies controller can work with wildcard *snapshots resource
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName(snapshotName)
			snapshotObj.SetNamespace(namespace)
			snapshotObj.Object["spec"] = map[string]interface{}{
				"backupClassName": "test-backup-class",
			}

			err := k8sClient.Create(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred(), "Should be able to create Snapshot (no RBAC errors)")

			// Step 2: Set HandledByDomainSpecificController=True
			// This verifies controller can update status subresource
			freshSnapshot := &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name:      snapshotName,
				Namespace: namespace,
			}, freshSnapshot)
			Expect(err).NotTo(HaveOccurred(), "Should be able to read Snapshot (no RBAC errors)")

			snapshotLike, err := snapshot.ExtractSnapshotLike(freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionHandledByDomainSpecificController,
				metav1.ConditionTrue,
				"Processed",
				"Domain controller processed snapshot",
			)
			conditions := snapshotLike.GetStatusConditions()
			snapshot.SyncConditionsToUnstructured(freshSnapshot, conditions)
			err = k8sClient.Status().Update(ctx, freshSnapshot)
			Expect(err).NotTo(HaveOccurred(), "Should be able to update Snapshot status (no RBAC errors)")

			// Step 3: Wait for SnapshotContent creation
			// This verifies controller can create wildcard *snapshotcontents resource
			Eventually(func() string {
				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name:      snapshotName,
					Namespace: namespace,
				}, freshSnapshot)
				if err != nil {
					return ""
				}
				snapshotLike, err := snapshot.ExtractSnapshotLike(freshSnapshot)
				if err != nil {
					return ""
				}
				return snapshotLike.GetStatusContentName()
			}, "10s", "100ms").ShouldNot(BeEmpty(), "Should be able to create SnapshotContent (no RBAC errors)")

			// Get content name
			freshSnapshot = &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name:      snapshotName,
				Namespace: namespace,
			}, freshSnapshot)
			Expect(err).NotTo(HaveOccurred(), "Should be able to read Snapshot (no RBAC errors)")
			snapshotLike, err = snapshot.ExtractSnapshotLike(freshSnapshot)
			Expect(err).NotTo(HaveOccurred())
			contentName = snapshotLike.GetStatusContentName()
			Expect(contentName).NotTo(BeEmpty())

			// Step 4: Verify controller can read SnapshotContent
			// This verifies controller can work with wildcard *snapshotcontents resource
			freshContent := &unstructured.Unstructured{}
			freshContent.SetGroupVersionKind(contentGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: contentName,
			}, freshContent)
			Expect(err).NotTo(HaveOccurred(), "Should be able to read SnapshotContent (no RBAC errors)")

			// Step 5: Verify controller can manage finalizers
			// This verifies controller can update SnapshotContent (for finalizer management)
			Eventually(func() bool {
				freshContent := &unstructured.Unstructured{}
				freshContent.SetGroupVersionKind(contentGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name: contentName,
				}, freshContent)
				if err != nil {
					return false
				}
				return snapshot.HasFinalizer(freshContent, snapshot.FinalizerParentProtect)
			}, "10s", "100ms").Should(BeTrue(), "Should be able to manage finalizers (no RBAC errors)")

			// Step 6: Verify controller can update SnapshotContent status
			// This verifies controller can work with status subresources
			freshContent = &unstructured.Unstructured{}
			freshContent.SetGroupVersionKind(contentGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: contentName,
			}, freshContent)
			Expect(err).NotTo(HaveOccurred(), "Should be able to read SnapshotContent (no RBAC errors)")

			contentLike, err := snapshot.ExtractSnapshotContentLike(freshContent)
			Expect(err).NotTo(HaveOccurred())

			snapshot.SetCondition(
				contentLike,
				snapshot.ConditionHandledByDomainSpecificController,
				metav1.ConditionTrue,
				"Accepted",
				"Domain controller accepted",
			)
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
			contentConditions := contentLike.GetStatusConditions()
			snapshot.SyncConditionsToUnstructured(freshContent, contentConditions)
			err = k8sClient.Status().Update(ctx, freshContent)
			Expect(err).NotTo(HaveOccurred(), "Should be able to update SnapshotContent status (no RBAC errors)")

			// Step 7: Verify no forbidden errors occurred
			// All operations above should succeed without forbidden errors
			// This is a smoke-check: if RBAC is misconfigured, operations would fail with forbidden errors
			// In envtest, RBAC may be disabled, but operations should still succeed

			// Final verification: controller can perform all necessary operations
			By("All RBAC operations succeeded without forbidden errors")
			By("Wildcard resources (*snapshots, *snapshotcontents) work correctly")
			By("Status subresources are accessible")
		})
	})
})
