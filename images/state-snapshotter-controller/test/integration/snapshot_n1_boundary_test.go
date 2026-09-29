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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// N1 boundary recovery: a Snapshot whose status was damaged converges back to Bound + Ready=True against
// the SnapshotContent it is still bound to.
//
// What "damaged" can mean here got narrower once the binding became set-once: the apiserver now refuses to
// clear status.boundSnapshotContentName at all (asserted below, and from the admission side in the set-once
// spec), so the binding cannot be lost in the first place. The damage that remains possible is a writer
// that drops the conditions, and the recovery this spec pins is that Ready is recomputed from the bound
// content rather than left stale.
var _ = Describe("Integration: Snapshot N1 boundary (recovery)", func() {
	It("keeps its binding through a status wipe and recomputes Ready", func() {
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-recover-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "snapshot-recovery",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		contentName := ""
		DeferCleanup(func() {
			cctx := context.Background()
			if contentName != "" {
				_ = k8sClient.Delete(cctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: contentName}})
			}
			_ = k8sClient.Delete(cctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-recover-cm", Namespace: nsName},
			Data:       map[string]string{"k": "v"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "snap",
				Namespace: nsName,
			},
			Spec: storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		key := types.NamespacedName{Namespace: nsName, Name: snap.Name}

		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			contentName = fresh.Status.BoundSnapshotContentName
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		// The wholesale wipe a damaged or foreign writer would attempt: refused, because the binding is
		// set-once and its companion rule forbids removal.
		wiped := &storagev1alpha1.Snapshot{}
		Expect(k8sClient.Get(ctx, key, wiped)).To(Succeed())
		wiped.Status.BoundSnapshotContentName = ""
		wiped.Status.Conditions = nil
		Expect(apierrors.IsInvalid(k8sClient.Status().Update(ctx, wiped))).To(BeTrue(),
			"clearing a published binding must be refused by admission")

		// What such a writer CAN still do: drop the conditions and keep the binding. That is the damage the
		// recovery below is about.
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			fresh.Status.Conditions = nil
			g.Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())
		}).WithTimeout(30 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			again := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, key, again)).To(Succeed())
			g.Expect(again.Status.BoundSnapshotContentName).To(Equal(contentName))
			ready := meta.FindStatusCondition(again.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			content := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: contentName}, content)).To(Succeed())
			contentReady := meta.FindStatusCondition(content.Status.Conditions, snapshot.ConditionReady)
			g.Expect(contentReady).NotTo(BeNil())
			g.Expect(contentReady.Status).To(Equal(metav1.ConditionTrue))
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		// Status repair may race with stale reconcile events produced by the manual
		// status wipe above. Assert convergence again instead of requiring an
		// immediate no-transient window in the shared integration manager.
		Eventually(func(g Gomega) {
			stable := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, key, stable)).To(Succeed())
			g.Expect(stable.Status.BoundSnapshotContentName).To(Equal(contentName))
			ready := meta.FindStatusCondition(stable.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		}).WithTimeout(10 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())
	})
})
