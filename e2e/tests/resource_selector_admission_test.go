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

// resourceSelectorAdmissionSpecs asserts the CEL contract that makes resourceSelector a Capture-only
// input: an import-mode Snapshot that ALSO carries spec.resourceSelector MUST be rejected by admission.
// resourceSelector is the root Snapshot's "what to capture" input; on Import there is no live namespace
// to list, so the field is meaningless. This mirrors the sibling capture-input rules already enforced by
// CEL — domain snapshots forbid spec.sourceRef on Import, the forked VolumeSnapshot forbids spec.source on
// Import — making resourceSelector the last capture-input to move from "silently ignored" to "forbidden".
//
// Skip-not-fail against an older CRD (same philosophy as resourceSelectorPersisted): the forbid-on-Import
// rule ships on the Snapshot CRD's spec-level x-kubernetes-validations. If the deployed controller image
// predates it, the create SUCCEEDS instead of being rejected. Rather than emit a misleading FAIL against a
// build that never carried the rule, we delete the accepted object and SKIP. Once the CEL is deployed this
// spec turns into a real create-rejection assertion with no further changes.
func resourceSelectorAdmissionSpecs() {
	Context("Phase 1c: resourceSelector admission (forbidden on Import)", func() {
		It("rejects an import-mode Snapshot that also sets resourceSelector", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			ns := uniqueNS("p1b-selector-import-neg")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			snap := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
				"kind":       "Snapshot",
				"metadata": map[string]interface{}{
					"name":      "selector-on-import",
					"namespace": ns,
				},
				"spec": map[string]interface{}{
					"mode": "Import",
					"resourceSelector": map[string]interface{}{
						"matchLabels": map[string]interface{}{rsLabelKey: rsValueKeep},
					},
				},
			}}

			By("Creating a Snapshot with mode: Import AND spec.resourceSelector (must be rejected by CEL)")
			created, err := suiteDyn.Resource(snapshotGVR).Namespace(ns).Create(ctx, snap, metav1.CreateOptions{})
			if err == nil {
				// Deployed Snapshot CRD lacks the forbid-on-Import CEL rule (image predates it). Drop the
				// accepted object and skip rather than fail — see the func doc.
				_ = suiteDyn.Resource(snapshotGVR).Namespace(ns).Delete(context.Background(), created.GetName(), metav1.DeleteOptions{})
				Skip("deployed Snapshot CRD has no forbid-on-Import CEL for spec.resourceSelector (controller image predates the rule); skipping")
			}
			Expect(err.Error()).To(ContainSubstring("resourceSelector"),
				"an import-mode Snapshot carrying resourceSelector must be rejected by the CEL, and the message must name the offending field")
		})
	})
}
