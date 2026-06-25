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
	"fmt"
	"os"
	"reflect"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// condManifestsArchived is the subtree-latch contract condition (Commit 3), mirrored onto Snapshot.
const condManifestsArchived = storagev1alpha1.ConditionManifestsArchived

// Hook-managed capture RBAC identifiers — MUST match hooks/go/040-namespace-capture-rbac and
// templates/controller/rbac-for-us.yaml.
const (
	captureRoleBindingName = "d8-state-snapshotter-capture"
	captureClusterRoleName = "d8:state-snapshotter:capture-namespace"
)

// envNSCaptureRework gates the heavy extended specs (temporary CRD discovery, child degradation) that
// need their own tree; the light specs run by default.
const envNSCaptureRework = "E2E_NS_CAPTURE_REWORK"

var (
	roleBindingGVR = schema.GroupVersionResource{
		Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings",
	}
	crdGVR = schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}
	secretGVR = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "secrets",
	}
)

// getRootOwnManifests fetches the root Snapshot's own-node manifests (the namespace-level objects captured
// into the root ManifestCheckpoint) via the core manifests-download subresource, verbatim.
func getRootOwnManifests(ctx context.Context, ns, snap string) ([]unstructured.Unstructured, error) {
	path := coreSnapshotSubPath(ns, snap, subManifestsDownload)
	body, err := aggGet(ctx, path, nil)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	return decodeManifestArray(body)
}

// waitRootArchived waits until the root Snapshot mirrors ManifestsArchived=True.
func waitRootArchived(ctx context.Context, ns, snap string, timeout time.Duration) error {
	return waitObjectCondition(ctx, snapshotGVR, ns, snap, condManifestsArchived, "True", timeout)
}

// applyConfigMap returns a simple namespaced ConfigMap (a guaranteed-included root manifest target).
func configMapObject(ns, name string, data map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"data":       data,
	}}
}

// namespaceCaptureReworkSpecs registers the Commit-5 coverage for the namespace-capture rework: the
// transient RBAC hook lifecycle, discovery inclusion/exclusion, raw secret capture, spec immutability,
// and (env-gated) arbitrary-CR discovery and child degradation with the ManifestsArchived latch.
func namespaceCaptureReworkSpecs() {
	captureRBACHookSpecs()  // E1
	rawSecretsSpecs()       // E4
	inclusionRuleSpecs()    // E5 (reuses the shared captured tree)
	specImmutabilitySpecs() // E6
	arbitraryCRSpecs()      // E2 (env-gated)
	childDegradationSpecs() // E3 (env-gated)
}

