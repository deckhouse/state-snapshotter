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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// The SnapshotContent spec is frozen by root-level CEL transition rules, with a single recycle-bin
// carve-out: spec.snapshotRef may be re-pointed (restore) only once status.parentDeleted latched true,
// while spec.deletionPolicy stays immutable in all cases. These admission contract tests pin that
// behaviour so the anti-spoofing handshake (alive parent) and the wave4B restore path cannot regress.
var _ = Describe("Integration: SnapshotContent spec immutability", func() {
	It("freezes spec while the parent is alive and allows only a snapshotRef re-point once parentDeleted latches", func() {
		ctx := context.Background()
		content := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "spec-immutable-content-"},
			Spec:       retainContentSpec(),
		}
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: content.Name}})
		})
		Expect(k8sClient.Create(ctx, content)).To(Succeed())
		key := client.ObjectKey{Name: content.Name}

		// Status updates remain allowed (immutability is spec-only). Re-Get a fresh object inside Eventually
		// so a concurrent SnapshotContent-controller write (it adds the parent-protect finalizer right after
		// creation, bumping resourceVersion) surfaces as a retryable conflict rather than a spurious 409.
		By("allowing status updates (immutability is spec-only)")
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			fresh.Status.ManifestCheckpointName = "mcp-a"
			setSnapshotContentReadyConditionForTest(&fresh.Status.Conditions)
			g.Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())
		}).Should(Succeed())

		// Each mutation re-Gets a fresh object inside Eventually so a concurrent SnapshotContent-controller
		// status write surfaces as a retryable conflict rather than masking the CEL admission result.
		By("rejecting a snapshotRef re-point while the parent Snapshot is alive (parentDeleted=false)")
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			fresh.Spec.SnapshotRef.Name = "restored-snapshot"
			g.Expect(apierrors.IsInvalid(k8sClient.Update(ctx, fresh))).To(BeTrue())
		}).Should(Succeed())

		By("rejecting a deletionPolicy change while the parent Snapshot is alive")
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			fresh.Spec.DeletionPolicy = storagev1alpha1.SnapshotContentDeletionPolicyDelete
			g.Expect(apierrors.IsInvalid(k8sClient.Update(ctx, fresh))).To(BeTrue())
		}).Should(Succeed())

		By("latching status.parentDeleted=true (the recycle-bin gate)")
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			fresh.Status.ParentDeleted = true
			g.Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())
		}).Should(Succeed())

		By("allowing a snapshotRef re-point once parentDeleted latched true")
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			fresh.Spec.SnapshotRef.Name = "restored-snapshot"
			fresh.Spec.SnapshotRef.UID = "restored-uid"
			g.Expect(k8sClient.Update(ctx, fresh)).To(Succeed())
		}).Should(Succeed())

		By("still rejecting a deletionPolicy change even after parentDeleted latched")
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			fresh.Spec.DeletionPolicy = storagev1alpha1.SnapshotContentDeletionPolicyDelete
			g.Expect(apierrors.IsInvalid(k8sClient.Update(ctx, fresh))).To(BeTrue())
		}).Should(Succeed())
	})
})

func setSnapshotContentReadyConditionForTest(conditions *[]metav1.Condition) {
	*conditions = append(*conditions, metav1.Condition{
		Type:               snapshot.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             snapshot.ReasonManifestCapturePending,
		Message:            "test status update",
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: 1,
	})
}
