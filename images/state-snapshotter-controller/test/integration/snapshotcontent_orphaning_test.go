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

	Describe("Orphaning - Snapshot Deleted", func() {
		It("should not remove finalizer based on snapshot deletion (handled by GenericSnapshotBinderController)", func() {
			// PRECONDITION: Create Snapshot
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName("test-orphaning-snapshot")
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
				snapshot.ConditionHandledByCustomSnapshotController,
				metav1.ConditionTrue,
				"Processed",
				"Domain controller processed snapshot",
			)
			snapshot.SyncConditionsToUnstructured(snapshotObj, snapshotLike.GetStatusConditions())
			err = k8sClient.Status().Update(ctx, snapshotObj)
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

			_, err = snapshotCtrl.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			var contentName string
			Eventually(func() bool {
				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				}, freshSnapshot)
				if err != nil {
					return false
				}
				like, err := snapshot.ExtractSnapshotLike(freshSnapshot)
				if err != nil {
					return false
				}
				contentName = like.GetStatusContentName()
				return contentName != ""
			}, "10s", "100ms").Should(BeTrue())

			_, _ = contentCtrl.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: contentName}})
			contentObj := &unstructured.Unstructured{}
			contentObj.SetGroupVersionKind(contentGVK)
			Expect(mgr.GetAPIReader().Get(ctx, types.NamespacedName{Name: contentName}, contentObj)).To(Succeed())
			Expect(contentObj.GetFinalizers()).To(ContainElement(snapshot.FinalizerParentProtect))

			err = k8sClient.Delete(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			_, err = contentCtrl.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: contentName}})
			Expect(err).NotTo(HaveOccurred())

			freshContent := &unstructured.Unstructured{}
			freshContent.SetGroupVersionKind(contentGVK)
			Expect(mgr.GetAPIReader().Get(ctx, types.NamespacedName{Name: contentName}, freshContent)).To(Succeed())
			Expect(freshContent.GetFinalizers()).To(ContainElement(snapshot.FinalizerParentProtect))
		})
	})
})
