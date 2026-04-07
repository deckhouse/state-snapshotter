//go:build integration
// +build integration

/*
Copyright 2025 Flant JSC

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

// T4 + eligibility-loss: Serial so DSC set does not collide with smoke/hot-add on the same RegistrationTest snapshot kind.
var _ = Describe("Integration: unified runtime RBAC and eligibility", Serial, func() {
	regSnapGVK := schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "RegistrationTestSnapshot"}
	regKey := regSnapGVK.String()

	cleanupDSCNames := []string{
		"integration-t4-no-rbac",
		"integration-eligibility-loss",
		"integration-dsc-smoke",
		"integration-unified-runtime-hot-add",
	}

	BeforeEach(func() {
		for _, name := range cleanupDSCNames {
			d := &storagev1alpha1.DomainSpecificSnapshotController{}
			d.SetName(name)
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, d))
		}
		for _, name := range cleanupDSCNames {
			n := name
			Eventually(func() bool {
				err := k8sClient.Get(ctx, client.ObjectKey{Name: n}, &storagev1alpha1.DomainSpecificSnapshotController{})
				return errors.IsNotFound(err)
			}).WithTimeout(20 * time.Second).WithPolling(100 * time.Millisecond).Should(BeTrue())
		}
	})

	AfterEach(func() {
		for _, name := range cleanupDSCNames {
			d := &storagev1alpha1.DomainSpecificSnapshotController{}
			d.SetName(name)
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, d))
		}
		for _, name := range cleanupDSCNames {
			n := name
			Eventually(func() bool {
				err := k8sClient.Get(ctx, client.ObjectKey{Name: n}, &storagev1alpha1.DomainSpecificSnapshotController{})
				return errors.IsNotFound(err)
			}).WithTimeout(20 * time.Second).WithPolling(100 * time.Millisecond).Should(BeTrue())
		}
	})

	registrationMapping := func() []storagev1alpha1.SnapshotResourceMappingEntry {
		return []storagev1alpha1.SnapshotResourceMappingEntry{
			{
				ResourceCRDName: "registrationtestsnapshots.test.deckhouse.io",
				SnapshotCRDName: "registrationtestsnapshots.test.deckhouse.io",
				ContentCRDName:  "registrationtestsnapshotcontents.test.deckhouse.io",
			},
		}
	}

	It("does not add watches while DSC is Accepted but RBACReady is unset (T4)", func() {
		Expect(unifiedSyncer).NotTo(BeNil())
		const name = "integration-t4-no-rbac"
		dsc := &storagev1alpha1.DomainSpecificSnapshotController{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: storagev1alpha1.DomainSpecificSnapshotControllerSpec{
				OwnerModule:             "integration-t4",
				SnapshotResourceMapping: registrationMapping(),
			},
		}
		Expect(k8sClient.Create(ctx, dsc)).To(Succeed())

		Eventually(func(g Gomega) {
			cur := &storagev1alpha1.DomainSpecificSnapshotController{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, cur)).To(Succeed())
			acc := meta.FindStatusCondition(cur.Status.Conditions, controllers.DSCConditionAccepted)
			g.Expect(acc).NotTo(BeNil())
			g.Expect(acc.Status).To(Equal(metav1.ConditionTrue))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		// Do not assert on ActiveSnapshotGVKKeys: it is monotonic and may retain keys from other Serial specs.
		Eventually(func(g Gomega) {
			st := unifiedSyncer.LastLayeredState()
			for _, p := range st.EligibleFromDSC {
				g.Expect(p.Snapshot).NotTo(Equal(regSnapGVK),
					"Accepted without RBACReady must not contribute to eligible-from-DSC layer")
			}
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})

	It("drops resolved pair when RBACReady goes false but keeps monotonic active key", func() {
		Expect(unifiedSyncer).NotTo(BeNil())
		const name = "integration-eligibility-loss"
		dsc := &storagev1alpha1.DomainSpecificSnapshotController{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: storagev1alpha1.DomainSpecificSnapshotControllerSpec{
				OwnerModule:             "integration-elig",
				SnapshotResourceMapping: registrationMapping(),
			},
		}
		Expect(k8sClient.Create(ctx, dsc)).To(Succeed())

		Eventually(func(g Gomega) {
			cur := &storagev1alpha1.DomainSpecificSnapshotController{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, cur)).To(Succeed())
			acc := meta.FindStatusCondition(cur.Status.Conditions, controllers.DSCConditionAccepted)
			g.Expect(acc).NotTo(BeNil())
			g.Expect(acc.Status).To(Equal(metav1.ConditionTrue))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		hook := &storagev1alpha1.DomainSpecificSnapshotController{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, hook)).To(Succeed())
		gen := hook.GetGeneration()
		meta.SetStatusCondition(&hook.Status.Conditions, metav1.Condition{
			Type:               controllers.DSCConditionRBACReady,
			Status:             metav1.ConditionTrue,
			Reason:             "IntegrationHook",
			Message:            "eligibility loss test",
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: gen,
		})
		Expect(k8sClient.Status().Update(ctx, hook)).To(Succeed())

		Eventually(func(g Gomega) {
			cur := &storagev1alpha1.DomainSpecificSnapshotController{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, cur)).To(Succeed())
			ready := meta.FindStatusCondition(cur.Status.Conditions, controllers.DSCConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			keys := unifiedSyncer.ActiveSnapshotGVKKeys()
			g.Expect(keys).To(HaveKey(regKey))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, hook)).To(Succeed())
		gen = hook.GetGeneration()
		meta.SetStatusCondition(&hook.Status.Conditions, metav1.Condition{
			Type:               controllers.DSCConditionRBACReady,
			Status:             metav1.ConditionFalse,
			Reason:             "IntegrationRevoke",
			Message:            "simulate RBAC loss",
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: gen,
		})
		Expect(k8sClient.Status().Update(ctx, hook)).To(Succeed())

		Eventually(func(g Gomega) {
			st := unifiedSyncer.LastLayeredState()
			for _, gvk := range st.ResolvedSnapshotGVKs {
				g.Expect(gvk).NotTo(Equal(regSnapGVK), "resolved should drop RegistrationTestSnapshot when DSC no longer watch-eligible")
			}
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		keys := unifiedSyncer.ActiveSnapshotGVKKeys()
		Expect(keys).To(HaveKey(regKey), "monotonic active set retains key after resolved drops (additive model)")
	})
})
