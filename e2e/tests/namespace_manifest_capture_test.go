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

// manifestCaptureRequest builds an MCR object with the given targets. targets is a list of
// {apiVersion, kind, name} maps; a target's namespace is always the MCR's own namespace (ns), so it is not
// set on the target. Used to exercise the admission webhook directly: on a live cluster the domain
// controller creates MCRs, but for admission assertions the suite's cluster-admin client creates them
// straight, so the cluster-scoped `namespaces get` SubjectAccessReview in the accept case passes.
func manifestCaptureRequest(ns, name string, targets []map[string]interface{}) *unstructured.Unstructured {
	t := make([]interface{}, len(targets))
	for i := range targets {
		t[i] = targets[i]
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "ManifestCaptureRequest",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec":       map[string]interface{}{"targets": t},
	}}
}

// namespaceManifestCaptureSpecs covers capturing the namespace's own Namespace object into the root
// Snapshot: the positive download check (the Namespace lands in the root own-manifests, verbatim,
// cluster-scoped) and the MCR admission webhook contract (only the MCR's own Namespace is an allowed
// cluster-scoped target).
func namespaceManifestCaptureSpecs() {
	Context("Namespace object capture", func() {
		It("captures the namespace's own Namespace object into the root Snapshot manifests", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*suiteCfg.captureReadyTO+2*time.Minute)
			defer cancel()

			ns := uniqueNS("nm")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			By("Applying a capturable ConfigMap")
			Expect(applyObjects(ctx, []*unstructured.Unstructured{configMapObject(ns, "nm-cm", map[string]interface{}{"a": "b"})}, ns)).To(Succeed())

			By("Capturing the root Snapshot")
			Expect(createRootSnapshot(ctx, ns, "nm-snap")).To(Succeed())
			_, err := waitSnapshotReady(ctx, ns, "nm-snap", suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitRootArchived(ctx, ns, "nm-snap", suiteCfg.captureReadyTO)).To(Succeed())

			By("Asserting the Namespace object is captured verbatim (cluster-scoped, empty metadata.namespace)")
			objs, err := getRootOwnManifests(ctx, ns, "nm-snap")
			Expect(err).NotTo(HaveOccurred())
			nsObj, found := findManifest(objs, "Namespace", ns)
			Expect(found).To(BeTrue(), "the Namespace object must be captured into the root snapshot manifests")
			Expect(nsObj.GetAPIVersion()).To(Equal("v1"))
			Expect(nsObj.GetName()).To(Equal(ns))
			Expect(nsObj.GetNamespace()).To(Equal(""), "a captured cluster-scoped object carries no namespace")
		})
	})

	Context("ManifestCaptureRequest admission (Namespace target)", func() {
		It("rejects a non-Namespace cluster-scoped target", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			ns := uniqueNS("mcr-cs")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			By("Creating an MCR targeting a cluster-scoped ClusterRole")
			_, err := suiteDyn.Resource(manifestCaptureRequestGVR).Namespace(ns).Create(ctx,
				manifestCaptureRequest(ns, "mcr-cs", []map[string]interface{}{
					{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "name": "cluster-admin"},
				}), metav1.CreateOptions{})
			Expect(err).To(HaveOccurred(), "a non-Namespace cluster-scoped target must be rejected by admission")
			Expect(err.Error()).To(ContainSubstring("cluster-scoped"))
		})

		It("rejects a Namespace target whose name differs from the MCR namespace", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			ns := uniqueNS("mcr-ns-other")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			By("Creating an MCR targeting a foreign Namespace (name != MCR namespace)")
			_, err := suiteDyn.Resource(manifestCaptureRequestGVR).Namespace(ns).Create(ctx,
				manifestCaptureRequest(ns, "mcr-ns-other", []map[string]interface{}{
					{"apiVersion": "v1", "kind": "Namespace", "name": "some-other-ns"},
				}), metav1.CreateOptions{})
			Expect(err).To(HaveOccurred(), "a foreign Namespace target must be rejected by admission")
			Expect(err.Error()).To(ContainSubstring("own namespace"))
		})

		It("accepts a Namespace target whose name equals the MCR namespace", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			ns := uniqueNS("mcr-ns-self")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			By("Creating an MCR targeting its own Namespace (name == MCR namespace)")
			created, err := suiteDyn.Resource(manifestCaptureRequestGVR).Namespace(ns).Create(ctx,
				manifestCaptureRequest(ns, "mcr-ns-self", []map[string]interface{}{
					{"apiVersion": "v1", "kind": "Namespace", "name": ns},
				}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "the MCR's own Namespace is an allowed cluster-scoped target")

			// This MCR is created bare (no owning Snapshot), so the manifest-capture controller will reconcile
			// it and may create a cluster-scoped ManifestCheckpoint. Delete it promptly so no cluster-scoped
			// remnant outlives the namespace teardown below.
			if created != nil {
				DeferCleanup(func() {
					_ = suiteDyn.Resource(manifestCaptureRequestGVR).Namespace(ns).
						Delete(context.Background(), created.GetName(), metav1.DeleteOptions{})
				})
			}
		})
	})
}
