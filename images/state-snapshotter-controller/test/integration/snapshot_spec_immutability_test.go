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
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// Snapshot is a one-shot artifact: its whole spec is frozen after creation by a spec-level CEL
// transition rule (self == oldSelf). These contract tests pin that admission behaviour so the
// "manifests captured exactly once, no recapture" invariant cannot silently regress.
var _ = Describe("Integration: Snapshot spec immutability", func() {
	It("rejects any spec change while allowing status updates", func() {
		ctx := context.Background()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ss-snap-immutable-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns := nsObj.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		const snapName = "immutable-snap"

		// A plain Capture-mode snapshot; the admission checks below are independent of the capture actually
		// progressing.
		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snapName, Namespace: ns},
			Spec:       storagev1alpha1.SnapshotSpec{Mode: storagev1alpha1.SnapshotModeCapture},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		// Every leg re-reads inside Eventually: the controller reconciles this Snapshot concurrently, so a
		// Get separated from its Update loses the race on resourceVersion and fails for a reason that has
		// nothing to do with admission.
		key := client.ObjectKey{Namespace: ns, Name: snapName}

		By("allowing status subresource updates (immutability is spec-only)")
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, key, snap)).To(Succeed())
			captured := true
			snap.Status.CaptureState = &storagev1alpha1.CaptureStateStatus{
				CommonController: &storagev1alpha1.CommonControllerCaptureState{ManifestCaptured: &captured},
			}
			g.Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())
		}).Should(Succeed())

		// Asserting merely "an error" would also accept a lost-update conflict, i.e. the spec change might
		// never have reached admission at all. Pin the rejection to Invalid, which is what a CEL refusal is.
		By("rejecting a spec.mode change")
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, key, snap)).To(Succeed())
			modePatch := snap.DeepCopy()
			modePatch.Spec.Mode = storagev1alpha1.SnapshotModeImport
			err := k8sClient.Update(ctx, modePatch)
			g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "want an admission rejection of the spec change, got %v", err)
		}).Should(Succeed())

		// The rule is a TRANSITION rule (self == oldSelf), not a blanket "no UPDATE on this object": an
		// update that leaves the spec untouched must still pass, otherwise labels/annotations/finalizers
		// could never be written on a Snapshot. Without this leg, replacing the rule with an
		// always-reject one would keep the test green while wedging every metadata write.
		By("accepting an update that leaves the spec untouched")
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, key, snap)).To(Succeed())
			metadataPatch := snap.DeepCopy()
			metadataPatch.Labels = map[string]string{"example.com/touched": "true"}
			g.Expect(k8sClient.Update(ctx, metadataPatch)).To(Succeed())
		}).Should(Succeed())
	})
})
