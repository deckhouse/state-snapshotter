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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// resourceSelector is a capture-only input: there is no live namespace to list on Import, so the
// spec-level CEL rule (self.mode != 'Import' || !has(self.resourceSelector)) rejects an import-mode
// Snapshot that also carries spec.resourceSelector at CREATE. These contract tests pin that admission
// behaviour AND its symmetry (Capture may carry a selector; Import without a selector is fine), so the
// "capture-input forbidden on Import" invariant — shared with spec.sourceRef on domain snapshots and
// spec.source on the forked VolumeSnapshot — cannot silently regress back to "silently ignored".
var _ = Describe("Integration: Snapshot resourceSelector forbidden on Import", func() {
	var ns string

	BeforeEach(func() {
		ctx := context.Background()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ss-import-selector-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns = nsObj.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
	})

	It("rejects an import-mode Snapshot that also sets resourceSelector", func() {
		ctx := context.Background()
		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "import-with-selector", Namespace: ns},
			Spec: storagev1alpha1.SnapshotSpec{
				Mode:             storagev1alpha1.SnapshotModeImport,
				ResourceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}},
			},
		}
		err := k8sClient.Create(ctx, snap)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected a CEL Invalid rejection, got %v", err)
		Expect(err.Error()).To(ContainSubstring("resourceSelector"),
			"the rejection message must name the offending field")
	})

	It("accepts an import-mode Snapshot without a resourceSelector", func() {
		ctx := context.Background()
		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "import-no-selector", Namespace: ns},
			Spec: storagev1alpha1.SnapshotSpec{
				Mode: storagev1alpha1.SnapshotModeImport,
			},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
	})

	It("accepts a capture-mode Snapshot that sets resourceSelector", func() {
		ctx := context.Background()
		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "capture-with-selector", Namespace: ns},
			Spec: storagev1alpha1.SnapshotSpec{
				Mode:             storagev1alpha1.SnapshotModeCapture,
				ResourceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}},
			},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
	})
})
