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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
)

// The snapshot-status contract decision itself (which CRD shapes are a violation and which stay lenient)
// is a pure function with its own unit coverage. What only a real apiserver can show is the wiring around
// it: a CRD served by discovery, read back through the manager's client, and its verdict carried into the
// status the reconciler writes. That path is what this spec pins.
const (
	contractProbeGroup    = "test.deckhouse.io"
	contractProbeVersion  = "v1alpha1"
	contractProbeKind     = "ContractProbeSnapshot"
	contractProbePlural   = "contractprobesnapshots"
	contractProbeCRDName  = contractProbePlural + "." + contractProbeGroup
	contractProbeCSDName  = "integration-csd-contract"
	contractProbeAPIGroup = contractProbeGroup + "/" + contractProbeVersion
)

// contractProbeSnapshotCRD is a snapshot kind that positively violates the framework status contract: its
// structural schema declares a status block, so field absence there is meaningful, but the block omits
// status.boundSnapshotContentName. That is the realistic domain mistake — a status was written, the field
// the generic orchestration reads by fixed name was not.
//
// The kind is referenced by nothing else in the suite, so registering it cannot perturb another spec's
// assertions about registered kinds.
func contractProbeSnapshotCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: contractProbeCRDName},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: contractProbeGroup,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    contractProbeVersion,
				Served:  true,
				Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec": {Type: "object"},
							"status": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"conditions": {
										Type: "array",
										Items: &apiextensionsv1.JSONSchemaPropsOrArray{
											Schema: &apiextensionsv1.JSONSchemaProps{Type: "object"},
										},
									},
								},
							},
						},
					},
				},
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
				},
			}},
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   contractProbePlural,
				Singular: "contractprobesnapshot",
				Kind:     contractProbeKind,
			},
		},
	}
}

var _ = Describe("Integration: CSD reconciler SnapshotContractUnsatisfied", func() {
	AfterEach(func() {
		d := &storagev1alpha1.CustomSnapshotDefinition{}
		d.SetName(contractProbeCSDName)
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, d))
		Eventually(func() bool {
			err := k8sClient.Get(ctx, client.ObjectKey{Name: contractProbeCSDName}, &storagev1alpha1.CustomSnapshotDefinition{})
			return errors.IsNotFound(err)
		}).WithTimeout(15 * time.Second).WithPolling(100 * time.Millisecond).Should(BeTrue())
		// The CRD stays installed on purpose: it is served for the rest of the run like every other test
		// kind, no other spec references it, and removing a CRD mid-run only buys flakiness.
	})

	// Two defects are pinned here, and both would ship silently without this spec.
	//
	// First, the reason has to reach the status at all: the contract verdict is computed on the resolved
	// mapping and could be dropped on the way into the Accepted condition (an early return, a branch that
	// reports the generic InvalidSpec instead). The gate would still be "covered" by its unit tests while
	// every operator saw the wrong cause. Confirmed by mutation: reporting InvalidSpec for a contract
	// failure turns this spec red.
	//
	// Second, the reason belongs on Accepted and NOT on Ready. Ready is the aggregate and carries the
	// generic NotReady with a pointer at Accepted; consumers read the specific cause off Accepted. Moving
	// the reason onto Ready would break that split, so both conditions are asserted together — a test that
	// only looked at Ready would accept the broken layout.
	It("reports the failed snapshot status contract on Accepted, and leaves Ready generic", func() {
		crd := contractProbeSnapshotCRD()
		err := k8sClient.Create(ctx, crd)
		if err != nil && !errors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}

		Eventually(func() bool {
			cur := &apiextensionsv1.CustomResourceDefinition{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: contractProbeCRDName}, cur); err != nil {
				return false
			}
			for _, c := range cur.Status.Conditions {
				if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
					return true
				}
			}
			return false
		}).WithTimeout(30*time.Second).WithPolling(200*time.Millisecond).
			Should(BeTrue(), "the probe snapshot CRD should become Established")

		// The CSD must be created only after discovery serves the kind. Created earlier, the first
		// reconcile would resolve nothing and settle on InvalidSpec, and nothing would re-trigger it.
		probeGVK := schema.GroupVersionKind{Group: contractProbeGroup, Version: contractProbeVersion, Kind: contractProbeKind}
		Eventually(func() error {
			_, err := mgr.GetRESTMapper().RESTMapping(probeGVK.GroupKind(), probeGVK.Version)
			return err
		}).WithTimeout(30*time.Second).WithPolling(200*time.Millisecond).
			Should(Succeed(), "RESTMapper should discover the probe snapshot kind")

		def := &storagev1alpha1.CustomSnapshotDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: contractProbeCSDName},
			Spec: storagev1alpha1.CustomSnapshotDefinitionSpec{
				APIVersion: contractProbeAPIGroup,
				Kind:       contractProbeKind,
				// The source ref points at the same kind: this spec exercises the contract gate, which
				// only ever reads the SNAPSHOT kind's CRD, and the source side just has to resolve.
				Source: storagev1alpha1.SnapshotGVKRef{
					APIVersion: contractProbeAPIGroup,
					Kind:       contractProbeKind,
				},
			},
		}
		Expect(k8sClient.Create(ctx, def)).To(Succeed())

		Eventually(func(g Gomega) {
			cur := &storagev1alpha1.CustomSnapshotDefinition{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contractProbeCSDName}, cur)).To(Succeed())

			acc := meta.FindStatusCondition(cur.Status.Conditions, controllers.CSDConditionAccepted)
			g.Expect(acc).NotTo(BeNil())
			g.Expect(acc.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(acc.Reason).To(Equal(controllers.CSDReasonSnapshotContractUnsatisfied))
			// The message must name the field that is actually missing, otherwise the reason could be
			// right for the wrong reason (any resolution failure reported under the contract label).
			g.Expect(acc.Message).To(ContainSubstring("status.boundSnapshotContentName"))
			g.Expect(acc.ObservedGeneration).To(Equal(cur.GetGeneration()))

			ready := meta.FindStatusCondition(cur.Status.Conditions, controllers.CSDConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal(controllers.CSDReadyReasonNotReady))
			g.Expect(ready.Reason).NotTo(Equal(controllers.CSDReasonSnapshotContractUnsatisfied),
				"the specific cause belongs on Accepted; Ready stays the generic aggregate")
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})
})
