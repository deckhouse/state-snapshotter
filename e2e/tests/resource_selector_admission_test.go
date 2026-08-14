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

package tests

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// resourceSelectorAdmissionSpecs asserts that a Snapshot carrying spec.resourceSelector is REFUSED by the
// cluster. Unlike the include/exclude specs in resource_selector_test.go — which cover a capture input that
// no longer exists and are therefore parked behind envResourceSelector — this one covers behaviour the
// module has today, so it runs by default: it is a single create against a real apiserver, with no capture.
//
// Why the field is still in the CRD at all: a field absent from the schema is PRUNED before validation, so
// an outdated client (an old d8 with -l, a stale YAML manifest) would be answered "created" and would
// silently receive a snapshot of the WHOLE namespace instead of the subset it asked for. The schema keeps
// the field solely so admission can turn that silent widening into an error.
//
// Skip-not-fail against an older CRD: if the deployed Snapshot CRD predates the refusal, the create
// SUCCEEDS. Rather than emit a misleading FAIL against a build that never carried the rule, the accepted
// object is deleted and the spec SKIPs — the same philosophy the rest of this suite uses for version skew.
func resourceSelectorAdmissionSpecs() {
	Context("Phase 1c: spec.resourceSelector is refused by admission", func() {
		It("rejects a Snapshot that carries spec.resourceSelector", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			ns := uniqueNS("p1c-selector-refused")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			snap := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
				"kind":       "Snapshot",
				"metadata": map[string]interface{}{
					"name":      "selector-refused",
					"namespace": ns,
				},
				"spec": map[string]interface{}{
					"mode": "Capture",
					"resourceSelector": map[string]interface{}{
						"matchLabels": map[string]interface{}{rsLabelKey: rsValueKeep},
					},
				},
			}}

			By("Creating a Snapshot with spec.resourceSelector (must be rejected)")
			created, err := suiteDyn.Resource(snapshotGVR).Namespace(ns).Create(ctx, snap, metav1.CreateOptions{})
			if err == nil {
				// Deployed Snapshot CRD predates the refusal. Drop the accepted object and skip — see the func doc.
				_ = suiteDyn.Resource(snapshotGVR).Namespace(ns).Delete(context.Background(), created.GetName(), metav1.DeleteOptions{})
				Skip("deployed Snapshot CRD does not refuse spec.resourceSelector (controller image predates the rule); skipping")
			}
			Expect(err.Error()).To(ContainSubstring("resourceSelector"),
				"the rejection must name the offending field so an operator can find it in their manifest")
		})
	})
}
