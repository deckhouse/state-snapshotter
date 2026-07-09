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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// Regression: root Snapshot must mirror SnapshotContent Ready after status-only content updates
// (SnapshotContent watch -> Snapshot reconcile). Without the watch, a stale Snapshot Ready=False
// can remain after bound content becomes Ready.
// Label("isolated"): this manager-driven Ready-propagation spec relies on a status-only SnapshotContent
// update waking the bound Snapshot via watch; under the shared-manager parallel pass it flakes on
// enqueue latency, so it runs serially in its own isolated pass (see Makefile test-integration).
var _ = Describe("Integration: SnapshotContent Ready propagates to bound Snapshot", Serial, Label("isolated"), func() {
	It("mirrors Ready=True when only SnapshotContent status changes", func() {
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-content-ready-prop-",
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "root-prop", Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		snapKey := types.NamespacedName{Namespace: nsName, Name: snap.Name}

		var contentName string
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, snapKey, fresh)).To(Succeed())
			g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			contentName = fresh.Status.BoundSnapshotContentName
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		contentKey := client.ObjectKey{Name: contentName}
		mcpName := "mcp-prop-" + strings.TrimPrefix(contentName, "ns-")

		// Content not ready yet (no MCP).
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			c := &storagev1alpha1.SnapshotContent{}
			if err := k8sClient.Get(ctx, contentKey, c); err != nil {
				return err
			}
			c.Status.ManifestCheckpointName = ""
			meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
				Type:               snapshot.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             snapshot.ReasonManifestCapturePending,
				Message:            "waiting for SnapshotContent.status.manifestCheckpointName",
				ObservedGeneration: c.Generation,
			})
			return k8sClient.Status().Update(ctx, c)
		})).To(Succeed())

		// Simulate stuck mirror from run 20260525-134758 (content later becomes Ready without Snapshot update).
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			s := &storagev1alpha1.Snapshot{}
			if err := k8sClient.Get(ctx, snapKey, s); err != nil {
				return err
			}
			meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
				Type:               snapshot.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             snapshot.ReasonManifestCapturePending,
				Message:            "waiting for SnapshotContent.status.manifestCheckpointName",
				ObservedGeneration: s.Generation,
			})
			return k8sClient.Status().Update(ctx, s)
		})).To(Succeed())

		// Status-only SnapshotContent update — must enqueue bound Snapshot (no Snapshot spec/metadata change).
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			c := &storagev1alpha1.SnapshotContent{}
			if err := k8sClient.Get(ctx, contentKey, c); err != nil {
				return err
			}
			c.Status.ManifestCheckpointName = mcpName
			meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
				Type:               snapshot.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             snapshot.ReasonCompleted,
				Message:            "manifest, data, and child content are ready",
				ObservedGeneration: c.Generation,
			})
			return k8sClient.Status().Update(ctx, c)
		})).To(Succeed())

		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, snapKey, fresh)).To(Succeed())
			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal(snapshot.ReasonCompleted))
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})
