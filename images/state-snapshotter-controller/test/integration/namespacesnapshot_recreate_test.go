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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// Must match NamespaceSnapshot controller label on ManifestCaptureRequest (internal/controllers/namespacesnapshot_capture.go).
const labelNamespaceSnapshotUID = "state-snapshotter.deckhouse.io/namespace-snapshot-uid"

var _ = Describe("Integration: NamespaceSnapshot recreate (stale MCR / §4.7)", func() {
	It("after deleting root and creating another with the same name, binds a new NSC and MCR by new UID and reaches Ready", func() {
		ctx := context.Background()
		contentName1 := ""
		contentName2 := ""

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-recreate-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "namespacesnapshot-recreate",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			if contentName2 != "" {
				_ = k8sClient.Delete(ctx, &storagev1alpha1.NamespaceSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: contentName2}})
			}
			if contentName1 != "" {
				_ = k8sClient.Delete(ctx, &storagev1alpha1.NamespaceSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: contentName1}})
			}
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-recreate-cm", Namespace: nsName},
			Data:       map[string]string{"k": "v"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		snapName := "snap"
		key := types.NamespacedName{Namespace: nsName, Name: snapName}

		snap1 := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snapName, Namespace: nsName},
			Spec:       storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap1)).To(Succeed())

		var uid1 types.UID
		var mcrKey1 client.ObjectKey
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			uid1 = fresh.UID
			contentName1 = fresh.Status.BoundSnapshotContentName
			mcrKey1 = client.ObjectKey{Namespace: nsName, Name: namespacemanifest.NamespaceSnapshotMCRName(uid1)}
			mcr := &ssv1alpha1.ManifestCaptureRequest{}
			g.Expect(k8sClient.Get(ctx, mcrKey1, mcr)).To(Succeed())
			g.Expect(mcr.Labels[labelNamespaceSnapshotUID]).To(Equal(string(uid1)))
			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		}).WithTimeout(90 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		Expect(k8sClient.Delete(ctx, &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snapName, Namespace: nsName},
		})).To(Succeed())

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, key, &storagev1alpha1.NamespaceSnapshot{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}).WithTimeout(90 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		// MCR for the deleted snapshot is not tied to NamespaceSnapshot deletion (retained / separate lifecycle);
		// the old MCR name is keyed by uid1 and must not block a new snapshot with the same metadata.name.
		Expect(k8sClient.Get(ctx, mcrKey1, &ssv1alpha1.ManifestCaptureRequest{})).To(Succeed())

		snap2 := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snapName, Namespace: nsName},
			Spec:       storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap2)).To(Succeed())

		var uid2 types.UID
		var mcrKey2 client.ObjectKey
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.UID).NotTo(Equal(uid1))
			uid2 = fresh.UID
			g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(Equal(contentName1))
			contentName2 = fresh.Status.BoundSnapshotContentName
			mcrKey2 = client.ObjectKey{Namespace: nsName, Name: namespacemanifest.NamespaceSnapshotMCRName(uid2)}
			g.Expect(mcrKey2).NotTo(Equal(mcrKey1))
			mcr := &ssv1alpha1.ManifestCaptureRequest{}
			g.Expect(k8sClient.Get(ctx, mcrKey2, mcr)).To(Succeed())
			g.Expect(mcr.Labels[labelNamespaceSnapshotUID]).To(Equal(string(uid2)))
			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		}).WithTimeout(90 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		sc := &storagev1alpha1.NamespaceSnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName1}, sc)).To(Succeed())
		Expect(sc.Spec.NamespaceSnapshotRef.UID).To(Equal(uid1))
		sc2 := &storagev1alpha1.NamespaceSnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName2}, sc2)).To(Succeed())
		Expect(sc2.Spec.NamespaceSnapshotRef.UID).To(Equal(uid2))
	})
})
