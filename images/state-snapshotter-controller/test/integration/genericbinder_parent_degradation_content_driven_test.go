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

// A2 (Slice 3) content-driven parent degradation, the replacement for the recursive
// propagateReadyFalseToParent snapshot-patching path.
//
// Contract under test: after bind/publish a parent generic Snapshot.Ready is a verbatim mirror of its
// bound SnapshotContent.Ready, and a child's durable degradation propagates UP the tree through the
// SnapshotContent aggregation only (INV-COND2/INV-COND4/INV-FAIL1):
//
//	child SnapshotContent Ready=False
//	  -> parent SnapshotContent ChildrenReady=False
//	  -> parent SnapshotContent Ready=False
//	  -> parent Snapshot.Ready mirror=False
//
// The propagation here is fully event-driven (artifact -> content -> parent content -> snapshot watches);
// there are NO manual reconcile calls. The recursive genericbinder propagateReadyFalseToParent only ever
// fired on child-Snapshot OBJECT deletion (a transient view), never on durable artifact/content loss, so it
// is inert in this scenario: this spec stays green with or without that code, which is exactly the proof
// required before removing it. Note that deleting the child Snapshot object alone is intentionally NOT a
// parent-degradation signal in the durable-content model (the child SnapshotContent survives via
// ObjectKeeper TTL); the real degradation signal is durable artifact/content loss, exercised below.
var _ = Describe("Integration: parent generic Snapshot degrades via SnapshotContent ChildrenReady (content-driven)", Serial, func() {
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
			Expect(binder.AddWatchForPair(mgr, snapshotGVK, commonGVK)).To(Succeed())
		})
	})

	It("flips parent Snapshot.Ready False/True via child SnapshotContent ChildrenReady + mirror (no recursive patch, no manual reconcile)", func() {
		// 1. Parent generic snapshot -> manager-driven binder creates and binds its parent common content.
		parent := &unstructured.Unstructured{}
		parent.SetGroupVersionKind(snapshotGVK)
		parent.SetName("gen-parent-degrade")
		parent.SetNamespace("default")
		parent.Object["spec"] = map[string]interface{}{"backupClassName": "test-backup-class"}
		Expect(k8sClient.Create(ctx, parent)).To(Succeed())
		DeferCleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(ctx, parent)) })
		setSnapshotDomainReadyCurrent(ctx, parent)

		parentKey := types.NamespacedName{Namespace: "default", Name: "gen-parent-degrade"}
		var parentContentName, parentContentUID string
		Eventually(func(g Gomega) {
			fresh := &unstructured.Unstructured{}
			fresh.SetGroupVersionKind(snapshotGVK)
			g.Expect(k8sClient.Get(ctx, parentKey, fresh)).To(Succeed())
			bound, _, _ := unstructured.NestedString(fresh.Object, "status", "boundSnapshotContentName")
			g.Expect(bound).NotTo(BeEmpty())
			parentContentName = bound
		}, 90*time.Second, 200*time.Millisecond).Should(Succeed())
		Eventually(func(g Gomega) {
			c := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: parentContentName}, c)).To(Succeed())
			parentContentUID = string(c.UID)
			g.Expect(parentContentUID).NotTo(BeEmpty())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: parentContentName}}))
		})

		// 2. Child common SnapshotContent owned by the parent content. The SnapshotContent ownerRef is the
		// wake-up route (mapSnapshotContentToParentContent) used by the manager's content controller; it is
		// set non-controller so it does not collide with the parent's own controller ownerRef.
		childContentName := parentContentName + "-child"
		child := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{
				Name: childContentName,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
					Kind:       "SnapshotContent",
					Name:       parentContentName,
					UID:        types.UID(parentContentUID),
				}},
			},
			Spec: storagev1alpha1.SnapshotContentSpec{DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain},
		}
		Expect(k8sClient.Create(ctx, child)).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: childContentName}}))
		})
		var childContentUID string
		Eventually(func(g Gomega) {
			c := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: childContentName}, c)).To(Succeed())
			childContentUID = string(c.UID)
			g.Expect(childContentUID).NotTo(BeEmpty())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		// 3. Truth refs: parent content references the child (childrenSnapshotContentRefs) and its own MCP;
		// child content references its own MCP. The generic binder leaves manifestCheckpointName untouched
		// while manifestCaptureRequestName is empty, so these links are stable across the flips below.
		parentMCP := "mcp-parent-degrade"
		childMCP := "mcp-child-degrade"
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			c := &storagev1alpha1.SnapshotContent{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: parentContentName}, c); err != nil {
				return err
			}
			c.Status.ManifestCheckpointName = parentMCP
			c.Status.ChildrenSnapshotContentRefs = []storagev1alpha1.SnapshotContentChildRef{{Name: childContentName}}
			return k8sClient.Status().Update(ctx, c)
		})).To(Succeed())
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			c := &storagev1alpha1.SnapshotContent{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: childContentName}, c); err != nil {
				return err
			}
			c.Status.ManifestCheckpointName = childMCP
			return k8sClient.Status().Update(ctx, c)
		})).To(Succeed())

		flipMCP := func(mcpName, contentName, contentUID string, status metav1.ConditionStatus, reason, message string) {
			ensureManifestCheckpointStatus(ctx, mcpName, contentName, contentUID, func(m *ssv1alpha1.ManifestCheckpoint) {
				meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
					Type:    ssv1alpha1.ManifestCheckpointConditionTypeReady,
					Status:  status,
					Reason:  reason,
					Message: message,
				})
			})
			DeferCleanup(func() {
				_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: mcpName}}))
			})
		}

		parentSnapshotReadyIs := func(want metav1.ConditionStatus) func(Gomega) {
			return func(g Gomega) {
				fresh := &unstructured.Unstructured{}
				fresh.SetGroupVersionKind(snapshotGVK)
				g.Expect(k8sClient.Get(ctx, parentKey, fresh)).To(Succeed())
				like, err := snapshot.ExtractSnapshotLike(fresh)
				g.Expect(err).NotTo(HaveOccurred())
				ready := snapshot.GetCondition(like, snapshot.ConditionReady)
				g.Expect(ready).NotTo(BeNil())
				g.Expect(ready.Status).To(Equal(want))
			}
		}
		parentContentConditionIs := func(condType string, want metav1.ConditionStatus) func(Gomega) {
			return func(g Gomega) {
				c := &storagev1alpha1.SnapshotContent{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: parentContentName}, c)).To(Succeed())
				cond := meta.FindStatusCondition(c.Status.Conditions, condType)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(want))
			}
		}

		// 4. Both contents Ready=True -> parent content Ready=True -> parent Snapshot.Ready mirrors True.
		flipMCP(parentMCP, parentContentName, parentContentUID, metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "checkpoint ready")
		flipMCP(childMCP, childContentName, childContentUID, metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "checkpoint ready")
		Eventually(parentContentConditionIs(snapshot.ConditionChildrenReady, metav1.ConditionTrue), 90*time.Second, 200*time.Millisecond).Should(Succeed())
		Eventually(parentContentConditionIs(snapshot.ConditionReady, metav1.ConditionTrue), 90*time.Second, 200*time.Millisecond).Should(Succeed())
		Eventually(parentSnapshotReadyIs(metav1.ConditionTrue), 90*time.Second, 200*time.Millisecond).
			Should(Succeed(), "parent Snapshot.Ready must mirror content Ready=True")

		// 5. Degrade the child's durable artifact (terminal MCP failure). No manual reconcile.
		flipMCP(childMCP, childContentName, childContentUID, metav1.ConditionFalse, ssv1alpha1.ManifestCheckpointConditionReasonFailed, "checkpoint corrupted")

		// 6. Content-driven convergence: child Ready=False -> parent ChildrenReady=False -> parent Ready=False
		// -> parent Snapshot.Ready mirror=False, all event-driven through the SnapshotContent tree.
		Eventually(parentContentConditionIs(snapshot.ConditionChildrenReady, metav1.ConditionFalse), 90*time.Second, 200*time.Millisecond).
			Should(Succeed(), "parent SnapshotContent.ChildrenReady must fall to False after the child content degrades")
		Eventually(parentContentConditionIs(snapshot.ConditionReady, metav1.ConditionFalse), 90*time.Second, 200*time.Millisecond).
			Should(Succeed(), "parent SnapshotContent.Ready must fall to False (Ready = RequestsReady && ChildrenReady)")
		Eventually(parentSnapshotReadyIs(metav1.ConditionFalse), 90*time.Second, 200*time.Millisecond).
			Should(Succeed(), "parent Snapshot.Ready must fall to False via SnapshotContent ChildrenReady + mirror, NOT via recursive snapshot patching")

		// 7. Recovery: child artifact restored -> parent Snapshot.Ready rises back to True (both directions).
		flipMCP(childMCP, childContentName, childContentUID, metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "checkpoint recovered")
		Eventually(parentSnapshotReadyIs(metav1.ConditionTrue), 90*time.Second, 200*time.Millisecond).
			Should(Succeed(), "parent Snapshot.Ready must rise back to True after the child content recovers")
	})
})
