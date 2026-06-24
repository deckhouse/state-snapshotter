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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// registerGenericMirrorBinderOnce installs a manager-driven generic binder for the isolated
// RegistrationTestSnapshot GVK exactly once for the suite. RegistrationTestSnapshot is used because no other
// integration spec drives a manager-registered binder on it (controller_registration_test only constructs
// controllers), so this global watch cannot interfere with the direct-reconcile TestSnapshot specs.
var registerGenericMirrorBinderOnce sync.Once

// Phase 2a user-facing convergence: a generic XxxxSnapshot exposes readiness via Snapshot.Ready, which must
// be a verbatim mirror of the bound SnapshotContent.Ready in BOTH directions and converge event-driven (no
// manual reconcile, no polling). The generic binder has no reverse Snapshot ref on the content, so it relies
// on the SnapshotContent -> owning Snapshot watch (mapBoundContentToSnapshots, by status.boundSnapshotContentName).
// Readiness of the bound common SnapshotContent is driven only through its own controller via a Ready
// ManifestCheckpoint that is flipped Ready=True -> Failed -> True (the MCP watch wakes the content controller).
var _ = Describe("Integration: generic Snapshot mirrors bound SnapshotContent Ready in both directions", Serial, func() {
	var (
		ctx         context.Context
		snapshotGVK schema.GroupVersionKind
		commonGVK   schema.GroupVersionKind
	)

	BeforeEach(func() {
		ctx = context.Background()
		snapshotGVK = schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "RegistrationTestSnapshot"}
		commonGVK = unifiedbootstrap.CommonSnapshotContentGVK()

		registerGenericMirrorBinderOnce.Do(func() {
			binder, err := controllers.NewGenericSnapshotBinderController(
				mgr.GetClient(), mgr.GetAPIReader(), scheme, testCfg, nil,
			)
			Expect(err).NotTo(HaveOccurred())
			// Map the isolated generic snapshot to the COMMON SnapshotContent so the manager's common
			// SnapshotContentController owns its Ready, and register both the For(snapshot) watch and the
			// new SnapshotContent -> Snapshot reverse wake-up watch.
			Expect(binder.AddWatchForPair(mgr, snapshotGVK, commonGVK)).To(Succeed())
		})
	})

	It("converges Snapshot.Ready True->False->True driven only by SnapshotContent (watch wake-up, no manual reconcile)", func() {
		snap := &unstructured.Unstructured{}
		snap.SetGroupVersionKind(snapshotGVK)
		snap.SetName("gen-mirror-wakeup")
		snap.SetNamespace("default")
		snap.Object["spec"] = map[string]interface{}{}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, snap))
		})

		// Simulate the domain controller publishing ChildrenSnapshotReady=True for the current generation so the
		// generic binder barrier passes (RegistrationTestSnapshot has no real domain controller).
		setSnapshotChildrenSnapshotReadyCurrent(ctx, snap)

		snapKey := types.NamespacedName{Namespace: "default", Name: "gen-mirror-wakeup"}

		// The manager-driven binder creates and binds the common SnapshotContent on its own.
		var contentName string
		Eventually(func(g Gomega) {
			fresh := &unstructured.Unstructured{}
			fresh.SetGroupVersionKind(snapshotGVK)
			g.Expect(k8sClient.Get(ctx, snapKey, fresh)).To(Succeed())
			bound, _, _ := unstructured.NestedString(fresh.Object, "status", "boundSnapshotContentName")
			g.Expect(bound).NotTo(BeEmpty())
			contentName = bound
		}, 90*time.Second, 200*time.Millisecond).Should(Succeed())

		var contentUID string
		Eventually(func(g Gomega) {
			c := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, c)).To(Succeed())
			contentUID = string(c.UID)
			g.Expect(contentUID).NotTo(BeEmpty())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: contentName}}))
		})

		// Publish the truth ref (status.manifestCheckpointName) once. The generic binder leaves it untouched
		// while status.manifestCaptureRequestName is empty, so the link is stable across the flips below.
		mcpName := "mcp-gen-mirror-wakeup"
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			c := &storagev1alpha1.SnapshotContent{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, c); err != nil {
				return err
			}
			c.Status.ManifestCheckpointName = mcpName
			return k8sClient.Status().Update(ctx, c)
		})).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: mcpName}}))
		})

		snapshotReadyStatusIs := func(want metav1.ConditionStatus) func(Gomega) {
			return func(g Gomega) {
				fresh := &unstructured.Unstructured{}
				fresh.SetGroupVersionKind(snapshotGVK)
				g.Expect(k8sClient.Get(ctx, snapKey, fresh)).To(Succeed())
				like, err := snapshot.ExtractSnapshotLike(fresh)
				g.Expect(err).NotTo(HaveOccurred())
				ready := snapshot.GetCondition(like, snapshot.ConditionReady)
				g.Expect(ready).NotTo(BeNil())
				g.Expect(ready.Status).To(Equal(want))
			}
		}

		flipMCP := func(status metav1.ConditionStatus, reason, message string) {
			ensureManifestCheckpointStatus(ctx, mcpName, contentName, contentUID, func(m *ssv1alpha1.ManifestCheckpoint) {
				meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
					Type:    ssv1alpha1.ManifestCheckpointConditionTypeReady,
					Status:  status,
					Reason:  reason,
					Message: message,
				})
			})
		}

		// Phase 1: content becomes Ready=True -> Snapshot.Ready mirrors True.
		flipMCP(metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "checkpoint ready")
		Eventually(snapshotReadyStatusIs(metav1.ConditionTrue), 90*time.Second, 200*time.Millisecond).
			Should(Succeed(), "Snapshot.Ready must mirror content Ready=True")

		// Phase 2: content degrades to Ready=False (terminal MCP failure) -> Snapshot.Ready mirrors False
		// purely via the SnapshotContent -> Snapshot watch (no manual snapshot reconcile).
		flipMCP(metav1.ConditionFalse, ssv1alpha1.ManifestCheckpointConditionReasonFailed, "checkpoint corrupted")
		Eventually(snapshotReadyStatusIs(metav1.ConditionFalse), 90*time.Second, 200*time.Millisecond).
			Should(Succeed(), "Snapshot.Ready must fall to False after the bound content degrades")

		// Phase 3: content recovers to Ready=True -> Snapshot.Ready mirrors True again.
		flipMCP(metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "checkpoint recovered")
		Eventually(snapshotReadyStatusIs(metav1.ConditionTrue), 90*time.Second, 200*time.Millisecond).
			Should(Succeed(), "Snapshot.Ready must rise back to True after the bound content recovers")
	})
})
