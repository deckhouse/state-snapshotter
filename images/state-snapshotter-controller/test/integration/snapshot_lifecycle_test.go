//go:build integration
// +build integration

/*
Copyright 2026 Flant JSC

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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

var _ = Describe("Integration: Snapshot ↔ SnapshotContent Lifecycle", func() {
	// PHASE 2.0: Integration: Snapshot ↔ SnapshotContent lifecycle
	//
	// This test fixes the basic lifecycle contract between Snapshot and SnapshotContent.
	// It serves as the foundation for all other integration tests.
	//
	// CONTRACT:
	// - GenericSnapshotBinderController creates SnapshotContent
	// - SnapshotContentController accepts the object and adds finalizer
	// - Linkage is stable: 1 Snapshot → 1 SnapshotContent
	// - No orphans exist
	//
	// INVARIANTS:
	// - Snapshot.status.boundSnapshotContentName → SnapshotContent.name
	// - SnapshotContent has no reverse runtime dependency on Snapshot
	// - SnapshotContent.ownerRef is set correctly
	// - SnapshotContent has finalizer

	var (
		ctx         context.Context
		snapshotGVK schema.GroupVersionKind
		contentGVK  schema.GroupVersionKind
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
	})

	Describe("Basic Lifecycle Contract", func() {
		// INTERFACE: GenericSnapshotBinderController.Reconcile + SnapshotContentController.Reconcile
		//
		// PRECONDITION:
		// - Snapshot created by user
		// - Snapshot has domain phase=Planned (simulated)
		//
		// ACTIONS:
		// 1. GenericSnapshotBinderController.Reconcile creates SnapshotContent
		// 2. SnapshotContentController.Reconcile adds finalizer
		// 3. Verify linkage and invariants
		//
		// EXPECTED BEHAVIOR:
		// - SnapshotContent exists
		// - Snapshot.status.boundSnapshotContentName is set
		// - SnapshotContent.spec has no reverse Snapshot reference
		// - SnapshotContent.ownerRef is set (ObjectKeeper for root)
		// - SnapshotContent has finalizer
		//
		// POSTCONDITION:
		// - Stable linkage: 1 Snapshot → 1 SnapshotContent
		// - No orphans
		//
		// INVARIANTS:
		// - Snapshot.status.boundSnapshotContentName → SnapshotContent.name
		// - SnapshotContent has no reverse runtime dependency on Snapshot
		// - SnapshotContent.ownerRef is correct
		// - SnapshotContent.finalizers contains "state-snapshotter.deckhouse.io/parent-protect"

		It("should establish stable Snapshot ↔ SnapshotContent linkage", func() {
			// Create controllers for this test (without registering with manager to avoid conflicts)
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

			// Note: We don't register controllers with manager to avoid conflicts with other tests
			// We call Reconcile directly, similar to consistency_test.go

			// PRECONDITION: Create root snapshot
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName("test-lifecycle-snapshot")
			snapshotObj.SetNamespace("default")
			snapshotObj.Object["spec"] = map[string]interface{}{}

			err = k8sClient.Create(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Simulate domain controller: publish phase=Planned.
			setSnapshotDomainPlannedCurrent(ctx, snapshotObj)

			// ACTIONS Step 1: GenericSnapshotBinderController creates SnapshotContent
			// Use Eventually to wait for the controller to process the snapshot
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				},
			}

			// Wait for GenericSnapshotBinderController to process and create SnapshotContent
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

				snapshotLike, err := snapshot.ExtractSnapshotLike(snapshotObj)
				if err != nil {
					return false
				}

				contentName = snapshotLike.GetStatusContentName()
				return contentName != ""
			}).Should(BeTrue(), "Snapshot.status.boundSnapshotContentName should be set")

			// EXPECTED BEHAVIOR: Snapshot.status.boundSnapshotContentName is set
			Expect(contentName).NotTo(BeEmpty(), "Snapshot.status.boundSnapshotContentName should be set")

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

			// INVARIANT: SnapshotContent.spec does not include a reverse Snapshot reference.
			spec, ok := contentObj.Object["spec"].(map[string]interface{})
			Expect(ok).To(BeTrue(), "SnapshotContent should have spec")
			Expect(spec).NotTo(HaveKey("snapshot"+"Ref"), "SnapshotContent must be self-contained")

			// INVARIANT: SnapshotContent.ownerRef is set correctly.
			ownerRefs := contentObj.GetOwnerReferences()
			Expect(ownerRefs).To(HaveLen(1), "SnapshotContent should have one ownerRef")
			Expect(ownerRefs[0].Kind).To(Equal("ObjectKeeper"), "Owner should be ObjectKeeper for root snapshot")
			Expect(ownerRefs[0].Controller).NotTo(BeNil())
			Expect(*ownerRefs[0].Controller).To(BeTrue())

			objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownerRefs[0].Name}, objectKeeper)).To(Succeed())
			Expect(objectKeeper.OwnerReferences).To(BeEmpty(), "root ObjectKeeper must follow Snapshot via spec.followObjectRef, not ownerRef")
			Expect(objectKeeper.Spec.FollowObjectRef).NotTo(BeNil())
			Expect(objectKeeper.Spec.FollowObjectRef.APIVersion).To(Equal(snapshotGVK.GroupVersion().String()))
			Expect(objectKeeper.Spec.FollowObjectRef.Kind).To(Equal(snapshotGVK.Kind))
			Expect(objectKeeper.Spec.FollowObjectRef.Namespace).To(Equal(snapshotObj.GetNamespace()))
			Expect(objectKeeper.Spec.FollowObjectRef.Name).To(Equal(snapshotObj.GetName()))
			Expect(objectKeeper.Spec.FollowObjectRef.UID).To(Equal(string(snapshotObj.GetUID())))

			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			}, snapshotObj)
			Expect(err).NotTo(HaveOccurred(), "Snapshot should still exist (no orphan)")
			snapshotOwnerRefs := snapshotObj.GetOwnerReferences()
			for _, ref := range snapshotOwnerRefs {
				Expect(ref.Kind).NotTo(Equal("ObjectKeeper"), "root Snapshot must not be owned by cluster-scoped ObjectKeeper")
			}

			// INVARIANT: SnapshotContent has finalizer
			finalizers := contentObj.GetFinalizers()
			Expect(finalizers).To(ContainElement(snapshot.FinalizerParentProtect), "SnapshotContent should have parent-protect finalizer")

			// INVARIANT: Stable linkage - 1 Snapshot → 1 SnapshotContent
			// Verify by checking that contentName matches the actual SnapshotContent name
			Expect(contentObj.GetName()).To(Equal(contentName), "SnapshotContent name should match Snapshot.status.boundSnapshotContentName")

			// INVARIANT: No orphans - root ObjectKeeper follows Snapshot and owns retained root SnapshotContent.
		})

		It("should set Snapshot Ready=True automatically when SnapshotContent becomes Ready=True", func() {
			// Current contract: the bound common SnapshotContent is driven Ready=True by its controller
			// (source of truth) and Snapshot.Ready is a verbatim mirror of it. This previously could not
			// converge in envtest: the binder self-asserted PlanningReady with observedGeneration=0 inside the
			// mirror step, which its own generation-gated Step-1 barrier then rejected on every subsequent
			// reconcile, so the mirror never re-ran after the content became Ready. Fixed by no longer
			// writing PlanningReady in the mirror step (genericbinder.checkConsistencyAndSetReady): PlanningReady
			// is owned upstream (bind-time publish for childless generic snapshots / the domain controller for
			// subtree snapshots) and the Step-1 barrier already guarantees it before the mirror runs.
			// INTERFACE: GenericSnapshotBinderController.checkConsistencyAndSetReady
			//
			// PRECONDITION:
			// - Snapshot created with domain phase=Planned
			// - SnapshotContent created and has finalizer
			//
			// ACTIONS:
			// 1. Set SnapshotContent Ready=True
			// 2. Trigger Snapshot reconciliation
			//
			// EXPECTED BEHAVIOR:
			// - GenericSnapshotBinderController.checkConsistencyAndSetReady is called after SnapshotContent creation
			// - Snapshot Ready=True is set automatically when SnapshotContent is Ready=True
			// - This verifies the fix: checkConsistencyAndSetReady is called after creating SnapshotContent
			//
			// POSTCONDITION:
			// - Snapshot Ready=True
			// - SnapshotContent Ready=True
			//
			// INVARIANTS:
			// - Snapshot Ready=True only when SnapshotContent Ready=True
			// - checkConsistencyAndSetReady is called automatically after SnapshotContent creation

			// Current model: every Snapshot binds the common SnapshotContent and Snapshot.Ready is an eventual
			// verbatim mirror of that common content's Ready (the binder reads the typed common SnapshotContent
			// by name, not a custom TestSnapshotContent). Bind TestSnapshot -> common content so the mirror can
			// resolve the bound content.
			// Construct with nil so the constructor does NOT auto-register TestSnapshot -> TestSnapshotContent;
			// the only content mapping is the explicit TestSnapshot -> common SnapshotContent below (matching
			// the binder's runtime registration). Without this, checkConsistencyAndSetReady would mirror the
			// wrong (custom) content GVK and never observe the common content's Ready.
			boundContentGVK := unifiedbootstrap.CommonSnapshotContentGVK()
			snapshotCtrl, err := controllers.NewGenericSnapshotBinderController(
				k8sClient,
				mgr.GetAPIReader(),
				scheme,
				testCfg,
				nil,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(snapshotCtrl.GVKRegistry.RegisterSnapshotContentMapping(
				snapshotGVK.Kind,
				snapshotGVK.GroupVersion().String(),
				boundContentGVK.Kind,
				boundContentGVK.GroupVersion().String(),
			)).To(Succeed())
			snapshotCtrl.SnapshotGVKs = []schema.GroupVersionKind{snapshotGVK}

			contentCtrl, err := controllers.NewSnapshotContentController(
				k8sClient,
				mgr.GetAPIReader(),
				scheme,
				mgr.GetRESTMapper(),
				testCfg,
				[]schema.GroupVersionKind{boundContentGVK},
			)
			Expect(err).NotTo(HaveOccurred())

			// Note: We don't register controllers with manager to avoid conflicts with other tests
			// We call Reconcile directly, similar to consistency_test.go
			// NOTE: This test relies on Reconcile being side-effect free and idempotent.
			// If Reconcile becomes async, adds rate-limiting, or deferred requeue, this test may need updates.

			// PRECONDITION: Create root snapshot
			snapshotObj := &unstructured.Unstructured{}
			snapshotObj.SetGroupVersionKind(snapshotGVK)
			snapshotObj.SetName("test-ready-propagation-snapshot")
			snapshotObj.SetNamespace("default")
			snapshotObj.Object["spec"] = map[string]interface{}{}

			err = k8sClient.Create(ctx, snapshotObj)
			Expect(err).NotTo(HaveOccurred())

			// Simulate domain controller: publish phase=Planned.
			setSnapshotDomainPlannedCurrent(ctx, snapshotObj)

			// ACTIONS Step 1: GenericSnapshotBinderController creates SnapshotContent
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				},
			}

			_, err = snapshotCtrl.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			// Wait for SnapshotContent creation
			var contentName string
			Eventually(func() bool {
				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				err := k8sClient.Get(ctx, types.NamespacedName{
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
			}).Should(BeTrue(), "Snapshot.status.boundSnapshotContentName should be set")

			// ACTIONS Step 2: SnapshotContentController adds finalizer
			contentReq := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: contentName,
				},
			}

			_, err = contentCtrl.Reconcile(ctx, contentReq)
			Expect(err).NotTo(HaveOccurred())

			// ACTIONS Step 3: Snapshot Ready mirror must keep polling while content is pending.
			result, err := snapshotCtrl.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", time.Duration(0)), "generic binder must poll pending content because SnapshotContent has no reverse Snapshot watch")

			freshPendingSnapshot := &unstructured.Unstructured{}
			freshPendingSnapshot.SetGroupVersionKind(snapshotGVK)
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: snapshotObj.GetName(), Namespace: snapshotObj.GetNamespace()}, freshPendingSnapshot)).To(Succeed())
			pendingLike, err := snapshot.ExtractSnapshotLike(freshPendingSnapshot)
			Expect(err).NotTo(HaveOccurred())
			pendingReady := snapshot.GetCondition(pendingLike, snapshot.ConditionReady)
			Expect(pendingReady).NotTo(BeNil())
			Expect(pendingReady.Status).To(Equal(metav1.ConditionFalse))

			// ACTIONS Step 4: drive the bound (common) SnapshotContent genuinely Ready. The controller owns
			// Ready, so the test must not force-write it: publish a Ready ManifestCheckpoint and link it via
			// status.manifestCheckpointName, then let the SnapshotContentController compute
			// ManifestsReady=True/VolumeReady=True/Ready=True (the content has no children and no data refs). The generic binder
			// does not build a root MCR and does not touch manifestCheckpointName while
			// status.captureState.domainSpecificController.manifestCaptureRequestName is empty, so this link is stable.
			boundContent := &storagev1alpha1.SnapshotContent{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: contentName}, boundContent)).To(Succeed())
			contentUID := string(boundContent.GetUID())

			mcpName := "mcp-" + contentName
			ensureManifestCheckpointStatus(ctx, mcpName, contentName, contentUID, func(m *ssv1alpha1.ManifestCheckpoint) {
				meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
					Type:    ssv1alpha1.ManifestCheckpointConditionTypeReady,
					Status:  metav1.ConditionTrue,
					Reason:  ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
					Message: "checkpoint ready",
				})
			})
			DeferCleanup(func() {
				_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: mcpName}}))
			})

			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				cur := &storagev1alpha1.SnapshotContent{}
				if getErr := k8sClient.Get(ctx, types.NamespacedName{Name: contentName}, cur); getErr != nil {
					return getErr
				}
				cur.Status.ManifestCheckpointName = mcpName
				return k8sClient.Status().Update(ctx, cur)
			})).To(Succeed())

			Eventually(func(g Gomega) {
				_, recErr := contentCtrl.Reconcile(ctx, contentReq)
				g.Expect(recErr).NotTo(HaveOccurred())
				fresh := &storagev1alpha1.SnapshotContent{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: contentName}, fresh)).To(Succeed())
				g.Expect(meta.IsStatusConditionTrue(fresh.Status.Conditions, snapshot.ConditionReady)).To(BeTrue())
			}, "60s", "200ms").Should(Succeed(), "bound SnapshotContent should become Ready=True")

			// ACTIONS Step 5: Snapshot.Ready is a verbatim mirror of the bound SnapshotContent.Ready. Keep
			// reconciling the binder (there is no reverse Snapshot watch) until the mirror converges, then
			// assert the Snapshot is Ready=True with the same (Completed) reason as the content.
			Eventually(func(g Gomega) {
				_, recErr := snapshotCtrl.Reconcile(ctx, req)
				g.Expect(recErr).NotTo(HaveOccurred())

				freshSnapshot := &unstructured.Unstructured{}
				freshSnapshot.SetGroupVersionKind(snapshotGVK)
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      snapshotObj.GetName(),
					Namespace: snapshotObj.GetNamespace(),
				}, freshSnapshot)).To(Succeed())
				snapshotLike, extractErr := snapshot.ExtractSnapshotLike(freshSnapshot)
				g.Expect(extractErr).NotTo(HaveOccurred())
				readyCond := snapshot.GetCondition(snapshotLike, snapshot.ConditionReady)
				g.Expect(readyCond).NotTo(BeNil(), "Ready condition should exist")
				g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue), "Ready should be True")
				g.Expect(readyCond.Reason).To(Equal(snapshot.ReasonCompleted), "Reason mirrors the bound SnapshotContent Ready reason")
			}, "30s", "200ms").Should(Succeed(), "Snapshot should mirror bound SnapshotContent Ready=True")

			// Verify the bound SnapshotContent is Ready=True (source of truth).
			finalContent := &storagev1alpha1.SnapshotContent{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: contentName}, finalContent)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(finalContent.Status.Conditions, snapshot.ConditionReady)).To(BeTrue(), "SnapshotContent should be Ready=True")
		})
	})
})
