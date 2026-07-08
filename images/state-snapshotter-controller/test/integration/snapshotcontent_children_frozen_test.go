//go:build integration
// +build integration

package integration

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// Block 4 (content-single-writer design §3.4, INV-CONTENT-CHILDREN-2): status.childrenSnapshotContentRefs is
// FROZEN once non-empty. The aggregator is the sole writer and publishes the complete child set in one
// transition (all-or-nothing, empty -> complete), so the Option A CEL rule
// (oldSelf.size()==0 || self==oldSelf) can pin true immutability at the API level: the empty->set transition
// is the ONLY allowed change; any later add, remove, reorder, or replace is rejected. These admission
// contract tests pin that behaviour (they also transitively prove the regenerated CRD's CEL cost estimate is
// within the apiserver budget — the MaxItems/MaxLength bounds — since an over-budget rule would make CRD
// installation, and thus this whole suite, fail).
var _ = Describe("Integration: SnapshotContent childrenSnapshotContentRefs frozen set", func() {
	It("accepts the first complete set then rejects any shrink/append/reorder/replace", func() {
		ctx := context.Background()
		content := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "children-frozen-content-"},
			Spec:       retainContentSpec(),
		}
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: content.Name}})
		})
		Expect(k8sClient.Create(ctx, content)).To(Succeed())
		key := client.ObjectKey{Name: content.Name}

		refs := func(names ...string) []storagev1alpha1.SnapshotContentChildRef {
			out := make([]storagev1alpha1.SnapshotContentChildRef, 0, len(names))
			for _, n := range names {
				out = append(out, storagev1alpha1.SnapshotContentChildRef{Name: n})
			}
			return out
		}

		// setChildren re-Gets a fresh object inside Eventually so a concurrent SnapshotContent-controller
		// status write (finalizer/conditions) surfaces as a retryable conflict rather than a spurious 409,
		// and asserts the update either succeeds (wantAccept) or is rejected by CEL admission (IsInvalid).
		setChildren := func(wantAccept bool, names ...string) {
			Eventually(func(g Gomega) {
				fresh := &storagev1alpha1.SnapshotContent{}
				g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
				fresh.Status.ChildrenSnapshotContentRefs = refs(names...)
				err := k8sClient.Status().Update(ctx, fresh)
				if wantAccept {
					g.Expect(err).NotTo(HaveOccurred())
				} else {
					g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected a CEL Invalid rejection, got %v", err)
				}
			}).Should(Succeed())
		}

		By("accepting the empty -> complete first set (oldSelf.size()==0)")
		setChildren(true, "child-a", "child-b")

		By("rejecting a shrink (dropping child-b)")
		setChildren(false, "child-a")

		By("rejecting an append (adding child-c)")
		setChildren(false, "child-a", "child-b", "child-c")

		By("rejecting a reorder (same members, different order)")
		setChildren(false, "child-b", "child-a")

		By("rejecting a replace (child-b -> child-c at the same count)")
		setChildren(false, "child-a", "child-c")

		By("accepting an idempotent re-write of the identical set (self==oldSelf)")
		setChildren(true, "child-a", "child-b")

		fresh := &storagev1alpha1.SnapshotContent{}
		Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
		Expect(fresh.Status.ChildrenSnapshotContentRefs).To(Equal(refs("child-a", "child-b")),
			"the frozen set must remain the accepted first set after every rejected mutation")
	})
})
