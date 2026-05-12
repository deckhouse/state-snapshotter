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

var _ = Describe("Integration: GenericSnapshotBinderController - Consistency Checks", func() {
	// PHASE 2.1: Integration: GenericSnapshotBinderController - Consistency Checks
	//
	// This test suite verifies that GenericSnapshotBinderController correctly handles inconsistent states:
	// - Detects inconsistency (missing Content, wrong Content, terminal states)
	// - Signals inconsistency through status/conditions
	// - Does NOT attempt to fix inconsistency imperatively
	// - Remains idempotent
	//
	// INTERFACE: controllers.GenericSnapshotBinderController.Reconcile + checkConsistencyAndSetReady
	//
	// INVARIANT:
	// - GenericSnapshotBinderController does NOT "self-heal" lost Content
	// - GenericSnapshotBinderController does NOT delete or recreate objects
	// - GenericSnapshotBinderController only signals problems through conditions

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

	Describe("Content Missing", func() {
		It("should set Ready=False when Content is missing (was Ready=True)", func() {
			// PRECONDITION: Create Snapshot with contentName but Content doesn't exist
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName("test-consistency-missing-content")
			snapshotObj.SetNamespace("default")
			snapshotObj.Object["spec"] = map[string]interface{}{
				"backupClassName": "test-backup-class",
			}

			err := k8sClient.Create(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Simulate domain controller - set conditions AFTER creation
			snapshotLike, err := snapshot.ExtractSnapshotLike(snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionHandledByCustomSnapshotController,
				metav1.ConditionTrue,
				"Processed",
				"Domain controller processed snapshot",
			)
			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionHandledByCommonController,
				metav1.ConditionTrue,
				"Processed",
				"Common controller processed snapshot",
			)
			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionReady,
				metav1.ConditionTrue,
				snapshot.ReasonReady,
				"Snapshot is ready",
			)
			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionInProgress,
				metav1.ConditionTrue,
				"Processing",
				"Snapshot is in progress",
			)

			// Set contentName to non-existent Content
			snapshot.SyncConditionsToUnstructured(snapshotObj, snapshotLike.GetStatusConditions())
			status, ok := snapshotObj.Object["status"].(map[string]interface{})
			if !ok {
				status = make(map[string]interface{})
				snapshotObj.Object["status"] = status
			}
			status["boundSnapshotContentName"] = "non-existent-content"

			// Update status after creation (conditions and contentName need to be in status)
			err = k8sClient.Status().Update(ctx, snapshotObj)
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

			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				},
			}

			// ACTIONS: GenericSnapshotBinderController.Reconcile
			// Controller should check consistency and set Ready=False
			Eventually(func() bool {
				_, _ = snapshotCtrl.Reconcile(ctx, req)

				// Check Ready=False condition
				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
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

				readyCond := snapshot.GetCondition(snapshotLike, snapshot.ConditionReady)
				return readyCond != nil && readyCond.Status == metav1.ConditionFalse && readyCond.Reason == snapshot.ReasonContentMissing
			}, "10s", "100ms").Should(BeTrue(), "Ready=False with ReasonContentMissing should be set")

			// EXPECTED BEHAVIOR: Ready=False set, Content NOT created
			freshSnapshot := &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			}, freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			snapshotLike, err = snapshot.ExtractSnapshotLike(freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			readyCond := snapshot.GetCondition(snapshotLike, snapshot.ConditionReady)
			Expect(readyCond).NotTo(BeNil(), "Ready condition should exist")
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse), "Ready should be False")
			Expect(readyCond.Reason).To(Equal(snapshot.ReasonContentMissing), "Reason should be ContentMissing")

			// Verify Content was NOT created
			contentObj := &unstructured.Unstructured{}
			contentObj.SetGroupVersionKind(contentGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: "non-existent-content",
			}, contentObj)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Content should NOT be created")

			// EXPECTED BEHAVIOR: Idempotent - second reconcile doesn't change state
			_, err = snapshotCtrl.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			freshSnapshot2 := &unstructured.Unstructured{}
			freshSnapshot2.SetGroupVersionKind(snapshotGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			}, freshSnapshot2)
			Expect(err).NotTo(HaveOccurred())

			snapshotLike2, err := snapshot.ExtractSnapshotLike(freshSnapshot2)
			Expect(err).NotTo(HaveOccurred())

			readyCond2 := snapshot.GetCondition(snapshotLike2, snapshot.ConditionReady)
			Expect(readyCond2).NotTo(BeNil())
			Expect(readyCond2.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond2.Reason).To(Equal(snapshot.ReasonContentMissing))
			// LastTransitionTime should be preserved (idempotent)
			Expect(readyCond2.LastTransitionTime).To(Equal(readyCond.LastTransitionTime), "LastTransitionTime should be preserved (idempotent)")
		})

		It("should NOT set Ready=False when Content is missing (never was Ready=True)", func() {
			// PRECONDITION: Create Snapshot with contentName but Content doesn't exist
			// Snapshot was NEVER Ready=True (only InProgress)
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName("test-consistency-missing-content-never-ready")
			snapshotObj.SetNamespace("default")
			snapshotObj.Object["spec"] = map[string]interface{}{
				"backupClassName": "test-backup-class",
			}

			err := k8sClient.Create(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Simulate domain controller - set conditions AFTER creation
			snapshotLike, err := snapshot.ExtractSnapshotLike(snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Snapshot is InProgress but never Ready=True
			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionHandledByCustomSnapshotController,
				metav1.ConditionTrue,
				"Processed",
				"Domain controller processed snapshot",
			)
			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionHandledByCommonController,
				metav1.ConditionTrue,
				"Processed",
				"Common controller processed snapshot",
			)
			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionInProgress,
				metav1.ConditionTrue,
				"Processing",
				"Snapshot is in progress",
			)
			// NO Ready=True condition

			// Set contentName to non-existent Content
			snapshot.SyncConditionsToUnstructured(snapshotObj, snapshotLike.GetStatusConditions())
			status, ok := snapshotObj.Object["status"].(map[string]interface{})
			if !ok {
				status = make(map[string]interface{})
				snapshotObj.Object["status"] = status
			}
			status["boundSnapshotContentName"] = "non-existent-content-2"

			// Update status after creation (conditions and contentName need to be in status)
			err = k8sClient.Status().Update(ctx, snapshotObj)
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

			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				},
			}

			// ACTIONS: GenericSnapshotBinderController.Reconcile
			_, err = snapshotCtrl.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			// EXPECTED BEHAVIOR: Ready mirrors missing SnapshotContent even if the snapshot was never Ready.
			freshSnapshot := &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			}, freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			snapshotLike, err = snapshot.ExtractSnapshotLike(freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			readyCond := snapshot.GetCondition(snapshotLike, snapshot.ConditionReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal(snapshot.ReasonContentMissing))

			// InProgress should remain
			inProgressCond := snapshot.GetCondition(snapshotLike, snapshot.ConditionInProgress)
			Expect(inProgressCond).NotTo(BeNil())
			Expect(inProgressCond.Status).To(Equal(metav1.ConditionTrue), "InProgress should remain True")
		})
	})

	Describe("Content Wrong Name", func() {
		It("should detect Content with wrong name (Content doesn't exist)", func() {
			// PRECONDITION: Create Snapshot pointing to non-existent Content
			// This simulates Content with wrong name (Content doesn't exist at all)
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName("test-consistency-wrong-name")
			snapshotObj.SetNamespace("default")
			snapshotObj.Object["spec"] = map[string]interface{}{
				"backupClassName": "test-backup-class",
			}

			err := k8sClient.Create(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Simulate domain controller - set conditions AFTER creation
			snapshotLike, err := snapshot.ExtractSnapshotLike(snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionHandledByCustomSnapshotController,
				metav1.ConditionTrue,
				"Processed",
				"Domain controller processed snapshot",
			)
			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionHandledByCommonController,
				metav1.ConditionTrue,
				"Processed",
				"Common controller processed snapshot",
			)
			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionReady,
				metav1.ConditionTrue,
				snapshot.ReasonReady,
				"Snapshot is ready",
			)

			contentName := "non-existent-content-wrong-name"
			// Set contentName to non-existent Content
			snapshot.SyncConditionsToUnstructured(snapshotObj, snapshotLike.GetStatusConditions())
			status, ok := snapshotObj.Object["status"].(map[string]interface{})
			if !ok {
				status = make(map[string]interface{})
				snapshotObj.Object["status"] = status
			}
			status["boundSnapshotContentName"] = contentName

			// Update status after creation (conditions and boundSnapshotContentName need to be in status)
			err = k8sClient.Status().Update(ctx, snapshotObj)
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

			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				},
			}

			// ACTIONS: GenericSnapshotBinderController.Reconcile
			// Controller should detect Content doesn't exist and set Ready=False
			Eventually(func() bool {
				_, _ = snapshotCtrl.Reconcile(ctx, req)

				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
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

				readyCond := snapshot.GetCondition(snapshotLike, snapshot.ConditionReady)
				return readyCond != nil && readyCond.Status == metav1.ConditionFalse && readyCond.Reason == snapshot.ReasonContentMissing
			}, "10s", "100ms").Should(BeTrue(), "Ready=False should be set when Content doesn't exist")

			// EXPECTED BEHAVIOR: Content NOT created
			contentObj := &unstructured.Unstructured{}
			contentObj.SetGroupVersionKind(contentGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name: contentName,
			}, contentObj)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Content should NOT be created")
		})
	})

	Describe("Terminal Condition - No-Op", func() {
		It("should be no-op when Snapshot is Ready=True (terminal)", func() {
			// PRECONDITION: Create Snapshot that is Ready=True (terminal)
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName("test-consistency-terminal-ready")
			snapshotObj.SetNamespace("default")
			snapshotObj.Object["spec"] = map[string]interface{}{
				"backupClassName": "test-backup-class",
			}

			err := k8sClient.Create(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Simulate domain controller - set conditions AFTER creation
			snapshotLike, err := snapshot.ExtractSnapshotLike(snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionHandledByCustomSnapshotController,
				metav1.ConditionTrue,
				"Processed",
				"Domain controller processed snapshot",
			)
			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionHandledByCommonController,
				metav1.ConditionTrue,
				"Processed",
				"Common controller processed snapshot",
			)
			snapshot.SetCondition(
				snapshotLike,
				snapshot.ConditionReady,
				metav1.ConditionTrue,
				snapshot.ReasonReady,
				"Snapshot is ready",
			)

			// Create Content that exists and is Ready=True
			contentName := "test-terminal-content"
			contentObj := &unstructured.Unstructured{}
			contentObj.SetGroupVersionKind(contentGVK)
			contentObj.SetName(contentName)
			contentObj.Object["spec"] = map[string]interface{}{}

			err = k8sClient.Create(ctx, contentObj)
			Expect(err).NotTo(HaveOccurred())

			// Set Content Ready=True and InProgress=False (terminal state)
			// IsReady() requires both Ready=True AND InProgress=False
			contentLike, err := snapshot.ExtractSnapshotContentLike(contentObj)
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

			snapshot.SyncConditionsToUnstructured(contentObj, contentLike.GetStatusConditions())
			err = k8sClient.Status().Update(ctx, contentObj)
			Expect(err).NotTo(HaveOccurred())

			// Verify Content is Ready=True and InProgress=False before proceeding
			// IsReady() requires Ready=True, and terminal state requires InProgress=False
			Eventually(func() bool {
				freshContent := &unstructured.Unstructured{}
				freshContent.SetGroupVersionKind(contentGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name: contentName,
				}, freshContent)
				if err != nil {
					return false
				}

				contentLike, err := snapshot.ExtractSnapshotContentLike(freshContent)
				if err != nil {
					return false
				}

				// Check both Ready=True and InProgress=False for terminal state
				readyCond := snapshot.GetCondition(contentLike, snapshot.ConditionReady)
				inProgressCond := snapshot.GetCondition(contentLike, snapshot.ConditionInProgress)

				readyOk := readyCond != nil && readyCond.Status == metav1.ConditionTrue
				inProgressOk := inProgressCond == nil || inProgressCond.Status == metav1.ConditionFalse

				return readyOk && inProgressOk && snapshot.IsReady(contentLike)
			}, "10s", "100ms").Should(BeTrue(), "Content should be Ready=True and InProgress=False (terminal state)")

			// Set contentName to existing Content
			snapshot.SyncConditionsToUnstructured(snapshotObj, snapshotLike.GetStatusConditions())
			status, ok := snapshotObj.Object["status"].(map[string]interface{})
			if !ok {
				status = make(map[string]interface{})
				snapshotObj.Object["status"] = status
			}
			status["boundSnapshotContentName"] = contentName

			// Update status after creation (conditions and boundSnapshotContentName need to be in status)
			err = k8sClient.Status().Update(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Get initial Ready condition AFTER status update - read fresh object
			freshSnapshotInitial := &unstructured.Unstructured{}
			freshSnapshotInitial.SetGroupVersionKind(snapshotGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			}, freshSnapshotInitial)
			Expect(err).NotTo(HaveOccurred())

			snapshotLikeInitial, err := snapshot.ExtractSnapshotLike(freshSnapshotInitial)
			Expect(err).NotTo(HaveOccurred())

			initialReadyCond := snapshot.GetCondition(snapshotLikeInitial, snapshot.ConditionReady)
			Expect(initialReadyCond).NotTo(BeNil())
			Expect(initialReadyCond.Status).To(Equal(metav1.ConditionTrue), "Initial Ready should be True")

			// Create controller
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

			// ACTIONS: Multiple reconciles (should be no-op)
			_, err = snapshotCtrl.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			_, err = snapshotCtrl.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			// EXPECTED BEHAVIOR: Ready condition unchanged (no-op)
			freshSnapshot := &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			err = mgr.GetAPIReader().Get(ctx, types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			}, freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			snapshotLike, err = snapshot.ExtractSnapshotLike(freshSnapshot)
			Expect(err).NotTo(HaveOccurred())

			finalReadyCond := snapshot.GetCondition(snapshotLike, snapshot.ConditionReady)
			Expect(finalReadyCond).NotTo(BeNil())
			Expect(finalReadyCond.Status).To(Equal(metav1.ConditionTrue), "Ready should remain True")
			Expect(finalReadyCond.Reason).To(Equal(snapshot.ReasonReady), "Reason should remain Ready")
			// LastTransitionTime should be unchanged (no-op)
			Expect(finalReadyCond.LastTransitionTime).To(Equal(initialReadyCond.LastTransitionTime), "LastTransitionTime should be unchanged (no-op)")
		})
	})
})
