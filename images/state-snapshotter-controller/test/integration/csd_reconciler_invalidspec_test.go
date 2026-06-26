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
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Integration: CSD reconciler InvalidSpec", func() {
	const unresolvableName = "integration-csd-unresolvable-kind"

	BeforeEach(func() {
		for _, n := range []string{unresolvableName} {
			d := &storagev1alpha1.CustomSnapshotDefinition{}
			d.SetName(n)
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, d))
		}
	})

	It("sets Accepted=False InvalidSpec when the snapshot kind cannot be resolved (no CRD)", func() {
		// Flat schema: one mapping per CSD. The InvalidSpec path is now an unresolvable GVK — a snapshot
		// kind that passes structural CRD validation (apiVersion/kind non-empty) but has no installed CRD,
		// so the reconciler's RESTMapping fails. (Intra-CSD duplicate kinds are impossible by construction.)
		csd := &storagev1alpha1.CustomSnapshotDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: unresolvableName},
			Spec: storagev1alpha1.CustomSnapshotDefinitionSpec{
				APIVersion: "nonexistent.deckhouse.io/v1alpha1",
				Kind:       "NonExistentSnapshot",
				Source: storagev1alpha1.SnapshotGVKRef{
					APIVersion: "nonexistent.deckhouse.io/v1alpha1",
					Kind:       "NonExistentResource",
				},
			},
		}
		Expect(k8sClient.Create(ctx, csd)).To(Succeed())

		Eventually(func(g Gomega) {
			cur := &storagev1alpha1.CustomSnapshotDefinition{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: unresolvableName}, cur)).To(Succeed())
			acc := meta.FindStatusCondition(cur.Status.Conditions, controllers.CSDConditionAccepted)
			g.Expect(acc).NotTo(BeNil())
			g.Expect(acc.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(acc.Reason).To(Equal(controllers.CSDReasonInvalidSpec))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})
})