// E1 — transient per-namespace RBAC hook (040-namespace-capture-rbac).
func captureRBACHookSpecs() {
	Context("Commit 5 / E1: transient per-namespace capture RBAC", func() {
		It("creates the capture RoleBinding while capturing and removes it once archived", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*suiteCfg.captureReadyTO+2*time.Minute)
			defer cancel()

			ns := uniqueNS("rbac-life")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			By("Applying a capturable ConfigMap and creating the root Snapshot")
			Expect(applyObjects(ctx, []*unstructured.Unstructured{configMapObject(ns, "e1-cm", map[string]interface{}{"a": "b"})}, ns)).To(Succeed())
			Expect(createRootSnapshot(ctx, ns, "e1-snap")).To(Succeed())

			By("Asserting the hook creates the capture RoleBinding while capture is in progress")
			Eventually(func(g Gomega) {
				rb, err := getResource(ctx, roleBindingGVR, ns, captureRoleBindingName)
				g.Expect(err).NotTo(HaveOccurred())
				roleRefName, _, _ := unstructured.NestedString(rb.Object, "roleRef", "name")
				g.Expect(roleRefName).To(Equal(captureClusterRoleName))
				subjects, _, _ := unstructured.NestedSlice(rb.Object, "subjects")
				g.Expect(subjects).NotTo(BeEmpty())
				subjName, _, _ := unstructured.NestedString(subjects[0].(map[string]interface{}), "name")
				g.Expect(subjName).To(Equal("controller"))
			}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(2 * time.Second).Should(Succeed())

			By("Waiting for the root to reach ManifestsArchived=True")
			Expect(waitRootArchived(ctx, ns, "e1-snap", suiteCfg.captureReadyTO)).To(Succeed())

			By("Asserting the hook removes the capture RoleBinding once archived (least privilege restored)")
			assertResourceGone(ctx, roleBindingGVR, ns, captureRoleBindingName, suiteCfg.captureReadyTO)
		})

		It("does not create a capture RoleBinding for import- or static-bind snapshots", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			ns := uniqueNS("rbac-notrigger")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			importSnap := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "storage.deckhouse.io/v1alpha1",
				"kind":       "Snapshot",
				"metadata":   map[string]interface{}{"name": "e1-import", "namespace": ns},
				"spec":       map[string]interface{}{"source": map[string]interface{}{"import": map[string]interface{}{}}},
			}}
			staticSnap := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "storage.deckhouse.io/v1alpha1",
				"kind":       "Snapshot",
				"metadata":   map[string]interface{}{"name": "e1-static", "namespace": ns},
				"spec":       map[string]interface{}{"source": map[string]interface{}{"snapshotContentName": "no-such-content"}},
			}}
			_, err := suiteDyn.Resource(snapshotGVR).Namespace(ns).Create(ctx, importSnap, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = suiteDyn.Resource(snapshotGVR).Namespace(ns).Create(ctx, staticSnap, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Asserting no capture RoleBinding ever appears in the namespace")
			Consistently(func() bool {
				_, err := getResource(ctx, roleBindingGVR, ns, captureRoleBindingName)
				return apierrors.IsNotFound(err)
			}).WithTimeout(20 * time.Second).WithPolling(4 * time.Second).Should(BeTrue())
		})

		It("removes the capture RoleBinding when the only capturing Snapshot is deleted before archive", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			ns := uniqueNS("rbac-del")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			Expect(applyObjects(ctx, []*unstructured.Unstructured{configMapObject(ns, "e1-cm", map[string]interface{}{"a": "b"})}, ns)).To(Succeed())
			Expect(createRootSnapshot(ctx, ns, "e1-del")).To(Succeed())

			By("Waiting for the capture RoleBinding to appear")
			Eventually(func() error {
				_, err := getResource(ctx, roleBindingGVR, ns, captureRoleBindingName)
				return err
			}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(2 * time.Second).Should(Succeed())

			By("Deleting the Snapshot and asserting the RoleBinding is removed")
			Expect(suiteDyn.Resource(snapshotGVR).Namespace(ns).Delete(ctx, "e1-del", metav1.DeleteOptions{})).To(Succeed())
			assertResourceGone(ctx, roleBindingGVR, ns, captureRoleBindingName, 2*time.Minute)
		})
	})
}

