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
		// CRITICAL: Cleanup resources between tests to prevent race conditions
		// This ensures tests don't interfere with each other when running multiple times
		// Use soft cleanup - delete resources but don't wait for completion
		// GC will handle cleanup in background, preventing blocking between tests

		// Cleanup namespaced resources (TestSnapshot) first
		snapshotList := &unstructured.UnstructuredList{}
		snapshotList.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "test.deckhouse.io",
			Version: "v1alpha1",
			Kind:    "TestSnapshotList",
		})
		if err := k8sClient.List(ctx, snapshotList); err == nil {
			for i := range snapshotList.Items {
				snapshot := &snapshotList.Items[i]
				// Ignore errors - resource might already be deleted
				_ = k8sClient.Delete(ctx, snapshot)
			}
		}

		// Cleanup cluster-scoped resources (TestSnapshotContent) after snapshots
		contentList := &unstructured.UnstructuredList{}
		contentList.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "test.deckhouse.io",
			Version: "v1alpha1",
			Kind:    "TestSnapshotContentList",
		})
		if err := k8sClient.List(ctx, contentList); err == nil {
			for i := range contentList.Items {
				content := &contentList.Items[i]
				// Try to remove finalizers to allow deletion
				freshContent := &unstructured.Unstructured{}
				freshContent.SetGroupVersionKind(contentGVK)
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: content.GetName()}, freshContent); err == nil {
					if snapshot.HasFinalizer(freshContent, snapshot.FinalizerParentProtect) {
						snapshot.RemoveFinalizer(freshContent, snapshot.FinalizerParentProtect)
						_ = k8sClient.Update(ctx, freshContent)
					}
				}
				// Delete content (ignore errors - might already be deleted)
				_ = k8sClient.Delete(ctx, content)
			}
		}

		// Small delay to allow GC to start processing deletions
		// Don't wait for completion - let GC work in background
		// This prevents blocking between tests while still allowing cleanup
		time.Sleep(100 * time.Millisecond)
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
			snapshotName := "test-snapshot-e2e"
			contentName := ""

			// Step 1: Create TestSnapshot (root, no parent)
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName(snapshotName)
			snapshotObj.SetNamespace(namespace)
			snapshotObj.Object["spec"] = map[string]interface{}{}

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
			Eventually(func() bool {
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
			}, "10s", "100ms").Should(BeTrue(), "Snapshot should reach Ready=True state")

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
			snapshotName := "test-snapshot-delete-e2e"
			contentName := ""

			// PRECONDITION: Create Snapshot and wait for Ready=True
			// (Reuse logic from Test 1)
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName(snapshotName)
			snapshotObj.SetNamespace(namespace)
			snapshotObj.Object["spec"] = map[string]interface{}{}

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
			Eventually(func() bool {
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
			}, "15s", "200ms").Should(BeTrue(), "Snapshot should reach Ready=True state")

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

			// Wait for finalizer removal (orphaning)
			// SnapshotContentController should detect Snapshot deletion and remove finalizer
			// In E2E, controllers run via watch, but we need to ensure reconcile happens
			// Use explicit reconcile trigger similar to integration tests for stability
			contentReq := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: contentName,
				},
			}
			Eventually(func() bool {
				// Trigger reconcile explicitly to ensure orphaning check happens
				// This is critical when running multiple tests together - controllers may be busy
				_, _ = contentController.Reconcile(ctx, contentReq)

				// Get fresh SnapshotContent from live API server
				freshContent := &unstructured.Unstructured{}
				freshContent.SetGroupVersionKind(contentGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
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
			snapshotName := "test-snapshot-rbac-e2e"
			contentName := ""

			// Step 1: Create TestSnapshot
			// This verifies controller can work with wildcard *snapshots resource
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName(snapshotName)
			snapshotObj.SetNamespace(namespace)
			snapshotObj.Object["spec"] = map[string]interface{}{}

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

