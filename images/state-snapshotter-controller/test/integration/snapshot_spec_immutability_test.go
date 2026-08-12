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

		// A plain Capture-mode snapshot with a resourceSelector; admission checks below are independent of
		// the capture actually progressing.
		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snapName, Namespace: ns},
			Spec: storagev1alpha1.SnapshotSpec{
				ResourceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}},
				Mode:             storagev1alpha1.SnapshotModeCapture,
			},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		// Status subresource updates must remain allowed (immutability is spec-only).
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: snapName}, snap)).To(Succeed())
		}).Should(Succeed())
		captured := true
		snap.Status.CaptureState = &storagev1alpha1.CaptureStateStatus{
			CommonController: &storagev1alpha1.CommonControllerCaptureState{ManifestCaptured: &captured},
		}
		Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())

		By("rejecting a scalar spec field change (resourceSelector)")
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: snapName}, snap)).To(Succeed())
		selectorPatch := snap.DeepCopy()
		selectorPatch.Spec.ResourceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "b"}}
		Expect(k8sClient.Update(ctx, selectorPatch)).NotTo(Succeed())

		By("rejecting a spec.mode change")
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: snapName}, snap)).To(Succeed())
		modePatch := snap.DeepCopy()
		modePatch.Spec.Mode = storagev1alpha1.SnapshotModeImport
		Expect(k8sClient.Update(ctx, modePatch)).NotTo(Succeed())
	})
})
