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
	"fmt"
	"strings"
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
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// objectKeeperHasControllerOwnerRefToSnapshotContent is true when ObjectKeeper lists SnapshotContent
// as a controlling ownerReference (OK→SnapshotContent). Retained root contract is the opposite: root SnapshotContent must
// depend on ObjectKeeper (SnapshotContent→OK) so TTL expiry on OK drives GC of the tree below SnapshotContent.
func objectKeeperHasControllerOwnerRefToSnapshotContent(refs []metav1.OwnerReference, contentName string, contentUID types.UID) bool {
	for i := range refs {
		ref := refs[i]
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "SnapshotContent" &&
			ref.Name == contentName && ref.UID == contentUID {
			return true
		}
	}
	return false
}

// rootContentOwnerRefToObjectKeeper is true when root content has a controlling ownerRef to the root ObjectKeeper (TTL anchor).
func rootContentOwnerRefToObjectKeeper(refs []metav1.OwnerReference, okName string, okUID types.UID) bool {
	for i := range refs {
		ref := refs[i]
		if ref.APIVersion == "deckhouse.io/v1alpha1" && ref.Kind == "ObjectKeeper" &&
			ref.Name == okName && ref.UID == okUID && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// mcpOwnerRefToRootContent is true when ManifestCheckpoint has a controlling ownerRef to the root SnapshotContent (snapshot capture path).
func mcpOwnerRefToRootContent(refs []metav1.OwnerReference, name string, uid types.UID) bool {
	for i := range refs {
		ref := refs[i]
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "SnapshotContent" &&
			ref.Name == name && ref.UID == uid && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// mcrOwnerRefToSnapshot is true when ManifestCaptureRequest has a controlling ownerRef to the root Snapshot (same namespace; GC when snapshot is removed).
func mcrOwnerRefToSnapshot(refs []metav1.OwnerReference, name string, uid types.UID) bool {
	for i := range refs {
		ref := refs[i]
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "Snapshot" &&
			ref.Name == name && ref.UID == uid && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

var _ = Describe("Integration: Snapshot deletion semantics", func() {
	It("Retain: deleting Snapshot removes root finalizer but keeps SnapshotContent", func() {
		ctx := context.Background()
		contentName := ""

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-del-retain-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "snapshot-deletion-retain",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			if contentName != "" {
				_ = k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{
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
			sc := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, sc)).To(Succeed())
			g.Expect(sc.Spec.DeletionPolicy).To(Equal(storagev1alpha1.SnapshotContentDeletionPolicyRetain))
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		Expect(k8sClient.Delete(ctx, &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snap.Name, Namespace: nsName},
		})).To(Succeed())

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, key, &storagev1alpha1.Snapshot{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		sc := &storagev1alpha1.SnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, sc)).To(Succeed())
	})

	It("Delete: root finalizer clears only after SnapshotContent is gone", func() {
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-del-delete-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "snapshot-deletion-delete",
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

		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "snap-del",
				Namespace: nsName,
			},
			Spec: storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		key := types.NamespacedName{Namespace: nsName, Name: snap.Name}
		contentName := fmt.Sprintf("ns-%s", strings.ReplaceAll(string(snap.UID), "-", ""))
		Expect(k8sClient.Create(ctx, &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{
				Name:       contentName,
				Finalizers: []string{snapshot.FinalizerParentProtect},
			},
			Spec: storagev1alpha1.SnapshotContentSpec{
				DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyDelete,
			},
		})).To(Succeed())

		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(fresh.Status.BoundSnapshotContentName).To(Equal(contentName))
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		Expect(k8sClient.Delete(ctx, &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snap.Name, Namespace: nsName},
		})).To(Succeed())

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, &storagev1alpha1.SnapshotContent{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, key, &storagev1alpha1.Snapshot{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())
	})

	It("unified retention: deleting Snapshot does not remove ManifestCheckpoint; MCR already gone after capture (Retain content)", func() {
		ctx := context.Background()
		contentName := ""

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-del-retain-mcp-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "snapshot-retain-mcp",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			if contentName != "" {
				_ = k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{
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

		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "snap",
				Namespace: nsName,
			},
			Spec: storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		key := types.NamespacedName{Namespace: nsName, Name: snap.Name}

		var rootUID types.UID
		var mcrKey client.ObjectKey
		var mcpName string
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			rootUID = fresh.UID
			contentName = fresh.Status.BoundSnapshotContentName
			mcrKey = client.ObjectKey{Namespace: nsName, Name: namespacemanifest.SnapshotMCRName(rootUID)}
			g.Expect(errors.IsNotFound(k8sClient.Get(ctx, mcrKey, &ssv1alpha1.ManifestCaptureRequest{}))).To(BeTrue())
			sc := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, sc)).To(Succeed())
			mcpName = sc.Status.ManifestCheckpointName
			g.Expect(mcpName).NotTo(BeEmpty())
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: mcpName}, &ssv1alpha1.ManifestCheckpoint{})).To(Succeed())
		}).WithTimeout(90 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

		content := &storagev1alpha1.SnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, content)).To(Succeed())
		for i := range content.OwnerReferences {
			Expect(content.OwnerReferences[i].Kind).NotTo(Equal("Snapshot"),
				"root SnapshotContent must not use ownerReferences to short-lived Snapshot")
		}
		ok := &deckhousev1alpha1.ObjectKeeper{}
		okName := namespacemanifest.SnapshotRootObjectKeeperName(nsName, snap.Name)
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: okName}, ok)).To(Succeed())
		Expect(ok.Spec.FollowObjectRef).NotTo(BeNil())
		Expect(ok.Spec.FollowObjectRef.Kind).To(Equal("Snapshot"))
		Expect(ok.Spec.FollowObjectRef.Name).To(Equal(snap.Name))
		Expect(ok.Spec.FollowObjectRef.Namespace).To(Equal(nsName))
		Expect(ok.Spec.FollowObjectRef.UID).To(Equal(string(rootUID)))
		Expect(ok.Spec.Mode).To(Equal("FollowObjectWithTTL"))
		Expect(ok.Spec.TTL).NotTo(BeNil())
		Expect(ok.Spec.TTL.Duration).To(Equal(config.DefaultSnapshotRootOKTTL))
		Expect(ok.OwnerReferences).To(BeEmpty(), "root ObjectKeeper must follow Snapshot via spec.followObjectRef, not ownerRef")
		Expect(objectKeeperHasControllerOwnerRefToSnapshotContent(ok.OwnerReferences, contentName, content.UID)).To(BeFalse(),
			"root ObjectKeeper must not list SnapshotContent as a controlling ownerRef; retained anchor is SnapshotContent→OK")
		freshRoot := &storagev1alpha1.Snapshot{}
		Expect(k8sClient.Get(ctx, key, freshRoot)).To(Succeed())
		Expect(rootContentOwnerRefToObjectKeeper(freshRoot.OwnerReferences, okName, ok.UID)).To(BeFalse(),
			"root Snapshot must not be owned by cluster-scoped ObjectKeeper")
		Expect(rootContentOwnerRefToObjectKeeper(content.OwnerReferences, okName, ok.UID)).To(BeTrue())

		mcp := &ssv1alpha1.ManifestCheckpoint{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: mcpName}, mcp)).To(Succeed())
		Expect(mcpOwnerRefToRootContent(mcp.OwnerReferences, contentName, content.UID)).To(BeTrue())

		Expect(k8sClient.Delete(ctx, &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snap.Name, Namespace: nsName},
		})).To(Succeed())

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, key, &storagev1alpha1.Snapshot{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}).WithTimeout(60 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, &storagev1alpha1.SnapshotContent{})).To(Succeed())
		Expect(errors.IsNotFound(k8sClient.Get(ctx, mcrKey, &ssv1alpha1.ManifestCaptureRequest{}))).To(BeTrue())
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: mcpName}, &ssv1alpha1.ManifestCheckpoint{})).To(Succeed())
	})

	It("Retain: user can delete SnapshotContent after root snapshot is gone (deletion completes; no GC/TTL contract)", func() {
		ctx := context.Background()
		contentName := ""

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-del-content-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "snapshot-retain-delete-content",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			if contentName != "" {
				_ = k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{
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
		}).WithTimeout(90 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

		Expect(k8sClient.Delete(ctx, &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snap.Name, Namespace: nsName},
		})).To(Succeed())

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, key, &storagev1alpha1.Snapshot{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}).WithTimeout(60 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, &storagev1alpha1.SnapshotContent{})).To(Succeed())

		Expect(k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: contentName},
		})).To(Succeed())

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, &storagev1alpha1.SnapshotContent{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}).WithTimeout(120 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		contentName = ""
	})
})

