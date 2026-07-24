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
)

// rootManifestCaptured reads the root Snapshot's manifest-leg latch
// (status.captureState.commonController.manifestCaptured), the core-internal monotonic bool that replaced
// the former ManifestsArchived condition. Returns (value, found).
func rootManifestCaptured(obj *unstructured.Unstructured) (captured bool, found bool) {
	return snapshotCommonControllerLatch(obj, "manifestCaptured")
}

// Hook-managed capture RBAC identifiers — MUST match hooks/go/040-namespace-capture-rbac and
// templates/controller/rbac-for-us.yaml.
const (
	captureRoleBindingName = "d8-state-snapshotter-capture"
	captureClusterRoleName = "d8:state-snapshotter:capture-namespace"
)

// envNSCaptureRework is the opt-out flag for the heavy extended specs (temporary CRD discovery, child
// degradation) that need their own tree. They run by default and can be disabled explicitly with
// E2E_NS_CAPTURE_REWORK=false.
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

// waitRootArchived waits until the root Snapshot's manifest-leg latch
// (status.captureState.commonController.manifestCaptured) flips to true.
func waitRootArchived(ctx context.Context, ns, snap string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		obj, err := getResource(ctx, snapshotGVR, ns, snap)
		if err == nil {
			captured, found := rootManifestCaptured(obj)
			if found && captured {
				return nil
			}
			last = fmt.Sprintf("found=%v captured=%v", found, captured)
		} else {
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for Snapshot %s/%s manifestCaptured=true; last: %s", ns, snap, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
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
	captureRBACHookSpecs()    // E1
	rawSecretsSpecs()         // E4
	inclusionRuleSpecs()      // E5 (self-contained: generic + RBAC + domain object inclusion/exclusion)
	specImmutabilitySpecs()   // E6
	eagerShellDeletionSpecs() // Block 0 (eager shell / pre-Planned deletion no-wedge)
	arbitraryCRSpecs()        // E2 (default on; opt-out: E2E_NS_CAPTURE_REWORK=false)
	childDegradationSpecs()   // E3 (default on; opt-out: E2E_NS_CAPTURE_REWORK=false)
}

// Block 0 — eager content shell / pre-Planned deletion. With the eager-shell fix (content-single-writer
// design §9) the SnapshotContent object is created AND bound as soon as the Snapshot exists, decoupled from
// the domain phase>=Planned barrier. A Snapshot deleted while still pre-Planned must NOT wedge on the
// eager shell's parent-protect finalizer: the binder deletion path removes it regardless of capture phase.
// The deterministic pre-Planned timing is pinned by the controller integration test
// (test/integration/snapshot_deletion_test.go); on a live cluster the Planned transition is too fast to
// pin, so this spec asserts the timing-robust no-wedge invariant (create -> immediate delete -> fully GC'd).
func eagerShellDeletionSpecs() {
	Context("Block 0: eager content shell / pre-Planned deletion", func() {
		It("does not wedge a root Snapshot deleted immediately after creation (no finalizer wedge)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			ns := uniqueNS("p1-eager-del")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			By("Applying a capturable ConfigMap")
			Expect(applyObjects(ctx, []*unstructured.Unstructured{configMapObject(ns, "b0-cm", map[string]interface{}{"a": "b"})}, ns)).To(Succeed())

			By("Creating and immediately deleting the root Snapshot (best-effort before Planned)")
			Expect(createRootSnapshot(ctx, ns, "b0-del")).To(Succeed())
			Expect(suiteDyn.Resource(snapshotGVR).Namespace(ns).Delete(ctx, "b0-del", metav1.DeleteOptions{})).To(Succeed())

			By("Asserting the Snapshot is fully removed (the eager content shell's finalizer never wedges deletion)")
			assertResourceGone(ctx, snapshotGVR, ns, "b0-del", 2*time.Minute)
		})
	})
}

