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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// reclaimTestHoldFinalizer keeps the VolumeSnapshotContent observable after its delete: envtest has no CSI
// sidecar, so without it the object would vanish the moment it is deleted and the policy it was deleted
// with could not be read back.
const reclaimTestHoldFinalizer = "test.state-snapshotter.deckhouse.io/hold"

// Label("isolated"): the VolumeSnapshotContent CRD these specs need is left out of the shared suite on
// purpose (see pr7InstallCSIClassAndContentCRDs).
//
// With spec.deletionPolicy=Delete, deleting the capture root must end with the physical reclaim of every
// volume snapshot of the tree, and the client must not have to touch anything but the root. The chain is:
// the root's deletion deletes the root content (instead of parking it under the ObjectKeeper's TTL); the
// garbage collector deletes each child content once its owner is gone; each child content's own teardown
// flips its VolumeSnapshotContent from Retain to Delete, stamps the CSI deletion annotation and deletes it.
// envtest runs no garbage collector, so the one step in the middle is played by the spec: it deletes the
// child content itself, after asserting that nothing below the root was reclaimed early.
var _ = Describe("Integration: a Delete-policy capture root reclaims the volume snapshots of its tree", Serial, Ordered, Label("isolated"), func() {
	BeforeAll(func() {
		pr7InstallCSIClassAndContentCRDs(context.Background())
	})

	It("deletes the root content with the root, and the child's teardown reclaims its VolumeSnapshotContent", func() {
		ctx := context.Background()
		ns := deletionPolicyNamespace(ctx, "ss-snap-delpolicy-reclaim-")
		root := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: ns},
			Spec:       storagev1alpha1.SnapshotSpec{DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyDelete},
		}
		Expect(k8sClient.Create(ctx, root)).To(Succeed())
		rootKey := client.ObjectKeyFromObject(root)
		rootContent := waitDeletionPolicyRootReady(ctx, rootKey)
		Expect(rootContent.Spec.DeletionPolicy).To(Equal(storagev1alpha1.SnapshotContentDeletionPolicyDelete))

		By("Creating the durable artifact of a volume node, pinned to Retain the way the tree keeps it")
		vscName := "vsc-" + ns
		vsc := &unstructured.Unstructured{}
		vsc.SetGroupVersionKind(pr7VSCGVK)
		vsc.SetName(vscName)
		vsc.SetFinalizers([]string{reclaimTestHoldFinalizer})
		_ = unstructured.SetNestedField(vsc.Object, "Retain", "spec", "deletionPolicy")
		_ = unstructured.SetNestedField(vsc.Object, pr7CSIDriver, "spec", "driver")
		_ = unstructured.SetNestedField(vsc.Object, vscName+"-handle", "spec", "source", "snapshotHandle")
		_ = unstructured.SetNestedField(vsc.Object, true, "status", "readyToUse")
		Expect(k8sClient.Create(ctx, vsc)).To(Succeed())
		DeferCleanup(func() {
			cur := &unstructured.Unstructured{}
			cur.SetGroupVersionKind(pr7VSCGVK)
			if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: vscName}, cur); err == nil {
				cur.SetFinalizers(nil)
				_ = k8sClient.Update(context.Background(), cur)
				_ = k8sClient.Delete(context.Background(), cur)
			}
		})

		By("Creating the child volume node's content under the root content, with the policy it inherits")
		controller := true
		child := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{
				Name: rootContent.Name + "-volume",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
					Kind:       "SnapshotContent",
					Name:       rootContent.Name,
					UID:        rootContent.UID,
					Controller: &controller,
				}},
			},
			Spec: storagev1alpha1.SnapshotContentSpec{
				DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyDelete,
				// The child's own snapshot is not part of this fixture; its absence only makes the content
				// controller latch boundSnapshotDeleted and leave the seeded status alone.
				SnapshotRef: integrationContentSnapshotRef(),
			},
		}
		Expect(k8sClient.Create(ctx, child)).To(Succeed())
		childKey := client.ObjectKeyFromObject(child)
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: child.Name}}))
		})
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur := &storagev1alpha1.SnapshotContent{}
			if err := k8sClient.Get(ctx, childKey, cur); err != nil {
				return err
			}
			base := cur.DeepCopy()
			cur.Status.Data = &storagev1alpha1.SnapshotDataBinding{
				SourceRef: storagev1alpha1.SnapshotSubjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: ns, Name: "data"},
				ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
					APIVersion: "snapshot.storage.k8s.io/v1", Kind: snapshot.KindVolumeSnapshotContent, Name: vscName,
				},
			}
			return k8sClient.Status().Patch(ctx, cur, client.MergeFrom(base))
		})).To(Succeed())
		Eventually(func(g Gomega) {
			cur := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, childKey, cur)).To(Succeed())
			g.Expect(cur.Finalizers).To(ContainElement(snapshot.FinalizerParentProtect))
			g.Expect(cur.Status.Data).NotTo(BeNil())
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		By("Deleting the root Snapshot, the only object the client deletes")
		Expect(k8sClient.Delete(ctx, root)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Name: rootContent.Name}, &storagev1alpha1.SnapshotContent{}))).To(BeTrue(),
				"a Delete root must take its content with it, not park it under the ObjectKeeper's TTL")
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, rootKey, &storagev1alpha1.Snapshot{}))).To(BeTrue())
		}).WithTimeout(60 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		// Nothing below the root is reclaimed by the root's own teardown: each node reclaims its own artifact.
		cur := &unstructured.Unstructured{}
		cur.SetGroupVersionKind(pr7VSCGVK)
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: vscName}, cur)).To(Succeed())
		Expect(cur.GetDeletionTimestamp()).To(BeNil())
		policy, _, _ := unstructured.NestedString(cur.Object, "spec", "deletionPolicy")
		Expect(policy).To(Equal("Retain"))

		By("Deleting the child content, as the garbage collector does once its owner is gone")
		Expect(k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: child.Name}})).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, childKey, &storagev1alpha1.SnapshotContent{}))).To(BeTrue())
		}).WithTimeout(60 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
		Eventually(func(g Gomega) {
			reclaimed := &unstructured.Unstructured{}
			reclaimed.SetGroupVersionKind(pr7VSCGVK)
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: vscName}, reclaimed)).To(Succeed())
			g.Expect(reclaimed.GetDeletionTimestamp()).NotTo(BeNil(), "the VolumeSnapshotContent must be deleted")
			policy, _, _ := unstructured.NestedString(reclaimed.Object, "spec", "deletionPolicy")
			g.Expect(policy).To(Equal("Delete"), "and deleted with Delete, so the CSI sidecar removes the physical snapshot")
			g.Expect(reclaimed.GetAnnotations()).To(HaveKeyWithValue("snapshot.storage.kubernetes.io/volumesnapshot-being-deleted", "yes"))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})
})
