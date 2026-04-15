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

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
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

func ownerRefToNamespaceSnapshotContent(refs []metav1.OwnerReference, name string, uid types.UID) bool {
	for i := range refs {
		ref := refs[i]
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "NamespaceSnapshotContent" &&
			ref.Name == name && ref.UID == uid {
			return true
		}
	}
	return false
}

var _ = Describe("Integration: NamespaceSnapshot deletion semantics", func() {
	It("Retain: deleting NamespaceSnapshot removes root finalizer but keeps NamespaceSnapshotContent", func() {
		ctx := context.Background()
		contentName := ""

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-del-retain-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "namespacesnapshot-deletion-retain",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			if contentName != "" {
				_ = k8sClient.Delete(ctx, &storagev1alpha1.NamespaceSnapshotContent{
					ObjectMeta: metav1.ObjectMeta{Name: contentName},
				})
			}
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-del-retain-cm", Namespace: nsName},
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
			sc := &storagev1alpha1.NamespaceSnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, sc)).To(Succeed())
			g.Expect(sc.Spec.DeletionPolicy).To(Equal(storagev1alpha1.SnapshotContentDeletionPolicyRetain))
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		Expect(k8sClient.Delete(ctx, &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snap.Name, Namespace: nsName},
		})).To(Succeed())

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, key, &storagev1alpha1.NamespaceSnapshot{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		sc := &storagev1alpha1.NamespaceSnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, sc)).To(Succeed())
	})

	It("Delete: root finalizer clears only after NamespaceSnapshotContent is gone", func() {
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-del-delete-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "namespacesnapshot-deletion-delete",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-del-delete-cm", Namespace: nsName},
			Data:       map[string]string{"k": "v"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		snap := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "snap-del",
				Namespace: nsName,
			},
			Spec: storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		key := types.NamespacedName{Namespace: nsName, Name: snap.Name}
		var contentName string
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			contentName = fresh.Status.BoundSnapshotContentName
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		sc := &storagev1alpha1.NamespaceSnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, sc)).To(Succeed())
		base := sc.DeepCopy()
		sc.Spec.DeletionPolicy = storagev1alpha1.SnapshotContentDeletionPolicyDelete
		Expect(k8sClient.Patch(ctx, sc, client.MergeFrom(base))).To(Succeed())

		Expect(k8sClient.Delete(ctx, &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snap.Name, Namespace: nsName},
		})).To(Succeed())

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, &storagev1alpha1.NamespaceSnapshotContent{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, key, &storagev1alpha1.NamespaceSnapshot{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())
	})

	It("unified retention: deleting NamespaceSnapshot does not remove MCR or ManifestCheckpoint (Retain content)", func() {
		ctx := context.Background()
		contentName := ""

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-del-retain-mcp-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "namespacesnapshot-retain-mcp",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			if contentName != "" {
				_ = k8sClient.Delete(ctx, &storagev1alpha1.NamespaceSnapshotContent{
					ObjectMeta: metav1.ObjectMeta{Name: contentName},
				})
			}
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-del-retain-mcp-cm", Namespace: nsName},
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

		var rootUID types.UID
		var mcrKey client.ObjectKey
		var mcpName string
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			rootUID = fresh.UID
			contentName = fresh.Status.BoundSnapshotContentName
			mcrKey = client.ObjectKey{Namespace: nsName, Name: namespacemanifest.NamespaceSnapshotMCRName(rootUID)}
			g.Expect(k8sClient.Get(ctx, mcrKey, &ssv1alpha1.ManifestCaptureRequest{})).To(Succeed())
			mcr := &ssv1alpha1.ManifestCaptureRequest{}
			g.Expect(k8sClient.Get(ctx, mcrKey, mcr)).To(Succeed())
			mcpName = namespacemanifest.GenerateManifestCheckpointNameFromUID(mcr.UID)
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: mcpName}, &ssv1alpha1.ManifestCheckpoint{})).To(Succeed())
		}).WithTimeout(90 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

		nsc := &storagev1alpha1.NamespaceSnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, nsc)).To(Succeed())
		ok := &deckhousev1alpha1.ObjectKeeper{}
		okName := namespacemanifest.NamespaceSnapshotRootObjectKeeperName(nsName, snap.Name)
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: okName}, ok)).To(Succeed())
		Expect(ok.Spec.FollowObjectRef).NotTo(BeNil())
		Expect(ok.Spec.FollowObjectRef.Kind).To(Equal("NamespaceSnapshot"))
		Expect(ok.Spec.FollowObjectRef.Name).To(Equal(snap.Name))
		Expect(ok.Spec.FollowObjectRef.Namespace).To(Equal(nsName))
		Expect(ok.Spec.FollowObjectRef.UID).To(Equal(string(rootUID)))
		Expect(ownerRefToNamespaceSnapshotContent(ok.OwnerReferences, contentName, nsc.UID)).To(BeTrue())

		mcp := &ssv1alpha1.ManifestCheckpoint{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: mcpName}, mcp)).To(Succeed())
		Expect(ownerRefToNamespaceSnapshotContent(mcp.OwnerReferences, contentName, nsc.UID)).To(BeTrue())

		Expect(k8sClient.Delete(ctx, &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snap.Name, Namespace: nsName},
		})).To(Succeed())

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, key, &storagev1alpha1.NamespaceSnapshot{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}).WithTimeout(60 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, &storagev1alpha1.NamespaceSnapshotContent{})).To(Succeed())
		Expect(k8sClient.Get(ctx, mcrKey, &ssv1alpha1.ManifestCaptureRequest{})).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: mcpName}, &ssv1alpha1.ManifestCheckpoint{})).To(Succeed())
	})

	It("Retain: user can delete NamespaceSnapshotContent after root snapshot is gone (deletion completes; no GC/TTL contract)", func() {
		ctx := context.Background()
		contentName := ""

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-del-content-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "namespacesnapshot-retain-delete-content",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			if contentName != "" {
				_ = k8sClient.Delete(ctx, &storagev1alpha1.NamespaceSnapshotContent{
					ObjectMeta: metav1.ObjectMeta{Name: contentName},
				})
			}
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-del-content-cm", Namespace: nsName},
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
		}).WithTimeout(90 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

		Expect(k8sClient.Delete(ctx, &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snap.Name, Namespace: nsName},
		})).To(Succeed())

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, key, &storagev1alpha1.NamespaceSnapshot{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}).WithTimeout(60 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, &storagev1alpha1.NamespaceSnapshotContent{})).To(Succeed())

		Expect(k8sClient.Delete(ctx, &storagev1alpha1.NamespaceSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: contentName},
		})).To(Succeed())

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, &storagev1alpha1.NamespaceSnapshotContent{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}).WithTimeout(120 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		contentName = ""
	})
})