// E1 — transient per-namespace RBAC hook (040-namespace-capture-rbac).
func captureRBACHookSpecs() {
	Context("Commit 5 / E1: transient per-namespace capture RBAC", func() {
		It("creates the capture RoleBinding while capturing and removes it once archived", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*suiteCfg.captureReadyTO+2*time.Minute)
			defer cancel()

			ns := uniqueNS("p1-rbac-lifecycle")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			By("Applying a capturable ConfigMap")
			Expect(applyObjects(ctx, []*unstructured.Unstructured{configMapObject(ns, "e1-cm", map[string]interface{}{"a": "b"})}, ns)).To(Succeed())

			// Open the appear-watch BEFORE creating the Snapshot: capture+archive now completes in ~1s, so
			// the transient capture RoleBinding (removed the instant the root reaches ManifestsArchived=True)
			// is reliably missed by an interval poll. A watch opened first cannot lose the ADDED event.
			rbWait, rbStop, err := startAppearWatch(ctx, roleBindingGVR, ns, captureRoleBindingName)
			Expect(err).NotTo(HaveOccurred())
			defer rbStop()

			By("Creating the root Snapshot")
			Expect(createRootSnapshot(ctx, ns, "e1-snap")).To(Succeed())

			By("Asserting the hook creates the capture RoleBinding while capture is in progress")
			rb, err := rbWait(suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			roleRefName, _, _ := unstructured.NestedString(rb.Object, "roleRef", "name")
			Expect(roleRefName).To(Equal(captureClusterRoleName))
			subjects, _, _ := unstructured.NestedSlice(rb.Object, "subjects")
			Expect(subjects).NotTo(BeEmpty())
			subjName, _, _ := unstructured.NestedString(subjects[0].(map[string]interface{}), "name")
			Expect(subjName).To(Equal("controller"))

			By("Waiting for the root to reach ManifestsArchived=True")
			Expect(waitRootArchived(ctx, ns, "e1-snap", suiteCfg.captureReadyTO)).To(Succeed())

			By("Asserting the hook removes the capture RoleBinding once archived (least privilege restored)")
			assertResourceGone(ctx, roleBindingGVR, ns, captureRoleBindingName, suiteCfg.captureReadyTO)
		})

		It("does not create a capture RoleBinding for import snapshots", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			ns := uniqueNS("p1-rbac-import-notrigger")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			importSnap := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
				"kind":       "Snapshot",
				"metadata":   map[string]interface{}{"name": "e1-import", "namespace": ns},
				"spec":       map[string]interface{}{"mode": "Import"},
			}}
			_, err := suiteDyn.Resource(snapshotGVR).Namespace(ns).Create(ctx, importSnap, metav1.CreateOptions{})
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

			ns := uniqueNS("p1-rbac-del-before-archive")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			Expect(applyObjects(ctx, []*unstructured.Unstructured{configMapObject(ns, "e1-cm", map[string]interface{}{"a": "b"})}, ns)).To(Succeed())

			// Open the appear-watch BEFORE creating the Snapshot so the ~1s capture window (the RoleBinding
			// is removed the instant the root reaches ManifestsArchived=True) cannot be missed by polling.
			rbWait, rbStop, err := startAppearWatch(ctx, roleBindingGVR, ns, captureRoleBindingName)
			Expect(err).NotTo(HaveOccurred())
			defer rbStop()

			Expect(createRootSnapshot(ctx, ns, "e1-del")).To(Succeed())

			By("Waiting for the capture RoleBinding to appear")
			_, err = rbWait(suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())

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

			ns := uniqueNS("p1-raw-secrets")
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

// E5 — discovery inclusion/exclusion rule. Self-contained: applies a rich set of ownerless user objects
// (ConfigMap + a non-default ServiceAccount + Role + RoleBinding + headless Service + replicas:0
// Deployment) plus a domain DemoVirtualMachine into its own namespace, captures a root Snapshot, and
// asserts the default-include discovery rule: every ownerless user object lands in the root own-manifests,
// while control-plane noise, controller-owned dependents, snapshot/own machinery, and domain
// subtree-covered objects are excluded.
func inclusionRuleSpecs() {
	Context("Commit 5 / E5: discovery inclusion/exclusion rule", func() {
		It("includes ownerless user objects (RBAC/Service/Deployment/SA/ConfigMap) and excludes machinery/noise/owner-managed/domain-covered objects", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+2*time.Minute)
			defer cancel()

			ns := uniqueNS("p1-discovery-inclusion")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			const (
				e5ConfigMap = "e5-cm"
				e5SA        = "e5-sa"
				e5Role      = "e5-role"
				e5RoleBind  = "e5-rolebinding"
				e5Service   = "e5-svc"
				e5Deploy    = "e5-deploy"
				e5VM        = "e5-vm"
			)

			configMap := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": e5ConfigMap, "namespace": ns},
				"data":       map[string]interface{}{"demo": "tree"},
			}}
			serviceAccount := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ServiceAccount",
				"metadata":   map[string]interface{}{"name": e5SA, "namespace": ns},
			}}
			role := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "rbac.authorization.k8s.io/v1",
				"kind":       "Role",
				"metadata":   map[string]interface{}{"name": e5Role, "namespace": ns},
				"rules": []interface{}{map[string]interface{}{
					"apiGroups": []interface{}{""},
					"resources": []interface{}{"configmaps"},
					"verbs":     []interface{}{"get", "list", "watch"},
				}},
			}}
			roleBinding := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "rbac.authorization.k8s.io/v1",
				"kind":       "RoleBinding",
				"metadata":   map[string]interface{}{"name": e5RoleBind, "namespace": ns},
				"roleRef": map[string]interface{}{
					"apiGroup": "rbac.authorization.k8s.io",
					"kind":     "Role",
					"name":     e5Role,
				},
				"subjects": []interface{}{map[string]interface{}{
					"kind":      "ServiceAccount",
					"name":      e5SA,
					"namespace": ns,
				}},
			}}
			// Headless Service (clusterIP: None): never collides on an allocated ClusterIP; its EndpointSlices
			// are controller-owned and so excluded from the root manifests.
			service := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata":   map[string]interface{}{"name": e5Service, "namespace": ns},
				"spec": map[string]interface{}{
					"clusterIP": "None",
					"selector":  map[string]interface{}{"app": e5Deploy},
					"ports": []interface{}{map[string]interface{}{
						"port":       int64(80),
						"targetPort": int64(80),
					}},
				},
			}}
			// replicas:0 Deployment: a real workload manifest that schedules no Pod (keeps the spec infra-free).
			// The Deployment itself is ownerless (included); its ReplicaSet is controller-owned (excluded).
			deployment := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]interface{}{"name": e5Deploy, "namespace": ns},
				"spec": map[string]interface{}{
					"replicas": int64(0),
					"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": e5Deploy}},
					"template": map[string]interface{}{
						"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": e5Deploy}},
						"spec": map[string]interface{}{
							"containers": []interface{}{map[string]interface{}{
								"name":  "pause",
								"image": "registry.k8s.io/pause:3.9",
							}},
						},
					},
				},
			}}
			// Domain DemoVirtualMachine: captured by its own domain child snapshot, so it must be EXCLUDED
			// from the namespace-root own-manifests (subtree-covered).
			vm := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": demoGroupVersion,
				"kind":       "DemoVirtualMachine",
				"metadata":   map[string]interface{}{"name": e5VM, "namespace": ns},
				"spec":       map[string]interface{}{},
			}}

			By("Applying the rich ownerless user object set plus a domain VM")
			Expect(applyObjects(ctx, []*unstructured.Unstructured{
				configMap, serviceAccount, role, roleBinding, service, deployment, vm,
			}, ns)).To(Succeed())

			By("Capturing the root Snapshot and waiting for the whole tree to be Ready and archived")
			Expect(createRootSnapshot(ctx, ns, "e5-snap")).To(Succeed())
			_, err := waitSnapshotReady(ctx, ns, "e5-snap", suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitRootArchived(ctx, ns, "e5-snap", suiteCfg.captureReadyTO)).To(Succeed())

			objs, err := getRootOwnManifests(ctx, ns, "e5-snap")
			Expect(err).NotTo(HaveOccurred())

			By("Asserting every ownerless user object is included in the root own-manifests")
			for _, want := range []struct{ kind, name string }{
				{"ConfigMap", e5ConfigMap},
				{"ServiceAccount", e5SA},
				{"Role", e5Role},
				{"RoleBinding", e5RoleBind},
				{"Service", e5Service},
				{"Deployment", e5Deploy},
			} {
				_, found := findManifest(objs, want.kind, want.name)
				Expect(found).To(BeTrue(), "ownerless %s/%s must be included in root manifests", want.kind, want.name)
			}

			By("Asserting objects captured by a domain child snapshot are excluded from the root")
			_, hasVM := findManifest(objs, "DemoVirtualMachine", e5VM)
			Expect(hasVM).To(BeFalse(), "DemoVirtualMachine is captured by its domain snapshot and must be excluded from root")

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
				Expect(k).NotTo(Equal("ReplicaSet"), "controller-owned ReplicaSet must not be captured")
			}

			By("Asserting control-plane noise is excluded")
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

			ns := uniqueNS("p1-spec-immutable-neg")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() { deleteNamespace(context.Background(), ns) })

			Expect(createRootSnapshot(ctx, ns, "e6-snap")).To(Succeed())

			By("Attempting to mutate spec.resourceSelector -> rejected by the CEL immutability rule")
			Eventually(func(g Gomega) {
				cur, err := getResource(ctx, snapshotGVR, ns, "e6-snap")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(unstructured.SetNestedStringMap(cur.Object, map[string]string{"app": "mutated"}, "spec", "resourceSelector", "matchLabels")).To(Succeed())
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

// E2 — arbitrary namespaced CR discovery (the headline feature), default-on (applies a temporary CRD).
func arbitraryCRSpecs() {
	Context("Commit 5 / E2: arbitrary CR discovery", func() {
		It("captures an arbitrary namespaced CR with no CSD mapping via discovery + wildcard RBAC", func() {
			if !envEnabledByDefault(os.Getenv(envNSCaptureRework)) {
				Skip(envNSCaptureRework + "=false: skipping temporary-CRD discovery spec")
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

			ns := uniqueNS("p1-arbitrary-cr")
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
			if !envEnabledByDefault(os.Getenv(envNSCaptureRework)) {
				Skip(envNSCaptureRework + "=false: skipping child-degradation spec")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*suiteCfg.captureReadyTO+3*time.Minute)
			defer cancel()

			ns := uniqueNS("p1-child-degrade")
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
			childNode, ok := firstNodeOfKind(nodes, "DemoVirtualMachineSnapshot")
			Expect(ok).To(BeTrue(), "expected a DemoVirtualMachineSnapshot child")
			childObj, err := getResource(ctx, demoVMSnapshotGVR, ns, childNode.name)
			Expect(err).NotTo(HaveOccurred())
			childContent, _, _ := unstructured.NestedString(childObj.Object, "status", "boundSnapshotContentName")
			Expect(childContent).NotTo(BeEmpty())
			// Child SnapshotContent is delete-protected: break-glass before the deliberate deletion so the
			// degradation trigger works under admission enforcement=Deny (harmless annotation under Audit).
			Expect(deleteWithAllowDelete(ctx, snapshotContentGVR, "", childContent)).To(Succeed())

			By("Asserting the root degrades to Ready=False (children leg) but the latch stays True")
			Expect(waitObjectCondition(ctx, snapshotGVR, ns, "e3-snap", condReady, "False", suiteCfg.captureReadyTO)).To(Succeed())
			Consistently(func(g Gomega) {
				root, err := getResource(ctx, snapshotGVR, ns, "e3-snap")
				g.Expect(err).NotTo(HaveOccurred())
				captured, found := rootManifestCaptured(root)
				g.Expect(found).To(BeTrue())
				g.Expect(captured).To(BeTrue(), "manifestCaptured must remain latched true through degradation")
			}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(Succeed())

			By("Asserting the capture RoleBinding is NOT re-created (RBAC keyed on the latch, not Ready)")
			Consistently(func() bool {
				_, err := getResource(ctx, roleBindingGVR, ns, captureRoleBindingName)
				return apierrors.IsNotFound(err)
			}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(BeTrue())
		})
	})
}