// E4 — raw secret capture + denylist.
func rawSecretsSpecs() {
	Context("Commit 5 / E4: raw secret capture and denylist", func() {
		It("captures Opaque/TLS secrets verbatim and excludes denylisted noise", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*suiteCfg.captureReadyTO+2*time.Minute)
			defer cancel()

			ns := uniqueNS("secrets")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			opaque := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Secret",
				"metadata":   map[string]interface{}{"name": "e4-opaque", "namespace": ns},
				"type":       "Opaque",
				"stringData": map[string]interface{}{"key": "s3cr3t-value"},
			}}
			tls := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Secret",
				"metadata":   map[string]interface{}{"name": "e4-tls", "namespace": ns},
				"type":       "kubernetes.io/tls",
				"data": map[string]interface{}{
					// Minimal valid base64 placeholders (kube does not validate cert content for type tls here).
					"tls.crt": "dGxzLWNydA==",
					"tls.key": "dGxzLWtleQ==",
				},
			}}
			saToken := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Secret",
				"metadata": map[string]interface{}{
					"name":        "e4-sa-token",
					"namespace":   ns,
					"annotations": map[string]interface{}{"kubernetes.io/service-account.name": "default"},
				},
				"type": "kubernetes.io/service-account-token",
			}}
			Expect(applyObjects(ctx, []*unstructured.Unstructured{opaque, tls, saToken}, ns)).To(Succeed())

			Expect(createRootSnapshot(ctx, ns, "e4-snap")).To(Succeed())
			_, err := waitSnapshotReady(ctx, ns, "e4-snap", suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitRootArchived(ctx, ns, "e4-snap", suiteCfg.captureReadyTO)).To(Succeed())

			objs, err := getRootOwnManifests(ctx, ns, "e4-snap")
			Expect(err).NotTo(HaveOccurred())

			By("Asserting Opaque and TLS secrets are captured with data byte-for-byte equal to the live object")
			for _, name := range []string{"e4-opaque", "e4-tls"} {
				captured, found := findManifest(objs, "Secret", name)
				Expect(found).To(BeTrue(), "secret %s must be captured verbatim", name)
				live, err := getResource(ctx, secretGVR, ns, name)
				Expect(err).NotTo(HaveOccurred())
				liveData, _, _ := unstructured.NestedMap(live.Object, "data")
				capturedData, _, _ := unstructured.NestedMap(captured.Object, "data")
				Expect(reflect.DeepEqual(liveData, capturedData)).To(BeTrue(), "secret %s data must match live verbatim", name)
			}

			By("Asserting denylisted noise is excluded from the captured root manifests")
			_, hasSAToken := findManifest(objs, "Secret", "e4-sa-token")
			Expect(hasSAToken).To(BeFalse(), "service-account-token secret must be excluded")
			_, hasDefaultSA := findManifest(objs, "ServiceAccount", "default")
			Expect(hasDefaultSA).To(BeFalse(), "default ServiceAccount must be excluded")
			_, hasRootCA := findManifest(objs, "ConfigMap", "kube-root-ca.crt")
			Expect(hasRootCA).To(BeFalse(), "kube-root-ca.crt ConfigMap must be excluded")
		})
	})
}

// E5 — discovery inclusion/exclusion rule on the shared captured tree.
func inclusionRuleSpecs() {
	Context("Commit 5 / E5: discovery inclusion/exclusion rule", func() {
		It("includes ownerless user objects and excludes machinery/noise/owner-managed objects", func() {
			Expect(captured.namespace).NotTo(BeEmpty(), "capture phase must have run first")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			objs, err := getRootOwnManifests(ctx, captured.namespace, captured.rootSnap)
			Expect(err).NotTo(HaveOccurred())

			By("Asserting the ownerless user ConfigMap is included")
			_, found := findManifest(objs, "ConfigMap", srcConfigMapName)
			Expect(found).To(BeTrue(), "ownerless ConfigMap must be included")

			By("Asserting snapshot machinery and own operational kinds never leak into root manifests")
			machineryKinds := []string{
				"Snapshot", "SnapshotContent", "VolumeSnapshot",
				"ManifestCaptureRequest", "VolumeCaptureRequest", "DataExport", "DataImport",
				"DemoVirtualMachineSnapshot", "DemoVirtualDiskSnapshot",
			}
			for i := range objs {
				k := objs[i].GetKind()
				Expect(machineryKinds).NotTo(ContainElement(k), "machinery kind %s leaked into root manifests", k)
				Expect(k).NotTo(Equal("Pod"), "controller-owned Pod must not be captured")
			}

			By("Asserting objects already captured by domain child snapshots are excluded from the root")
			_, hasVM := findManifest(objs, "DemoVirtualMachine", srcVMName)
			Expect(hasVM).To(BeFalse(), "DemoVirtualMachine is captured by its domain snapshot and must be excluded from root")

			By("Asserting noise objects are excluded")
			_, hasDefaultSA := findManifest(objs, "ServiceAccount", "default")
			Expect(hasDefaultSA).To(BeFalse(), "default ServiceAccount must be excluded")
			_, hasRootCA := findManifest(objs, "ConfigMap", "kube-root-ca.crt")
			Expect(hasRootCA).To(BeFalse(), "kube-root-ca.crt ConfigMap must be excluded")
			for i := range objs {
				k := objs[i].GetKind()
				Expect(k).NotTo(Equal("Endpoints"), "Endpoints must be excluded")
				Expect(k).NotTo(Equal("Lease"), "Lease must be excluded")
				Expect(k).NotTo(Equal("Event"), "Event must be excluded")
			}
		})
	})
}

