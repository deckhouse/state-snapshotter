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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	deckhousev1alpha1 "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/deckhouseio/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// deletionPolicyNamespace creates a namespace holding one ConfigMap, so a capture of it has something to
// store, and removes it after the spec.
func deletionPolicyNamespace(ctx context.Context, prefix string) string {
	GinkgoHelper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	DeferCleanup(func() {
		_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns.Name}})
	})
	Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "payload", Namespace: ns.Name},
		Data:       map[string]string{"k": "v"},
	})).To(Succeed())
	return ns.Name
}

// deletionPolicySnapshot builds a Snapshot as unstructured, which is how a client that sets fields the typed
// API may not hold (an unknown value, a field on the wrong mode) reaches the apiserver.
func deletionPolicySnapshot(ns, name, mode, policy string) *unstructured.Unstructured {
	spec := map[string]interface{}{}
	if mode != "" {
		spec["mode"] = mode
	}
	if policy != "" {
		spec["deletionPolicy"] = policy
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "Snapshot",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec":       spec,
	}}
}

// waitDeletionPolicyRootReady waits for the root to reach Ready=True and returns its bound content.
func waitDeletionPolicyRootReady(ctx context.Context, key types.NamespacedName) *storagev1alpha1.SnapshotContent {
	GinkgoHelper()
	content := &storagev1alpha1.SnapshotContent{}
	Eventually(func(g Gomega) {
		fresh := &storagev1alpha1.Snapshot{}
		g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
		g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())
		ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: fresh.Status.BoundSnapshotContentName}, content)).To(Succeed())
	}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())
	return content
}