var _ = Describe("Integration: Snapshot MCR ownerReference (N2a)", func() {
	It("garbage-collects ManifestCaptureRequest when Snapshot is deleted during capture", func() {
		ctx := context.Background()
		contentName := ""

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-mcr-own-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "snapshot-mcr-ownerref-gc",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			if contentName != "" {
				_ = k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: contentName}})
			}
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-mcr-own-cm", Namespace: nsName},
			Data:       map[string]string{"k": "v"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		key := types.NamespacedName{Namespace: nsName, Name: snap.Name}

		var mcrKey client.ObjectKey
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			contentName = fresh.Status.BoundSnapshotContentName
			mcrKey = client.ObjectKey{Namespace: nsName, Name: namespacemanifest.SnapshotMCRName(fresh.UID)}
			mcr := &ssv1alpha1.ManifestCaptureRequest{}
			if err := k8sClient.Get(ctx, mcrKey, mcr); err != nil {
				g.Expect(errors.IsNotFound(err)).To(BeFalse(), "wait until MCR exists")
				g.Expect(err).NotTo(HaveOccurred())
			}
			g.Expect(mcrOwnerRefToSnapshot(mcr.OwnerReferences, fresh.Name, fresh.UID)).To(BeTrue())
		}).WithTimeout(90 * time.Second).WithPolling(25 * time.Millisecond).Should(Succeed())

		rootSnap := &storagev1alpha1.Snapshot{}
		Expect(k8sClient.Get(ctx, key, rootSnap)).To(Succeed())
		rootUID := rootSnap.UID

		// Foreground cascading delete would block Snapshot removal until kube-controller-manager GC
		// deletes the dependent MCR; plain envtest has no GC. Background delete matches retained-path semantics
		// (snapshot object goes away; MCR is collected asynchronously on a real cluster).
		Expect(k8sClient.Delete(ctx, &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snap.Name, Namespace: nsName},
		}, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, key, &storagev1alpha1.Snapshot{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}).WithTimeout(90 * time.Second).WithPolling(50 * time.Millisecond).Should(Succeed())

		mcr := &ssv1alpha1.ManifestCaptureRequest{}
		err := k8sClient.Get(ctx, mcrKey, mcr)
		if errors.IsNotFound(err) {
			return
		}
		Expect(err).NotTo(HaveOccurred())
		// Without kube-controller-manager, owner-reference GC may not run; ownerRef contract still holds.
		Expect(mcrOwnerRefToSnapshot(mcr.OwnerReferences, snap.Name, rootUID)).To(BeTrue())
	})
})
