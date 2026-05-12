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

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Proof R3-style: CSD becomes watch-eligible → unifiedruntime.Sync adds watches → LayeredGVKState + active keys update.
// RegistrationTest* CRDs (not TestSnapshot): lifecycle tests use direct Reconcile on TestSnapshot; a global TestSnapshot
// watch from this spec would never be torn down (additive watches) and races those tests with 409 conflicts.
// Serial: avoids overlapping with other CSD specs that mutate the same API surface.
var _ = Describe("Integration: unified runtime hot-add (CSD → Sync)", Serial, func() {
	const hotCSDName = "integration-unified-runtime-hot-add"
	// Same name as csd_api_smoke_test: two eligible CSDs with the same snapshot GVK → KindConflict / Accepted=False.
	const integrationSmokeCSDName = "integration-csd-smoke"
	// RBAC/eligibility specs (other Serial Describe) use the same RegistrationTest kind; must be gone or KindConflict breaks this block and smoke.
	const integrationEligibilityCSDName = "integration-eligibility-loss"
	const integrationT4CSDName = "integration-t4-no-rbac"
	snapGVK := schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "RegistrationTestSnapshot"}

	csdNamesToClear := []string{hotCSDName, integrationSmokeCSDName, integrationEligibilityCSDName, integrationT4CSDName}

	BeforeEach(func() {
		for _, name := range csdNamesToClear {
			d := &storagev1alpha1.CustomSnapshotDefinition{}
			d.SetName(name)
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, d))
		}
		for _, name := range csdNamesToClear {
			n := name
			Eventually(func() bool {
				err := k8sClient.Get(ctx, client.ObjectKey{Name: n}, &storagev1alpha1.CustomSnapshotDefinition{})
				return errors.IsNotFound(err)
			}).WithTimeout(15 * time.Second).WithPolling(100 * time.Millisecond).Should(BeTrue())
		}
	})

	AfterEach(func() {
		for _, name := range csdNamesToClear {
			d := &storagev1alpha1.CustomSnapshotDefinition{}
			d.SetName(name)
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, d))
		}
		for _, name := range csdNamesToClear {
			n := name
			Eventually(func() bool {
				err := k8sClient.Get(ctx, client.ObjectKey{Name: n}, &storagev1alpha1.CustomSnapshotDefinition{})
				return errors.IsNotFound(err)
			}).WithTimeout(15 * time.Second).WithPolling(100 * time.Millisecond).Should(BeTrue())
		}
	})

	It("registers watches when CSD becomes eligible and updates layered state", func() {
		Expect(unifiedSyncer).NotTo(BeNil())

		csd := &storagev1alpha1.CustomSnapshotDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: hotCSDName},
			Spec: storagev1alpha1.CustomSnapshotDefinitionSpec{
				OwnerModule: "integration-hot-add",
				SnapshotResourceMapping: []storagev1alpha1.SnapshotResourceMappingEntry{
					{
						ResourceCRDName: "registrationtestsnapshots.test.deckhouse.io",
						SnapshotCRDName: "registrationtestsnapshots.test.deckhouse.io",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, csd)).To(Succeed())

		Eventually(func(g Gomega) {
			cur := &storagev1alpha1.CustomSnapshotDefinition{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: hotCSDName}, cur)).To(Succeed())
			acc := meta.FindStatusCondition(cur.Status.Conditions, controllers.CSDConditionAccepted)
			g.Expect(acc).NotTo(BeNil())
			g.Expect(acc.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(acc.ObservedGeneration).To(Equal(cur.GetGeneration()))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		hook := &storagev1alpha1.CustomSnapshotDefinition{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: hotCSDName}, hook)).To(Succeed())
		gen := hook.GetGeneration()
		meta.SetStatusCondition(&hook.Status.Conditions, metav1.Condition{
			Type:               controllers.CSDConditionRBACReady,
			Status:             metav1.ConditionTrue,
			Reason:             "IntegrationHook",
			Message:            "hot-add test",
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: gen,
		})
		Expect(k8sClient.Status().Update(ctx, hook)).To(Succeed())

		Eventually(func(g Gomega) {
			cur := &storagev1alpha1.CustomSnapshotDefinition{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: hotCSDName}, cur)).To(Succeed())
			ready := meta.FindStatusCondition(cur.Status.Conditions, controllers.CSDConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		key := snapGVK.String()
		Eventually(func(g Gomega) {
			keys := unifiedSyncer.ActiveSnapshotGVKKeys()
			g.Expect(keys).NotTo(BeNil())
			g.Expect(keys).To(HaveKey(key))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			st := unifiedSyncer.LastLayeredState()
			found := false
			for _, gvk := range st.ResolvedSnapshotGVKs {
				if gvk == snapGVK {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "resolved layer should include RegistrationTestSnapshot")
			g.Expect(st.EligibleFromCSD).NotTo(BeEmpty())
			var foundEligible bool
			for _, p := range st.EligibleFromCSD {
				if p.Snapshot == snapGVK {
					foundEligible = true
					break
				}
			}
			g.Expect(foundEligible).To(BeTrue(), "eligible-from-CSD layer should list RegistrationTestSnapshot pair")
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})
})
