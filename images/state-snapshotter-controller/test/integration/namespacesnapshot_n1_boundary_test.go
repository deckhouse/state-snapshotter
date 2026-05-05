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
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

var _ = Describe("Integration: NamespaceSnapshot N1 boundary (mismatch + recovery)", func() {
	It("sets ContentRefMismatch when SnapshotContent namespaceSnapshotRef does not match root", func() {
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-mismatch-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "namespacesnapshot-mismatch",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		snap := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "snap",
				Namespace: nsName,
			},
			Spec: storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		key := types.NamespacedName{Namespace: nsName, Name: snap.Name}
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, key, snap)).To(Succeed())
			g.Expect(snap.UID).NotTo(BeEmpty())
		}).WithTimeout(15 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

		contentName := fmt.Sprintf("ns-%s", strings.ReplaceAll(string(snap.UID), "-", ""))
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: contentName}})
			_ = k8sClient.Delete(ctx, &storagev1alpha1.NamespaceSnapshot{ObjectMeta: metav1.ObjectMeta{Name: snap.Name, Namespace: nsName}})
		})

		badRef := storagev1alpha1.SnapshotSubjectRef{
			APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
			Kind:       "NamespaceSnapshot",
			Name:       snap.Name,
			Namespace:  nsName,
			UID:        types.UID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"),
		}
		badContent := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: contentName},
			Spec: storagev1alpha1.SnapshotContentSpec{
				SnapshotRef:    badRef,
				DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
			},
		}
		// The reconciler may create SnapshotContent before this Create runs (N2a adds more work per loop).
		// Accept AlreadyExists and patch spec to the mismatched ref so the scenario stays deterministic.
		if err := k8sClient.Create(ctx, badContent); err != nil {
			Expect(apierrors.IsAlreadyExists(err)).To(BeTrue(), "unexpected error creating SnapshotContent: %v", err)
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				existing := &storagev1alpha1.SnapshotContent{}
				if err := k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, existing); err != nil {
					return err
				}
				existing.Spec.SnapshotRef = badRef
				return k8sClient.Update(ctx, existing)
			})).To(Succeed())
		}

		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal("ContentRefMismatch"))
			bound := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionBound)
			g.Expect(bound).NotTo(BeNil())
			g.Expect(bound.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(bound.Reason).To(Equal("ContentRefMismatch"))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})

	It("recovers Bound and Ready after status is cleared while SnapshotContent still matches", func() {
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-recover-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "namespacesnapshot-recovery",
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

		snap := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "snap",
				Namespace: nsName,
			},
			Spec: storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		key := types.NamespacedName{Namespace: nsName, Name: snap.Name}

		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			contentName = fresh.Status.BoundSnapshotContentName
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		fresh := &storagev1alpha1.NamespaceSnapshot{}
		Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
		fresh.Status.BoundSnapshotContentName = ""
		fresh.Status.Conditions = nil
		fresh.Status.ObservedGeneration = 0
		Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())

		Eventually(func(g Gomega) {
			again := &storagev1alpha1.NamespaceSnapshot{}
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
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, content)).To(Succeed())
			contentReady := meta.FindStatusCondition(content.Status.Conditions, snapshot.ConditionReady)
			g.Expect(contentReady).NotTo(BeNil())
			g.Expect(contentReady.Status).To(Equal(metav1.ConditionTrue))
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		Consistently(func(g Gomega) {
			stable := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, key, stable)).To(Succeed())
			g.Expect(stable.Status.BoundSnapshotContentName).To(Equal(contentName))
			ready := meta.FindStatusCondition(stable.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		}).WithTimeout(2 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())
	})
})
