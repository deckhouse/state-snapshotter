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

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

// Smoke: CSD CRD + scheme + reconciler writes Accepted; status subresource + Ready after RBACReady handshake.
// Serial: same RegistrationTest snapshot kind as other CSD specs; avoids KindConflict with parallel nodes.
var _ = Describe("Integration: CustomSnapshotDefinition API smoke", Serial, func() {
	const smokeName = "integration-csd-smoke"

	BeforeEach(func() {
		// Same RegistrationTest snapshot kind as other specs; remove stray CSDs or KindConflict breaks Accepted/Ready.
		for _, name := range []string{
			smokeName,
			"integration-unified-runtime-hot-add",
			"integration-eligibility-loss",
			"integration-t4-no-rbac",
		} {
			d := &storagev1alpha1.CustomSnapshotDefinition{}
			d.SetName(name)
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, d))
		}
		for _, name := range []string{
			smokeName,
			"integration-unified-runtime-hot-add",
			"integration-eligibility-loss",
			"integration-t4-no-rbac",
		} {
			n := name
			Eventually(func() bool {
				err := k8sClient.Get(ctx, client.ObjectKey{Name: n}, &storagev1alpha1.CustomSnapshotDefinition{})
				return errors.IsNotFound(err)
			}).WithTimeout(20 * time.Second).WithPolling(100 * time.Millisecond).Should(BeTrue())
		}
	})

	It("reconciles Accepted from CRD resolution and supports Ready after RBACReady", func() {
		gvk := schema.GroupVersionKind{
			Group:   storagev1alpha1.APIGroup,
			Version: storagev1alpha1.APIVersion,
			Kind:    "CustomSnapshotDefinition",
		}
		Expect(scheme.Recognizes(gvk)).To(BeTrue(), "AddToScheme must register CustomSnapshotDefinition")

		Eventually(func() bool {
			crdObj := &apiextensionsv1.CustomResourceDefinition{}
			if err := k8sClient.Get(ctx, client.ObjectKey{
				Name: "customsnapshotdefinitions.state-snapshotter.deckhouse.io",
			}, crdObj); err != nil {
				return false
			}
			for _, c := range crdObj.Status.Conditions {
				if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
					return true
				}
			}
			return false
		}).Should(BeTrue(), "CSD CRD from crds/ should become Established")

		csd := &storagev1alpha1.CustomSnapshotDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: smokeName,
			},
			Spec: storagev1alpha1.CustomSnapshotDefinitionSpec{
				OwnerModule: "integration-test",
				// RegistrationTest* CRDs: avoids hanging a global TestSnapshot watch (lifecycle tests use direct Reconcile on TestSnapshot).
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
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: csd.Name}, cur)).To(Succeed())
			acc := meta.FindStatusCondition(cur.Status.Conditions, controllers.CSDConditionAccepted)
			g.Expect(acc).NotTo(BeNil())
			g.Expect(acc.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(acc.Reason).To(Equal("Resolved"))
			g.Expect(acc.ObservedGeneration).To(Equal(cur.GetGeneration()))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			cur := &storagev1alpha1.CustomSnapshotDefinition{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: csd.Name}, cur)).To(Succeed())
			ready := meta.FindStatusCondition(cur.Status.Conditions, controllers.CSDConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		hookCSD := &storagev1alpha1.CustomSnapshotDefinition{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: csd.Name}, hookCSD)).To(Succeed())
		gen := hookCSD.GetGeneration()
		meta.SetStatusCondition(&hookCSD.Status.Conditions, metav1.Condition{
			Type:               controllers.CSDConditionRBACReady,
			Status:             metav1.ConditionTrue,
			Reason:             "IntegrationHook",
			Message:            "simulated hook",
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: gen,
		})
		Expect(k8sClient.Status().Update(ctx, hookCSD)).To(Succeed())

		Eventually(func(g Gomega) {
			cur := &storagev1alpha1.CustomSnapshotDefinition{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: csd.Name}, cur)).To(Succeed())
			ready := meta.FindStatusCondition(cur.Status.Conditions, controllers.CSDConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal("Active"))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})
})
