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

// Source object names of the restore-fidelity fixture: a ServiceAccount, a Role, a RoleBinding whose
// subject points at that ServiceAccount, an Opaque and a TLS Secret, and a ConfigMap. This is a small
// capture of its OWN, not an extension of the shared manifest-only tree: that tree is read by several
// other specs of this phase and deliberately carries only a ConfigMap plus a domain VM, so generic
// objects must not be grafted onto it for one spec.
const (
	restoreFidelitySA           = "restore-sa"
	restoreFidelityRole         = "restore-role"
	restoreFidelityRoleBinding  = "restore-rolebinding"
	restoreFidelityOpaqueSecret = "restore-opaque"
	restoreFidelityTLSSecret    = "restore-tls"
	restoreFidelityConfigMap    = "restore-cm"
	restoreFidelityRootSnapshot = "restore-refs-snap"
)

// The ConfigMap value the fixture captures, and the value its SOURCE object is changed to after the
// capture. The read-stability spec drives that change: it is what makes "the output is compiled from
// the durable checkpoint, not from the live source objects" falsifiable.
const (
	restoreFidelityConfigMapKey  = "demo"
	restoreFidelityCapturedValue = "captured-before-drift"
	restoreFidelityDriftedValue  = "changed-after-capture"
)

// restoreFidelityInventory is the exact set of objects the fixture applies and therefore the set the
// restore output must carry. It is the non-vacuity bound of every assertion below: an empty or
// truncated restore output fails here instead of passing a loop that never iterates.
var restoreFidelityInventory = []struct{ kind, name string }{
	{"ServiceAccount", restoreFidelitySA},
	{"Role", restoreFidelityRole},
	{"RoleBinding", restoreFidelityRoleBinding},
	{"Secret", restoreFidelityOpaqueSecret},
	{"Secret", restoreFidelityTLSSecret},
	{"ConfigMap", restoreFidelityConfigMap},
}

// buildRestoreFidelitySource returns the fixture objects for ns. The RoleBinding subject carries an
// EXPLICIT namespace: that is the field the restore compiler has to follow to the target namespace, and
// a subject without one is left alone by design, so an implicit subject would make the spec vacuous.
func buildRestoreFidelitySource(ns string) []*unstructured.Unstructured {
	serviceAccount := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ServiceAccount",
		"metadata":   map[string]interface{}{"name": restoreFidelitySA, "namespace": ns},
	}}
	role := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "Role",
		"metadata":   map[string]interface{}{"name": restoreFidelityRole, "namespace": ns},
		"rules": []interface{}{map[string]interface{}{
			"apiGroups": []interface{}{""},
			"resources": []interface{}{"configmaps"},
			"verbs":     []interface{}{"get", "list", "watch"},
		}},
	}}
	roleBinding := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "RoleBinding",
		"metadata":   map[string]interface{}{"name": restoreFidelityRoleBinding, "namespace": ns},
		"roleRef": map[string]interface{}{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     "Role",
			"name":     restoreFidelityRole,
		},
		"subjects": []interface{}{map[string]interface{}{
			"kind":      "ServiceAccount",
			"name":      restoreFidelitySA,
			"namespace": ns,
		}},
	}}
	opaque := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]interface{}{"name": restoreFidelityOpaqueSecret, "namespace": ns},
		"type":       "Opaque",
		"stringData": map[string]interface{}{"key": "restore-fidelity-value"},
	}}
	tls := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]interface{}{"name": restoreFidelityTLSSecret, "namespace": ns},
		"type":       "kubernetes.io/tls",
		"data": map[string]interface{}{
			// Minimal valid base64 placeholders (kube does not validate cert content for type tls here).
			"tls.crt": "dGxzLWNydA==",
			"tls.key": "dGxzLWtleQ==",
		},
	}}
	return []*unstructured.Unstructured{
		serviceAccount, role, roleBinding, opaque, tls,
		configMapObject(ns, restoreFidelityConfigMap, map[string]interface{}{
			restoreFidelityConfigMapKey: restoreFidelityCapturedValue,
		}),
	}
}

