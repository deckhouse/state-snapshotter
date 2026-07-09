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
			Spec:       retainContentSpec(),
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
