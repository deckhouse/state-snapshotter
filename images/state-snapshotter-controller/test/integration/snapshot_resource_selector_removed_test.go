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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// spec.resourceSelector is a removed input kept in the schema ONLY to be refused. The refusal is the whole
// point of keeping it: a field absent from the schema is PRUNED before validation, so a client still
// sending a selector would be told "created" and would silently receive a snapshot of the entire namespace
// instead of the subset it asked for. These specs pin that the apiserver rejects it instead.
//
// They must go through the apiserver: the rule is CEL on the CRD, and a fake client would bypass it and
// prove nothing. The requests are built as unstructured on purpose — that is how an outdated client (an old
// d8 binary, a stale YAML manifest) sends it, and it keeps the specs honest once the typed stub is finally
// dropped from the Go API.
var _ = Describe("Integration: Snapshot spec.resourceSelector is refused", func() {
	var (
		ns             string
		selectorFields = map[string]interface{}{"matchLabels": map[string]interface{}{"app": "demo"}}
	)

	BeforeEach(func() {
		ctx := context.Background()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ss-snap-selector-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns = nsObj.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
	})

	newSnapshot := func(name, mode string, withSelector bool) *unstructured.Unstructured {
		spec := map[string]interface{}{}
		if mode != "" {
			spec["mode"] = mode
		}
		if withSelector {
			spec["resourceSelector"] = selectorFields
		}
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
			"kind":       "Snapshot",
			"metadata":   map[string]interface{}{"name": name, "namespace": ns},
			"spec":       spec,
		}}
	}

	// Both modes are covered because the rule is deliberately mode-independent: the field is gone from the
	// API outright, not merely "meaningless on Import" as an earlier contract had it.
	DescribeTable("rejects a Snapshot carrying spec.resourceSelector",
		func(name, mode string) {
			err := k8sClient.Create(context.Background(), newSnapshot(name, mode, true))

			Expect(err).To(HaveOccurred(), "a Snapshot carrying spec.resourceSelector must be refused, not silently widened to the whole namespace")
			Expect(err.Error()).To(ContainSubstring("resourceSelector"),
				"the rejection must name the offending field so an operator can find it in their manifest")
		},
		Entry("capture mode (explicit)", "selector-capture", string(storagev1alpha1.SnapshotModeCapture)),
		Entry("import mode", "selector-import", string(storagev1alpha1.SnapshotModeImport)),
		Entry("mode omitted (defaulted to Capture)", "selector-default-mode", ""),
	)

	It("accepts a Snapshot without the field, so the rule refuses only the selector", func() {
		created := newSnapshot("no-selector", string(storagev1alpha1.SnapshotModeCapture), false)

		Expect(k8sClient.Create(context.Background(), created)).To(Succeed(), "the rule must not reject an ordinary Snapshot")
		spec, found, err := unstructured.NestedMap(created.Object, "spec")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(spec).To(HaveKey("mode"))
		Expect(spec).NotTo(HaveKey("resourceSelector"), "an accepted Snapshot must carry no selector at all")
	})
})