// restoreFidelitySpecs registers the restore-leg fidelity specs: cross-object namespace references,
// secret payloads, and read stability. They complement the manifest-level restore happy path, which
// proves a ConfigMap and a domain object come back, but cannot see any of the three: a ConfigMap has no
// reference to another object, no payload the sanitizer could touch, and one read cannot show whether a
// second one agrees.
func restoreFidelitySpecs() {
	Context("Manifest-level restore fidelity (cross-references, secrets, read stability)", func() {
		var (
			srcNS     string
			restoreNS string
			// restored is the restore output read by the first spec below and applied by it; the later
			// specs assert against it and against the objects it created.
			restored []unstructured.Unstructured
		)

		BeforeAll(func() {
			srcNS = uniqueNS("p2-restore-refs-src")
			restoreNS = uniqueNS("p2-restore-refs-dst")

			ctx, cancel := context.WithTimeout(context.Background(), 3*suiteCfg.captureReadyTO+2*time.Minute)
			defer cancel()

			By("Creating the source and restore namespaces")
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			Expect(ensureNamespace(ctx, restoreNS)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, srcNS)
				deleteNamespace(cctx, restoreNS)
			})

			By("Applying the fixture (ServiceAccount + Role + RoleBinding + Opaque/TLS Secrets + ConfigMap)")
			Expect(applyObjects(ctx, buildRestoreFidelitySource(srcNS), srcNS)).To(Succeed())

			By("Capturing the root Snapshot and waiting for it to be Ready and archived")
			Expect(createRootSnapshot(ctx, srcNS, restoreFidelityRootSnapshot)).To(Succeed())
			_, err := waitSnapshotReady(ctx, srcNS, restoreFidelityRootSnapshot, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitRootArchived(ctx, srcNS, restoreFidelityRootSnapshot, suiteCfg.captureReadyTO)).To(Succeed())
		})

		It("rewrites the RoleBinding ServiceAccount subject into the restore namespace and applies cleanly there", func() {
			// The namespace rewrite has two independent halves: metadata.namespace, which every emitted
			// object gets, and the kind-specific rewrite of references that live INSIDE the body. A
			// RoleBinding whose subject keeps the source namespace applies without a single error and then
			// grants nothing to the restored ServiceAccount — permissions that are silently dead. Only an
			// assertion on subjects[].namespace catches that. The in-process rewrite itself is unit-tested
			// (TestSanitizeForRestore_RewritesRoleBindingServiceAccountNamespace); what only a live run can
			// show is that the manifest served by the aggregated endpoint still carries the rewrite AND is
			// accepted by the API server with it — hence both probes below.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			By("Reading apply-ready manifests for the restore namespace")
			path := coreSnapshotSubPath(srcNS, restoreFidelityRootSnapshot, subManifestsRestore)
			body, err := aggGet(ctx, path, map[string]string{"targetNamespace": restoreNS})
			Expect(err).NotTo(HaveOccurred(), "GET %s", path)

			restored, err = decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("restore output for %s: %d manifests\n", restoreNS, len(restored))

			By("Asserting the whole fixture is in the restore output (the subject and roleRef targets travel with the binding)")
			for _, want := range restoreFidelityInventory {
				_, found := findManifest(restored, want.kind, want.name)
				Expect(found).To(BeTrue(), "restore output must carry %s/%s", want.kind, want.name)
			}

			By("Applying the restored manifests")
			ptrs := make([]*unstructured.Unstructured, 0, len(restored))
			for i := range restored {
				ptrs = append(ptrs, &restored[i])
			}
			Expect(applyObjects(ctx, ptrs, restoreNS)).To(Succeed())

			By("Asserting the subject namespace followed the move — in the served manifest and on the applied object")
			servedRB, found := findManifest(restored, "RoleBinding", restoreFidelityRoleBinding)
			Expect(found).To(BeTrue())
			appliedRB, err := getResource(ctx, roleBindingGVR, restoreNS, restoreFidelityRoleBinding)
			Expect(err).NotTo(HaveOccurred(), "the restored RoleBinding must exist in %s", restoreNS)

			for _, probe := range []struct {
				what string
				obj  *unstructured.Unstructured
			}{
				{"restore output", servedRB},
				{"applied object", appliedRB},
			} {
				Expect(probe.obj.GetNamespace()).To(Equal(restoreNS),
					"%s: metadata.namespace must be the restore namespace", probe.what)

				subjects, found, err := unstructured.NestedSlice(probe.obj.Object, "subjects")
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(BeTrue(), "%s: the RoleBinding must keep its subjects", probe.what)
				Expect(subjects).To(HaveLen(1), "%s: the fixture binds exactly one subject", probe.what)

				subject, ok := subjects[0].(map[string]interface{})
				Expect(ok).To(BeTrue(), "%s: subject must be an object", probe.what)
				Expect(subject["kind"]).To(Equal("ServiceAccount"), "%s: the subject kind must survive", probe.what)
				Expect(subject["name"]).To(Equal(restoreFidelitySA), "%s: the subject name must survive", probe.what)
				Expect(subject["namespace"]).To(Equal(restoreNS),
					"%s: subjects[].namespace must follow the moved namespace instead of pointing back at the source namespace %s", probe.what, srcNS)
			}
		})

		It("carries Opaque and TLS secret data and type through the restore path unchanged", func() {
			// The sanitizer strips runtime metadata and status but must leave a Secret's data and type
			// alone; the only Secret a capture drops is the service-account-token kind. Capture-side
			// fidelity is pinned by the raw-secret spec in namespace_capture_rbac_test.go, so this spec
			// asserts the delta of the restore leg: the served manifest and the Secret that lands in the
			// target namespace both carry the source bytes. A Secret mangled here restores an object that
			// exists and is unusable — the workload reading it fails, not the restore.
			Expect(restored).NotTo(BeEmpty(), "the preceding spec must have read and applied the restore output")

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			for _, name := range []string{restoreFidelityOpaqueSecret, restoreFidelityTLSSecret} {
				source, err := getResource(ctx, secretGVR, srcNS, name)
				Expect(err).NotTo(HaveOccurred(), "source secret %s must exist", name)
				sourceData, _, _ := unstructured.NestedMap(source.Object, "data")
				Expect(sourceData).NotTo(BeEmpty(), "source secret %s must carry data, otherwise the comparison is vacuous", name)
				sourceType, _, _ := unstructured.NestedString(source.Object, "type")
				Expect(sourceType).NotTo(BeEmpty(), "source secret %s must carry a type", name)

				served, found := findManifest(restored, "Secret", name)
				Expect(found).To(BeTrue(), "restore output must carry secret %s", name)
				servedData, _, _ := unstructured.NestedMap(served.Object, "data")
				Expect(servedData).To(Equal(sourceData),
					"restore output for secret %s must carry the source data byte-for-byte", name)
				servedType, _, _ := unstructured.NestedString(served.Object, "type")
				Expect(servedType).To(Equal(sourceType), "restore output for secret %s must keep its type", name)

				applied, err := getResource(ctx, secretGVR, restoreNS, name)
				Expect(err).NotTo(HaveOccurred(), "the restored secret %s must exist in %s", name, restoreNS)
				appliedData, _, _ := unstructured.NestedMap(applied.Object, "data")
				Expect(appliedData).To(Equal(sourceData),
					"the applied secret %s must carry the source data byte-for-byte", name)
				appliedType, _, _ := unstructured.NestedString(applied.Object, "type")
				Expect(appliedType).To(Equal(sourceType), "the applied secret %s must keep its type", name)
			}
		})

		It("serves the same manifest set on two consecutive reads, from the checkpoint and not from the live source", func() {
			// Two reads of a static cluster prove nothing about WHERE the data comes from: a compiler that
			// listed the live source objects on every call would answer identically both times. So the
			// source ConfigMap is CHANGED between the two reads, and the second read must still serve the
			// captured value — that is the durable-checkpoint property, and a compiler that followed live
			// state fails it. On top of that the pair of reads pins the set contract: identity is
			// apiVersion|kind|name (every object is rewritten into the one target namespace), no object
			// disappears, none is duplicated, and no body drifts between calls, so a client that retries a
			// failed download applies what it inspected. The emission ORDER is not part of the contract and
			// is deliberately not asserted.
			//
			// Limits: this pins the read path against following the live source object; it does not prove
			// stability over hours or across a controller restart, and a compiler that served a cached copy
			// of the live source would still pass (the change is asserted against seconds later, not
			// against a cache that has not caught up).
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			path := coreSnapshotSubPath(srcNS, restoreFidelityRootSnapshot, subManifestsRestore)
			readSet := func() map[string]unstructured.Unstructured {
				body, err := aggGet(ctx, path, map[string]string{"targetNamespace": restoreNS})
				Expect(err).NotTo(HaveOccurred(), "GET %s", path)
				objs, err := decodeManifestArray(body)
				Expect(err).NotTo(HaveOccurred())
				set := map[string]unstructured.Unstructured{}
				for i := range objs {
					key := objs[i].GetAPIVersion() + "|" + objs[i].GetKind() + "|" + objs[i].GetName()
					_, duplicate := set[key]
					Expect(duplicate).To(BeFalse(), "restore output must not repeat object %s", key)
					set[key] = objs[i]
				}
				return set
			}

			first := readSet()

			By("Changing the source ConfigMap after the capture — the restore output must not follow it")
			liveCM, err := getResource(ctx, configMapGVR, srcNS, restoreFidelityConfigMap)
			Expect(err).NotTo(HaveOccurred(), "the source ConfigMap must still exist")
			Expect(unstructured.SetNestedField(liveCM.Object, restoreFidelityDriftedValue, "data", restoreFidelityConfigMapKey)).To(Succeed())
			_, err = suiteDyn.Resource(configMapGVR).Namespace(srcNS).Update(ctx, liveCM, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred(), "changing the source ConfigMap must succeed")

			second := readSet()
			GinkgoWriter.Printf("restore output stability: compared %d manifests across two consecutive reads\n", len(first))

			By("Asserting the second read still serves the captured ConfigMap value, not the live one")
			servedCM, present := second["v1|ConfigMap|"+restoreFidelityConfigMap]
			Expect(present).To(BeTrue(), "the restore output must still carry ConfigMap %s", restoreFidelityConfigMap)
			servedValue, found, err := unstructured.NestedString(servedCM.Object, "data", restoreFidelityConfigMapKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), "the served ConfigMap must carry data.%s", restoreFidelityConfigMapKey)
			Expect(servedValue).To(Equal(restoreFidelityCapturedValue),
				"the restore output must serve the captured value; %q is what the live source object was just changed to", restoreFidelityDriftedValue)

			By("Asserting the two reads agree as a set")
			Expect(len(first)).To(BeNumerically(">=", len(restoreFidelityInventory)),
				"the comparison must cover at least the whole fixture, otherwise two empty reads would agree")
			Expect(second).To(HaveLen(len(first)), "the two reads must return the same number of manifests")
			for key, obj := range first {
				other, present := second[key]
				Expect(present).To(BeTrue(), "manifest %s is missing from the second read", key)
				Expect(other.Object).To(BeComparableTo(obj.Object), "manifest %s differs between two consecutive reads", key)
			}
		})
	})
}
