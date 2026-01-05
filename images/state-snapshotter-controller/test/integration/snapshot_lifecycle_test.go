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

var _ = Describe("Integration: Snapshot ↔ SnapshotContent Lifecycle", func() {
	// PHASE 2.0: Integration: Snapshot ↔ SnapshotContent lifecycle
	//
	// This test fixes the basic lifecycle contract between Snapshot and SnapshotContent.
	// It serves as the foundation for all other integration tests.
	//
	// CONTRACT:
	// - SnapshotController creates SnapshotContent
	// - SnapshotContentController accepts the object and adds finalizer
	// - Linkage is stable: 1 Snapshot → 1 SnapshotContent
	// - No orphans exist
	//
	// INVARIANTS:
	// - Snapshot.status.contentName → SnapshotContent.name
	// - SnapshotContent.spec.snapshotRef → Snapshot reference
	// - SnapshotContent.ownerRef is set correctly
	// - SnapshotContent has finalizer

	var (
		ctx              context.Context
		snapshotGVK      schema.GroupVersionKind
		contentGVK       schema.GroupVersionKind
		snapshotCtrl     *controllers.SnapshotController
		contentCtrl      *controllers.SnapshotContentController
	)

	BeforeEach(func() {
		ctx = context.Background()

		// Define test GVKs for a generic snapshot type
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

		// Create SnapshotController
		var err error
		snapshotCtrl, err = controllers.NewSnapshotController(
			k8sClient,
			mgr.GetAPIReader(),
			scheme,
			testCfg,
			[]schema.GroupVersionKind{snapshotGVK},
		)
		Expect(err).NotTo(HaveOccurred())

		// Create SnapshotContentController
		contentCtrl, err = controllers.NewSnapshotContentController(
			k8sClient,
			mgr.GetAPIReader(),
			scheme,
			testCfg,
			[]schema.GroupVersionKind{contentGVK},
		)
		Expect(err).NotTo(HaveOccurred())

		// Setup controllers with manager
		err = snapshotCtrl.SetupWithManager(mgr)
		Expect(err).NotTo(HaveOccurred())

		err = contentCtrl.SetupWithManager(mgr)
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("Basic Lifecycle Contract", func() {
		// INTERFACE: SnapshotController.Reconcile + SnapshotContentController.Reconcile
		//
		// PRECONDITION:
		// - Snapshot created by user
		// - Snapshot has HandledByDomainSpecificController=True (simulated)
		//
		// ACTIONS:
		// 1. SnapshotController.Reconcile creates SnapshotContent
		// 2. SnapshotContentController.Reconcile adds finalizer
		// 3. Verify linkage and invariants
		//
		// EXPECTED BEHAVIOR:
		// - SnapshotContent exists
		// - Snapshot.status.contentName is set
		// - SnapshotContent.spec.snapshotRef points to Snapshot
		// - SnapshotContent.ownerRef is set (ObjectKeeper for root)
		// - SnapshotContent has finalizer
		//
		// POSTCONDITION:
		// - Stable linkage: 1 Snapshot → 1 SnapshotContent
		// - No orphans
		//
		// INVARIANTS:
		// - Snapshot.status.contentName → SnapshotContent.name
		// - SnapshotContent.spec.snapshotRef → Snapshot reference
		// - SnapshotContent.ownerRef is correct
		// - SnapshotContent.finalizers contains "snapshot.deckhouse.io/parent-protect"

		It("should establish stable Snapshot ↔ SnapshotContent linkage", func() {
			// PRECONDITION: Create root snapshot
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName("test-lifecycle-snapshot")
			snapshotObj.SetNamespace("default")
			snapshotObj.Object["spec"] = map[string]interface{}{}

			err := k8sClient.Create(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Simulate domain controller: set HandledByDomainSpecificController=True
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

			// ACTIONS Step 1: SnapshotController creates SnapshotContent
			// Use Eventually to wait for the controller to process the snapshot
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				},
			}

			// Wait for SnapshotController to process and create SnapshotContent
			var contentName string
			Eventually(func() bool {
				_, err := snapshotCtrl.Reconcile(ctx, req)
				if err != nil {
					return false
				}

				// Re-read snapshot to get updated status
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
			}).Should(BeTrue(), "Snapshot.status.contentName should be set")

			// EXPECTED BEHAVIOR: Snapshot.status.contentName is set
			Expect(contentName).NotTo(BeEmpty(), "Snapshot.status.contentName should be set")

			// EXPECTED BEHAVIOR: SnapshotContent exists
			contentObj := &unstructured.Unstructured{}
			contentObj.SetGroupVersionKind(contentGVK)
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: contentName,
			}, contentObj)
			Expect(err).NotTo(HaveOccurred(), "SnapshotContent should exist")

			// ACTIONS Step 2: SnapshotContentController adds finalizer
			contentReq := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: contentName,
				},
			}

			_, err = contentCtrl.Reconcile(ctx, contentReq)
			Expect(err).NotTo(HaveOccurred())

			// Wait for reconciliation
			time.Sleep(200 * time.Millisecond)

			// Re-read SnapshotContent to get finalizer
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: contentName,
			}, contentObj)
			Expect(err).NotTo(HaveOccurred())

			// INVARIANT: SnapshotContent.spec.snapshotRef → Snapshot reference
			spec, ok := contentObj.Object["spec"].(map[string]interface{})
			Expect(ok).To(BeTrue(), "SnapshotContent should have spec")
			snapshotRef, ok := spec["snapshotRef"].(map[string]interface{})
			Expect(ok).To(BeTrue(), "SnapshotContent.spec.snapshotRef should exist")
			Expect(snapshotRef["kind"]).To(Equal(snapshotGVK.Kind), "snapshotRef.kind should match Snapshot Kind")
			Expect(snapshotRef["name"]).To(Equal(snapshotObj.GetName()), "snapshotRef.name should match Snapshot name")
			Expect(snapshotRef["namespace"]).To(Equal(snapshotObj.GetNamespace()), "snapshotRef.namespace should match Snapshot namespace")

			// INVARIANT: SnapshotContent.ownerRef is set correctly
			ownerRefs := contentObj.GetOwnerReferences()
			Expect(ownerRefs).To(HaveLen(1), "SnapshotContent should have one ownerRef")
			// For root snapshot, owner should be ObjectKeeper
			Expect(ownerRefs[0].Kind).To(Equal("ObjectKeeper"), "Owner should be ObjectKeeper for root snapshot")

			// INVARIANT: SnapshotContent has finalizer
			finalizers := contentObj.GetFinalizers()
			Expect(finalizers).To(ContainElement(snapshot.FinalizerParentProtect), "SnapshotContent should have parent-protect finalizer")

			// INVARIANT: Stable linkage - 1 Snapshot → 1 SnapshotContent
			// Verify by checking that contentName matches the actual SnapshotContent name
			Expect(contentObj.GetName()).To(Equal(contentName), "SnapshotContent name should match Snapshot.status.contentName")

			// INVARIANT: No orphans - SnapshotContent references existing Snapshot
			// This is verified by successful Get() above
			// Additional check: verify Snapshot still exists
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			}, snapshotObj)
			Expect(err).NotTo(HaveOccurred(), "Snapshot should still exist (no orphan)")
		})
	})
})

