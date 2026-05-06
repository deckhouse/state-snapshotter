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

var _ = Describe("Integration: DSC reconciler InvalidSpec", func() {
	const dupKindName = "integration-dsc-dup-snapshot-kind"

	BeforeEach(func() {
		for _, n := range []string{dupKindName} {
			d := &storagev1alpha1.DomainSpecificSnapshotController{}
			d.SetName(n)
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, d))
		}
	})

	It("sets Accepted=False InvalidSpec when two mappings repeat the same snapshot kind", func() {
		dsc := &storagev1alpha1.DomainSpecificSnapshotController{
			ObjectMeta: metav1.ObjectMeta{Name: dupKindName},
			Spec: storagev1alpha1.DomainSpecificSnapshotControllerSpec{
				OwnerModule: "integration-test",
				SnapshotResourceMapping: []storagev1alpha1.SnapshotResourceMappingEntry{
					{
						ResourceCRDName: "testsnapshots.test.deckhouse.io",
						SnapshotCRDName: "testsnapshots.test.deckhouse.io",
					},
					{
						ResourceCRDName: "testsnapshots.test.deckhouse.io",
						SnapshotCRDName: "testsnapshots.test.deckhouse.io",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, dsc)).To(Succeed())

		Eventually(func(g Gomega) {
			cur := &storagev1alpha1.DomainSpecificSnapshotController{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: dupKindName}, cur)).To(Succeed())
			acc := meta.FindStatusCondition(cur.Status.Conditions, controllers.DSCConditionAccepted)
			g.Expect(acc).NotTo(BeNil())
			g.Expect(acc.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(acc.Reason).To(Equal(controllers.DSCReasonInvalidSpec))
			g.Expect(acc.Message).To(ContainSubstring("duplicate snapshot kind"))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})
})
