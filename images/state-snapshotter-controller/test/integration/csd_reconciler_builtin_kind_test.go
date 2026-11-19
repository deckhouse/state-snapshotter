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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
)

var _ = Describe("Integration: CSD reconciler SnapshotKindNotCustomResource", func() {
	const builtinName = "integration-csd-builtin-snapshot-kind"

	AfterEach(func() {
		d := &storagev1alpha1.CustomSnapshotDefinition{}
		d.SetName(builtinName)
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, d))
	})

	It("sets Accepted=False SnapshotKindNotCustomResource when the snapshot kind is built in", func() {
		csd := &storagev1alpha1.CustomSnapshotDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: builtinName},
			Spec: storagev1alpha1.CustomSnapshotDefinitionSpec{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Source:     storagev1alpha1.SnapshotGVKRef{APIVersion: "v1", Kind: "ConfigMap"},
			},
		}
		Expect(k8sClient.Create(ctx, csd)).To(Succeed())

		Eventually(func(g Gomega) {
			cur := &storagev1alpha1.CustomSnapshotDefinition{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: builtinName}, cur)).To(Succeed())
			acc := meta.FindStatusCondition(cur.Status.Conditions, controllers.CSDConditionAccepted)
			g.Expect(acc).NotTo(BeNil())
			g.Expect(acc.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(acc.Reason).To(Equal(controllers.CSDReasonSnapshotKindNotCustomResource))
			g.Expect(acc.Message).To(ContainSubstring(`"configmaps."`))
			g.Expect(acc.ObservedGeneration).To(Equal(cur.GetGeneration()))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})
})
