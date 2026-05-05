//go:build integration
// +build integration

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

package integration

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

var _ = Describe("Integration: Snapshot N1 boundary (recovery)", func() {
	It("recovers Bound and Ready after status is cleared while SnapshotContent still matches", func() {
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

		fresh := &storagev1alpha1.Snapshot{}
		Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
		fresh.Status.BoundSnapshotContentName = ""
		fresh.Status.Conditions = nil
		fresh.Status.ObservedGeneration = 0
		Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())

		Eventually(func(g Gomega) {
			again := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, key, again)).To(Succeed())
			g.Expect(again.Status.BoundSnapshotContentName).To(Equal(contentName))
			bound := meta.FindStatusCondition(again.Status.Conditions, snapshot.ConditionBound)
			g.Expect(bound).NotTo(BeNil())
			g.Expect(bound.Status).To(Equal(metav1.ConditionTrue))
			graphReady := meta.FindStatusCondition(again.Status.Conditions, snapshot.ConditionGraphReady)
			g.Expect(graphReady).NotTo(BeNil())
			g.Expect(graphReady.Status).To(Equal(metav1.ConditionTrue))
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
