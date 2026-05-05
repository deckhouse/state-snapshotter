//go:build integration
// +build integration

package integration

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

var _ = Describe("Integration: SnapshotContent spec immutability", func() {
	It("allows status updates and rejects spec changes", func() {
		ctx := context.Background()
		content := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "spec-immutable-content-"},
			Spec: storagev1alpha1.SnapshotContentSpec{
				BackupRepositoryName: "repo-a",
				DeletionPolicy:       storagev1alpha1.SnapshotContentDeletionPolicyRetain,
			},
		}
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: content.Name}})
		})
		Expect(k8sClient.Create(ctx, content)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: content.Name}, content)).To(Succeed())
		}).Should(Succeed())
		content.Status.ManifestCheckpointName = "mcp-a"
		setSnapshotContentReadyConditionForTest(&content.Status.Conditions)
		Expect(k8sClient.Status().Update(ctx, content)).To(Succeed())

		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: content.Name}, content)).To(Succeed())
		specPatch := content.DeepCopy()
		specPatch.Spec.DeletionPolicy = storagev1alpha1.SnapshotContentDeletionPolicyDelete
		Expect(k8sClient.Update(ctx, specPatch)).NotTo(Succeed())

		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: content.Name}, content)).To(Succeed())
		specPatch = content.DeepCopy()
		specPatch.Spec.BackupRepositoryName = "repo-b"
		Expect(k8sClient.Update(ctx, specPatch)).NotTo(Succeed())
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
