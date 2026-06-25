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
		ns := newStaticBindNamespace(ctx, "ss-snap-immutable-")
		const snapName = "immutable-snap"

		// Static-bind source keeps the snapshot from kicking off live capture; the referenced content
		// never exists, so the controller just waits non-terminally — irrelevant to admission checks.
		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snapName, Namespace: ns},
			Spec: storagev1alpha1.SnapshotSpec{
				SnapshotClassName: "class-a",
				Source:            &storagev1alpha1.SnapshotSource{SnapshotContentName: "no-such-content-" + ns},
			},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		// Status subresource updates must remain allowed (immutability is spec-only).
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: snapName}, snap)).To(Succeed())
		}).Should(Succeed())
		snap.Status.ManifestCaptureRequestName = "mcr-immutable"
		Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())

		By("rejecting a scalar spec field change (snapshotClassName)")
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: snapName}, snap)).To(Succeed())
		classPatch := snap.DeepCopy()
		classPatch.Spec.SnapshotClassName = "class-b"
		Expect(k8sClient.Update(ctx, classPatch)).NotTo(Succeed())

		By("rejecting a spec.source change")
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: snapName}, snap)).To(Succeed())
		sourcePatch := snap.DeepCopy()
		sourcePatch.Spec.Source = &storagev1alpha1.SnapshotSource{SnapshotContentName: "other-content-" + ns}
		Expect(k8sClient.Update(ctx, sourcePatch)).NotTo(Succeed())

		By("rejecting removal of spec.source (mode switch to dynamic capture)")
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: snapName}, snap)).To(Succeed())
		removePatch := snap.DeepCopy()
		removePatch.Spec.Source = nil
		Expect(k8sClient.Update(ctx, removePatch)).NotTo(Succeed())
	})
})