// E6 — Snapshot.spec immutability (CEL self == oldSelf) + ManifestsArchived mirror on the captured root.
func specImmutabilitySpecs() {
	Context("Commit 5 / E6: Snapshot.spec immutability", func() {
		It("rejects any spec update via admission", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			ns := uniqueNS("immutable")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			Expect(createRootSnapshot(ctx, ns, "e6-snap")).To(Succeed())

			By("Attempting to mutate spec.snapshotClassName -> rejected by the CEL immutability rule")
			Eventually(func(g Gomega) {
				cur, err := getResource(ctx, snapshotGVR, ns, "e6-snap")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(unstructured.SetNestedField(cur.Object, "some-class", "spec", "snapshotClassName")).To(Succeed())
				_, updErr := suiteDyn.Resource(snapshotGVR).Namespace(ns).Update(ctx, cur, metav1.UpdateOptions{})
				g.Expect(updErr).To(HaveOccurred(), "spec update must be rejected (immutable)")
			}).WithTimeout(30 * time.Second).WithPolling(3 * time.Second).Should(Succeed())
		})

		It("mirrors ManifestsArchived onto the captured root Snapshot", func() {
			Expect(captured.namespace).NotTo(BeEmpty(), "capture phase must have run first")
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.captureReadyTO+time.Minute)
			defer cancel()
			Expect(waitRootArchived(ctx, captured.namespace, captured.rootSnap, suiteCfg.captureReadyTO)).To(Succeed())
		})
	})
}

// E2 — arbitrary namespaced CR discovery (the headline feature), env-gated (applies a temporary CRD).
func arbitraryCRSpecs() {
	Context("Commit 5 / E2: arbitrary CR discovery", func() {
		It("captures an arbitrary namespaced CR with no CSD mapping via discovery + wildcard RBAC", func() {
			if !envBool(os.Getenv(envNSCaptureRework)) {
				Skip(envNSCaptureRework + " not set: skipping temporary-CRD discovery spec")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*suiteCfg.captureReadyTO+3*time.Minute)
			defer cancel()

			crd := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "apiextensions.k8s.io/v1",
				"kind":       "CustomResourceDefinition",
				"metadata":   map[string]interface{}{"name": "widgetthings.e2e.snapshotter.test"},
				"spec": map[string]interface{}{
					"group": "e2e.snapshotter.test",
					"names": map[string]interface{}{
						"plural":   "widgetthings",
						"singular": "widgetthing",
						"kind":     "WidgetThing",
						"listKind": "WidgetThingList",
					},
					"scope": "Namespaced",
					"versions": []interface{}{map[string]interface{}{
						"name":    "v1",
						"served":  true,
						"storage": true,
						"schema": map[string]interface{}{"openAPIV3Schema": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"spec": map[string]interface{}{"type": "object", "x-kubernetes-preserve-unknown-fields": true},
							},
						}},
					}},
				},
			}}
			Expect(applyObjects(ctx, []*unstructured.Unstructured{crd}, "")).To(Succeed())
			DeferCleanup(func() {
				_ = suiteDyn.Resource(crdGVR).Delete(context.Background(), "widgetthings.e2e.snapshotter.test", metav1.DeleteOptions{})
			})
			Expect(waitObjectCondition(ctx, crdGVR, "", "widgetthings.e2e.snapshotter.test", "Established", "True", 2*time.Minute)).To(Succeed())

			ns := uniqueNS("arbcr")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			widgetGVR := schema.GroupVersionResource{Group: "e2e.snapshotter.test", Version: "v1", Resource: "widgetthings"}
			widget := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "e2e.snapshotter.test/v1",
				"kind":       "WidgetThing",
				"metadata":   map[string]interface{}{"name": "w1", "namespace": ns},
				"spec":       map[string]interface{}{"color": "blue"},
			}}
			_, err := suiteDyn.Resource(widgetGVR).Namespace(ns).Create(ctx, widget, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Expect(createRootSnapshot(ctx, ns, "e2-snap")).To(Succeed())
			_, err = waitSnapshotReady(ctx, ns, "e2-snap", suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())

			objs, err := getRootOwnManifests(ctx, ns, "e2-snap")
			Expect(err).NotTo(HaveOccurred())
			_, found := findManifest(objs, "WidgetThing", "w1")
			Expect(found).To(BeTrue(), "arbitrary CR must be captured via discovery + wildcard RBAC")
		})
	})
}

