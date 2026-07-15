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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

// Integration counterpart of the SDK unit test TestEnsureManifestCapture_EmptyTargetsFailsClosed.
//
// The "a manifest capture is never empty" invariant is a CEL rule on the ManifestCaptureRequest CRD
// (spec.x-kubernetes-validations: "has(self.targets) && size(self.targets) > 0"), enforced by the
// kube-apiserver during schema validation — BEFORE any validating webhook. A unit test with a fake client
// does no admission, so it cannot exercise CEL; only this envtest (real apiserver + the generated CRD
// loaded from crds/) proves the rule actually rejects an empty capture. The SDK-side ErrEmptyManifest
// guard is the complementary unit-level check (defense in depth).
var _ = Describe("Integration: ManifestCaptureRequest non-empty-targets CEL", func() {
	var nsName string

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "mcr-cel-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName = ns.Name
		Eventually(func(g Gomega) {
			fresh := &corev1.Namespace{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nsName}, fresh)).To(Succeed())
			g.Expect(fresh.Status.Phase).To(Equal(corev1.NamespaceActive))
		}).Should(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})
	})

	It("rejects an MCR with omitted targets", func() {
		mcr := &ssv1alpha1.ManifestCaptureRequest{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsName, Name: "mcr-omitted"},
		}
		err := k8sClient.Create(ctx, mcr)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.targets must list at least one object to capture"))
	})

	It("rejects an MCR with an empty targets list", func() {
		mcr := &ssv1alpha1.ManifestCaptureRequest{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsName, Name: "mcr-empty"},
			Spec:       ssv1alpha1.ManifestCaptureRequestSpec{Targets: []ssv1alpha1.ManifestTarget{}},
		}
		err := k8sClient.Create(ctx, mcr)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.targets must list at least one object to capture"))
	})

	It("accepts an MCR with at least one target", func() {
		mcr := &ssv1alpha1.ManifestCaptureRequest{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsName, Name: "mcr-valid"},
			Spec: ssv1alpha1.ManifestCaptureRequestSpec{
				Targets: []ssv1alpha1.ManifestTarget{
					{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-a"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, mcr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, mcr) })
	})

	It("rejects changing spec.targets after creation (immutable)", func() {
		mcr := &ssv1alpha1.ManifestCaptureRequest{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsName, Name: "mcr-immutable"},
			Spec: ssv1alpha1.ManifestCaptureRequestSpec{
				Targets: []ssv1alpha1.ManifestTarget{
					{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-a"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, mcr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, mcr) })

		// The capture plan is frozen: any change to spec.targets is rejected by the CRD's CEL transition rule.
		mcr.Spec.Targets = []ssv1alpha1.ManifestTarget{
			{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-b"},
		}
		err := k8sClient.Update(ctx, mcr)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.targets is immutable"))
	})
})
