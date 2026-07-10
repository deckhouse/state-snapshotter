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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// Ready Contract on the current condition model: SnapshotContent owns Ready, derived from
// ManifestsReady (its own ManifestCheckpoint) AND DataReady (its own data refs) AND ChildrenReady (direct child SnapshotContents).
// The controller only manages the common SnapshotContent GVK, so these specs operate on the common type and
// drive readiness through a real Ready ManifestCheckpoint (never force-writing the content Ready condition).
var _ = Describe("Integration: SnapshotContentController - Ready Contract", Serial, func() {
	var (
		ctx         context.Context
		contentCtrl *controllers.SnapshotContentController
	)

	commonGVK := unifiedbootstrap.CommonSnapshotContentGVK()

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		contentCtrl, err = controllers.NewSnapshotContentController(
			k8sClient,
			mgr.GetAPIReader(),
			scheme,
			mgr.GetRESTMapper(),
			testCfg,
			[]schema.GroupVersionKind{commonGVK},
		)
		Expect(err).NotTo(HaveOccurred())
	})

	createContent := func(generateName string) (string, string) {
		c := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{GenerateName: generateName},
			Spec:       retainContentSpec(),
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		name := c.Name
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: name}}))
		})
		var uid string
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, fresh)).To(Succeed())
			uid = string(fresh.UID)
			g.Expect(uid).NotTo(BeEmpty())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		// retainContentSpec's spec.snapshotRef is a core Snapshot, so every content here is a namespace-root
		// content subject to the monotonic, lowest-priority subtreeManifestsPersisted gate that only the
		// (absent) snapshot reconciler latches. Seed it to its satisfied value so readiness is driven purely
		// by the ManifestsReady/ChildrenReady legs under test; without it the gate pins Ready=False forever.
		// (The orphan-link ChildrenReady gate is vacuously open here: the owning Snapshot is never created, so
		// it declares no orphan VolumeSnapshot leaves — the former residualVolumeCapture seed is gone.)
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			c := &storagev1alpha1.SnapshotContent{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, c); err != nil {
				return err
			}
			c.Status.SubtreeManifestsPersisted = true
			return k8sClient.Status().Update(ctx, c)
		})).To(Succeed())
		return name, uid
	}

	readyMCP := func(mcpName, contentName, contentUID string) {
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
	}

	reconcile := func(name string) {
		_, _ = contentCtrl.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	}

	It("should NOT set Ready=True when manifestCheckpointName is empty", func() {
		name, _ := createContent("ready-contract-no-mcp-")

		// Without a published manifestCheckpointName the content can never be ManifestsReady, so the controller
		// must keep Ready=False. The controller owns Ready; the test only observes it.
		Consistently(func(g Gomega) {
			reconcile(name)
			c := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, c)).To(Succeed())
			g.Expect(meta.IsStatusConditionTrue(c.Status.Conditions, snapshot.ConditionReady)).
				To(BeFalse(), "Ready must stay False without manifestCheckpointName")
		}, 3*time.Second, 200*time.Millisecond).Should(Succeed())
	})

	It("should require children to be Ready before setting Ready=True", func() {
		// Child content without a checkpoint yet -> stays not Ready.
		childName, childUID := createContent("rc-child-")

		// Parent content: own Ready ManifestCheckpoint + a direct child ref.
		parentName, parentUID := createContent("rc-parent-")
		parentMCP := "mcp-" + parentName
		readyMCP(parentMCP, parentName, parentUID)

		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			c := &storagev1alpha1.SnapshotContent{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: parentName}, c); err != nil {
				return err
			}
			c.Status.ManifestCheckpointName = parentMCP
			c.Status.ChildrenSnapshotContentRefs = []storagev1alpha1.SnapshotContentChildRef{{Name: childName}}
			return k8sClient.Status().Update(ctx, c)
		})).To(Succeed())

		// ChildrenReady gate: parent must stay Ready=False while the child is not Ready, even though the
		// parent's own ManifestsReady/DataReady are satisfied.
		Consistently(func(g Gomega) {
			reconcile(childName)
			reconcile(parentName)
			c := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: parentName}, c)).To(Succeed())
			ready := meta.FindStatusCondition(c.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse), "parent must stay not Ready while child is not Ready")
		}, 3*time.Second, 200*time.Millisecond).Should(Succeed())

		// Drive the child genuinely Ready via its own Ready ManifestCheckpoint (controller owns Ready).
		childMCP := "mcp-" + childName
		readyMCP(childMCP, childName, childUID)
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			c := &storagev1alpha1.SnapshotContent{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: childName}, c); err != nil {
				return err
			}
			c.Status.ManifestCheckpointName = childMCP
			return k8sClient.Status().Update(ctx, c)
		})).To(Succeed())

		Eventually(func(g Gomega) {
			reconcile(childName)
			c := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: childName}, c)).To(Succeed())
			g.Expect(meta.IsStatusConditionTrue(c.Status.Conditions, snapshot.ConditionReady)).To(BeTrue())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed(), "child should become Ready=True")

		// With the child Ready, the parent's only remaining gate (ChildrenReady) is satisfied -> Ready=True.
		Eventually(func(g Gomega) {
			reconcile(childName)
			reconcile(parentName)
			c := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: parentName}, c)).To(Succeed())
			g.Expect(meta.IsStatusConditionTrue(c.Status.Conditions, snapshot.ConditionReady)).To(BeTrue())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed(), "parent must become Ready after child is Ready")
	})
})
