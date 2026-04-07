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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Smoke test for R1: DSC CRD from repo crds/, client-go scheme, status subresource.
var _ = Describe("Integration: DomainSpecificSnapshotController API smoke", func() {
	It("registers GVK in scheme, CRD is established, create and status update work", func() {
		gvk := schema.GroupVersionKind{
			Group:   storagev1alpha1.APIGroup,
			Version: storagev1alpha1.APIVersion,
			Kind:    "DomainSpecificSnapshotController",
		}
		Expect(scheme.Recognizes(gvk)).To(BeTrue(), "AddToScheme must register DomainSpecificSnapshotController")

		Eventually(func() bool {
			crdObj := &apiextensionsv1.CustomResourceDefinition{}
			if err := k8sClient.Get(ctx, client.ObjectKey{
				Name: "domainspecificsnapshotcontrollers.state-snapshotter.deckhouse.io",
			}, crdObj); err != nil {
				return false
			}
			for _, c := range crdObj.Status.Conditions {
				if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
					return true
				}
			}
			return false
		}).Should(BeTrue(), "DSC CRD from crds/ should become Established")

		dsc := &storagev1alpha1.DomainSpecificSnapshotController{
			ObjectMeta: metav1.ObjectMeta{
				Name: "integration-dsc-smoke",
			},
			Spec: storagev1alpha1.DomainSpecificSnapshotControllerSpec{
				OwnerModule: "integration-test",
				// Deliberately minimal / unrealistic mapping: this test is API plumbing (CRD + status),
				// not snapshotResourceMapping semantics or reconciler validation.
				SnapshotResourceMapping: []storagev1alpha1.SnapshotResourceMappingEntry{
					{
						ResourceCRDName: "testsnapshots.test.deckhouse.io",
						SnapshotCRDName: "testsnapshots.test.deckhouse.io",
						ContentCRDName:  "testsnapshotcontents.test.deckhouse.io",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, dsc)).To(Succeed())

		fetched := &storagev1alpha1.DomainSpecificSnapshotController{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: dsc.Name}, fetched)).To(Succeed())
		observedGen := fetched.GetGeneration()

		fetched.Status.Conditions = []metav1.Condition{
			{
				Type:               "Accepted",
				Status:             metav1.ConditionTrue,
				Reason:             "IntegrationSmoke",
				Message:            "ok",
				LastTransitionTime: metav1.Now(),
				ObservedGeneration: observedGen,
			},
		}
		Expect(k8sClient.Status().Update(ctx, fetched)).To(Succeed())

		after := &storagev1alpha1.DomainSpecificSnapshotController{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: dsc.Name}, after)).To(Succeed())
		Expect(after.Status.Conditions).NotTo(BeEmpty())
		Expect(after.Status.Conditions[0].Type).To(Equal("Accepted"))
		Expect(after.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
		Expect(after.Status.Conditions[0].ObservedGeneration).To(Equal(observedGen))
	})
})