// E3 — child snapshot degradation keeps the root archived (latch) and does NOT re-grant capture RBAC.
func childDegradationSpecs() {
	Context("Commit 5 / E3: child degradation vs ManifestsArchived latch", func() {
		It("keeps root ManifestsArchived=True and does not re-create capture RBAC on child loss", func() {
			if !envBool(os.Getenv(envNSCaptureRework)) {
				Skip(envNSCaptureRework + " not set: skipping child-degradation spec")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*suiteCfg.captureReadyTO+3*time.Minute)
			defer cancel()

			ns := uniqueNS("degrade")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			Expect(applyObjects(ctx, buildManifestOnlySource(ns), ns)).To(Succeed())
			Expect(createRootSnapshot(ctx, ns, "e3-snap")).To(Succeed())
			rootContent, err := waitSnapshotReady(ctx, ns, "e3-snap", suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, rootContent, suiteCfg.captureReadyTO)).To(Succeed())
			Expect(waitRootArchived(ctx, ns, "e3-snap", suiteCfg.captureReadyTO)).To(Succeed())

			By("Degrading the tree by deleting a child snapshot's bound SnapshotContent")
			nodes, err := walkSnapshotTree(ctx, ns, "e3-snap")
			Expect(err).NotTo(HaveOccurred())
			childNode, ok := firstNodeOfKind(nodes, "DemoVirtualDiskSnapshot")
			Expect(ok).To(BeTrue(), "expected a DemoVirtualDiskSnapshot child")
			childObj, err := getResource(ctx, demoDiskSnapshotGVR, ns, childNode.name)
			Expect(err).NotTo(HaveOccurred())
			childContent, _, _ := unstructured.NestedString(childObj.Object, "status", "boundSnapshotContentName")
			Expect(childContent).NotTo(BeEmpty())
			Expect(suiteDyn.Resource(snapshotContentGVR).Delete(ctx, childContent, metav1.DeleteOptions{})).To(Succeed())

			By("Asserting the root degrades to Ready=False (children leg) but the latch stays True")
			Expect(waitObjectCondition(ctx, snapshotGVR, ns, "e3-snap", condReady, "False", suiteCfg.captureReadyTO)).To(Succeed())
			Consistently(func(g Gomega) {
				root, err := getResource(ctx, snapshotGVR, ns, "e3-snap")
				g.Expect(err).NotTo(HaveOccurred())
				st, _, found := conditionStatus(root, condManifestsArchived)
				g.Expect(found).To(BeTrue())
				g.Expect(st).To(Equal("True"), "ManifestsArchived must remain latched True through degradation")
			}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(Succeed())

			By("Asserting the capture RoleBinding is NOT re-created (RBAC keyed on the latch, not Ready)")
			Consistently(func() bool {
				_, err := getResource(ctx, roleBindingGVR, ns, captureRoleBindingName)
				return apierrors.IsNotFound(err)
			}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(BeTrue())
		})
	})
}