// spec.deletionPolicy lets the creator of a capture root choose, once, whether deleting it parks the stored
// tree in the recycle bin (Retain, the behaviour of every tree before the field existed) or reclaims it
// right away (Delete). The admission half is CEL on the CRD, so it is checked against the apiserver here; a
// fake client would bypass it.
var _ = Describe("Integration: Snapshot spec.deletionPolicy", func() {
	Describe("admission", func() {
		var ns string
		BeforeEach(func() {
			nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ss-snap-delpolicy-cel-"}}
			Expect(k8sClient.Create(context.Background(), nsObj)).To(Succeed())
			ns = nsObj.Name
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
			})
		})

		// Import is refused with either value: an imported tree is always reclaimed on deletion, so the field
		// there could only restate that (Delete) or promise something the controller does not do (Retain).
		DescribeTable("refuses the field on an Import snapshot",
			func(name, policy string) {
				err := k8sClient.Create(context.Background(), deletionPolicySnapshot(ns, name, string(storagev1alpha1.SnapshotModeImport), policy))
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "want an admission rejection, got %v", err)
				Expect(err.Error()).To(ContainSubstring("spec.deletionPolicy applies to Capture only"))
			},
			Entry("Delete", "import-delete", storagev1alpha1.SnapshotContentDeletionPolicyDelete),
			Entry("Retain", "import-retain", storagev1alpha1.SnapshotContentDeletionPolicyRetain),
		)

		It("still accepts an Import snapshot without the field", func() {
			Expect(k8sClient.Create(context.Background(), deletionPolicySnapshot(ns, "import-plain", string(storagev1alpha1.SnapshotModeImport), ""))).To(Succeed())
		})

		DescribeTable("accepts the field on a Capture snapshot and stores it as sent",
			func(name, mode, policy string) {
				created := deletionPolicySnapshot(ns, name, mode, policy)
				Expect(k8sClient.Create(context.Background(), created)).To(Succeed())
				got, _, err := unstructured.NestedString(created.Object, "spec", "deletionPolicy")
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(Equal(policy), "the field is not defaulted: what is stored is what was sent")
			},
			Entry("Capture, Delete", "capture-delete", string(storagev1alpha1.SnapshotModeCapture), storagev1alpha1.SnapshotContentDeletionPolicyDelete),
			Entry("Capture, Retain", "capture-retain", string(storagev1alpha1.SnapshotModeCapture), storagev1alpha1.SnapshotContentDeletionPolicyRetain),
			Entry("mode omitted, Delete", "default-mode-delete", "", storagev1alpha1.SnapshotContentDeletionPolicyDelete),
			Entry("field omitted", "capture-omitted", string(storagev1alpha1.SnapshotModeCapture), ""),
		)

		It("refuses a value outside Retain and Delete", func() {
			err := k8sClient.Create(context.Background(), deletionPolicySnapshot(ns, "capture-orphan", string(storagev1alpha1.SnapshotModeCapture), "Orphan"))
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "want an admission rejection, got %v", err)
		})

		// The spec-level self == oldSelf rule covers the new field as well: the policy is chosen at creation,
		// and neither setting it later nor changing it is possible.
		DescribeTable("refuses changing the field after creation",
			func(name, from, to string) {
				ctx := context.Background()
				Expect(k8sClient.Create(ctx, deletionPolicySnapshot(ns, name, string(storagev1alpha1.SnapshotModeCapture), from))).To(Succeed())
				key := client.ObjectKey{Namespace: ns, Name: name}
				Eventually(func(g Gomega) {
					cur := &storagev1alpha1.Snapshot{}
					g.Expect(k8sClient.Get(ctx, key, cur)).To(Succeed())
					cur.Spec.DeletionPolicy = to
					err := k8sClient.Update(ctx, cur)
					g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "want an admission rejection of the spec change, got %v", err)
				}).Should(Succeed())
			},
			Entry("Retain to Delete", "change-retain-delete", storagev1alpha1.SnapshotContentDeletionPolicyRetain, storagev1alpha1.SnapshotContentDeletionPolicyDelete),
			Entry("Delete to Retain", "change-delete-retain", storagev1alpha1.SnapshotContentDeletionPolicyDelete, storagev1alpha1.SnapshotContentDeletionPolicyRetain),
			Entry("omitted to Delete", "change-omitted-delete", "", storagev1alpha1.SnapshotContentDeletionPolicyDelete),
		)
	})

	It("Retain: the root content takes the policy and is parked under the root ObjectKeeper after deletion", func() {
		ctx := context.Background()
		ns := deletionPolicyNamespace(ctx, "ss-snap-delpolicy-retain-")
		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns},
			Spec: storagev1alpha1.SnapshotSpec{
				Mode:           storagev1alpha1.SnapshotModeCapture,
				DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
			},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		key := client.ObjectKeyFromObject(snap)
		content := waitDeletionPolicyRootReady(ctx, key)
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: content.Name}})
		})
		Expect(content.Spec.DeletionPolicy).To(Equal(storagev1alpha1.SnapshotContentDeletionPolicyRetain))
		okName := snapshot.GenerateObjectKeeperName(snap.UID)

		Expect(k8sClient.Delete(ctx, snap)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &storagev1alpha1.Snapshot{}))).To(BeTrue())
		}).WithTimeout(60 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		// Parked in the recycle bin: the content outlives its Snapshot, latched as orphaned, still owned by
		// the ObjectKeeper whose TTL decides when it goes.
		ok := &deckhousev1alpha1.ObjectKeeper{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: okName}, ok)).To(Succeed())
		Expect(ok.Spec.TTL).NotTo(BeNil())
		Expect(ok.Spec.TTL.Duration).To(BeNumerically(">", 0))
		Eventually(func(g Gomega) {
			parked := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: content.Name}, parked)).To(Succeed())
			g.Expect(parked.DeletionTimestamp).To(BeNil())
			g.Expect(parked.Status.BoundSnapshotDeleted).To(BeTrue())
			g.Expect(rootContentOwnerRefToObjectKeeper(parked.OwnerReferences, okName, ok.UID)).To(BeTrue())
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})

	It("Delete: the root content takes the policy and is deleted together with the root, without the TTL", func() {
		ctx := context.Background()
		ns := deletionPolicyNamespace(ctx, "ss-snap-delpolicy-delete-")
		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns},
			Spec:       storagev1alpha1.SnapshotSpec{DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyDelete},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		key := client.ObjectKeyFromObject(snap)
		content := waitDeletionPolicyRootReady(ctx, key)
		Expect(content.Spec.DeletionPolicy).To(Equal(storagev1alpha1.SnapshotContentDeletionPolicyDelete))
		// Same ownership shape as a Retain tree: only what the root's deletion does differs.
		ok := &deckhousev1alpha1.ObjectKeeper{}
		okName := snapshot.GenerateObjectKeeperName(snap.UID)
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: okName}, ok)).To(Succeed())
		Expect(rootContentOwnerRefToObjectKeeper(content.OwnerReferences, okName, ok.UID)).To(BeTrue())

		Expect(k8sClient.Delete(ctx, snap)).To(Succeed())
		// The content is gone first — the root keeps its finalizer until it is — and the keeper's TTL, which
		// is far longer than this spec, has nothing to do with it.
		Eventually(func(g Gomega) {
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Name: content.Name}, &storagev1alpha1.SnapshotContent{}))).To(BeTrue())
		}).WithTimeout(60 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &storagev1alpha1.Snapshot{}))).To(BeTrue())
		}).WithTimeout(60 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})

	// A child content copies the policy of its parent's content. The child here is a generic snapshot kind
	// controlled by the root Snapshot — the same shape as an orphan-PVC VolumeSnapshot node or a domain
	// child — driven by a manager-registered generic binder.
	DescribeTable("a child content inherits the root's policy",
		func(rootPolicy, wantChild string) {
			ctx := context.Background()
			childGVK := schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "RegistrationTestSnapshot"}
			registerGenericMirrorBinderOnce.Do(func() {
				binder, err := controllers.NewGenericSnapshotBinderController(mgr.GetClient(), mgr.GetAPIReader(), scheme, testCfg, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(binder.AddWatchForPair(mgr, childGVK, unifiedbootstrap.CommonSnapshotContentGVK())).To(Succeed())
			})

			ns := deletionPolicyNamespace(ctx, "ss-snap-delpolicy-child-")
			root := &storagev1alpha1.Snapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: ns},
				Spec:       storagev1alpha1.SnapshotSpec{DeletionPolicy: rootPolicy},
			}
			Expect(k8sClient.Create(ctx, root)).To(Succeed())
			rootKey := client.ObjectKeyFromObject(root)
			rootContent := &storagev1alpha1.SnapshotContent{}
			Eventually(func(g Gomega) {
				fresh := &storagev1alpha1.Snapshot{}
				g.Expect(k8sClient.Get(ctx, rootKey, fresh)).To(Succeed())
				g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: fresh.Status.BoundSnapshotContentName}, rootContent)).To(Succeed())
			}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: rootContent.Name}})
			})

			child := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{}}}
			child.SetGroupVersionKind(childGVK)
			child.SetNamespace(ns)
			child.SetName("child")
			controller := true
			child.SetOwnerReferences([]metav1.OwnerReference{{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "Snapshot",
				Name:       root.Name,
				UID:        root.UID,
				Controller: &controller,
			}})
			Expect(k8sClient.Create(ctx, child)).To(Succeed())
			DeferCleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), child)) })

			childContent := &storagev1alpha1.SnapshotContent{}
			Eventually(func(g Gomega) {
				fresh := &unstructured.Unstructured{}
				fresh.SetGroupVersionKind(childGVK)
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(child), fresh)).To(Succeed())
				bound, _, _ := unstructured.NestedString(fresh.Object, "status", "boundSnapshotContentName")
				g.Expect(bound).NotTo(BeEmpty())
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: bound}, childContent)).To(Succeed())
			}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: childContent.Name}})
			})

			Expect(childContent.Spec.DeletionPolicy).To(Equal(wantChild))
			owner := metav1.GetControllerOf(childContent)
			Expect(owner).NotTo(BeNil())
			Expect(owner.Kind).To(Equal("SnapshotContent"))
			Expect(owner.UID).To(Equal(rootContent.UID), "the content the policy is copied from is the one that owns the child")
		},
		Entry("Delete root", storagev1alpha1.SnapshotContentDeletionPolicyDelete, storagev1alpha1.SnapshotContentDeletionPolicyDelete),
		Entry("Retain root", storagev1alpha1.SnapshotContentDeletionPolicyRetain, storagev1alpha1.SnapshotContentDeletionPolicyRetain),
		Entry("root without the field", "", storagev1alpha1.SnapshotContentDeletionPolicyRetain),
	)
})
