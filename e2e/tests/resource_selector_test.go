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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// resourceSelector e2e fixtures. The label key is namespaced to the e2e suite to avoid clashing with the
// built-in capture exclusions (heritage=deckhouse etc.). Each source carries keep/drop/no-label variants
// so a single source exercises include (matchLabels) and exclude (matchExpressions NotIn) semantics,
// including the "object without the key passes a NotIn selector" edge.
const (
	rsLabelKey  = "snapshot.e2e/group"
	rsValueKeep = "keep"
	rsValueDrop = "drop"

	rsCMKeep    = "cm-keep"
	rsCMDrop    = "cm-drop"
	rsCMNoLabel = "cm-nolabel"
	rsVMKeep    = "vm-keep"
	rsVMDrop    = "vm-drop"
	rsVMNoLabel = "vm-nolabel"

	rsRootInclude = "selector-include"
	rsRootExclude = "selector-exclude"

	// volume-data fixtures (phase-3, env-gated).
	rsVolRoot       = "selector-vol"
	rsVolConfigMap  = "selector-vol-cm"
	rsVolPVCKeep    = "selector-pvc-keep"
	rsVolPVCDrop    = "selector-pvc-drop"
	rsVolProbePod   = "selector-vol-probe"
	rsVMSnapshotKnd = "DemoVirtualMachineSnapshot"
)

// labeledConfigMap builds a ConfigMap optionally carrying the e2e selector label (empty group = no label).
func labeledConfigMap(ns, name, group string) *unstructured.Unstructured {
	meta := map[string]interface{}{"name": name, "namespace": ns}
	if group != "" {
		meta["labels"] = map[string]interface{}{rsLabelKey: group}
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   meta,
		"data":       map[string]interface{}{"demo": "selector"},
	}}
}

// labeledDemoVM builds a manifest-only DemoVirtualMachine (the CSD-expansion leg) optionally carrying the
// e2e selector label. Manifest-only (no virtualDiskName) so it produces a child snapshot without volume data.
func labeledDemoVM(ns, name, group string) *unstructured.Unstructured {
	meta := map[string]interface{}{"name": name, "namespace": ns}
	if group != "" {
		meta["labels"] = map[string]interface{}{rsLabelKey: group}
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualMachine",
		"metadata":   meta,
		"spec":       map[string]interface{}{},
	}}
}

// buildSelectorSource returns the labeled manifest + CSD source: keep/drop/no-label ConfigMaps and
// keep/drop/no-label manifest-only DemoVirtualMachines.
func buildSelectorSource(ns string) []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		labeledConfigMap(ns, rsCMKeep, rsValueKeep),
		labeledConfigMap(ns, rsCMDrop, rsValueDrop),
		labeledConfigMap(ns, rsCMNoLabel, ""),
		labeledDemoVM(ns, rsVMKeep, rsValueKeep),
		labeledDemoVM(ns, rsVMDrop, rsValueDrop),
		labeledDemoVM(ns, rsVMNoLabel, ""),
	}
}

// createRootSnapshotWithSelector creates a root Snapshot whose spec.resourceSelector is built from the
// given matchLabels and/or matchExpressions (either may be empty). It is the selector-aware analogue of
// createRootSnapshot.
func createRootSnapshotWithSelector(ctx context.Context, ns, name string, matchLabels map[string]interface{}, matchExpressions []interface{}) error {
	selector := map[string]interface{}{}
	if len(matchLabels) > 0 {
		selector["matchLabels"] = matchLabels
	}
	if len(matchExpressions) > 0 {
		selector["matchExpressions"] = matchExpressions
	}
	snap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "Snapshot",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"resourceSelector": selector,
		},
	}}
	_, err := suiteDyn.Resource(snapshotGVR).Namespace(ns).Create(ctx, snap, metav1.CreateOptions{})
	return err
}

// resourceSelectorPersisted reports whether the created Snapshot still carries spec.resourceSelector after
// a round-trip through the apiserver. Structural CRD schemas PRUNE unknown fields, so if the deployed
// Snapshot CRD predates this feature the selector is silently dropped on create and capture runs as if
// unfiltered (capture-all). Against such an old controller image the include/exclude assertions would
// FAIL in a way that looks like a feature regression; the specs MUST skip instead so an old image yields
// a clear SKIP rather than a misleading FAIL.
func resourceSelectorPersisted(ctx context.Context, ns, name string) bool {
	obj, err := getResource(ctx, snapshotGVR, ns, name)
	if err != nil {
		return false
	}
	_, found, _ := unstructured.NestedMap(obj.Object, "spec", "resourceSelector")
	return found
}

// vmSnapshotManifestNames downloads the DemoVirtualMachine manifests materialized by each
// DemoVirtualMachineSnapshot node and returns the set of DemoVirtualMachine names found across the tree.
func vmSnapshotManifestNames(ctx context.Context, ns string, nodes []childRef) ([]string, error) {
	var names []string
	for _, n := range nodes {
		if n.kind != rsVMSnapshotKnd {
			continue
		}
		path := coreGenericSubPath(ns, resDemoVMSnapshots, n.name, subManifestsDownload)
		body, err := aggGet(ctx, path, nil)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", path, err)
		}
		objs, err := decodeManifestArray(body)
		if err != nil {
			return nil, err
		}
		for i := range objs {
			if objs[i].GetKind() == "DemoVirtualMachine" {
				names = append(names, objs[i].GetName())
			}
		}
	}
	return names, nil
}

func countNodesOfKind(nodes []childRef, kind string) int {
	n := 0
	for _, node := range nodes {
		if node.kind == kind {
			n++
		}
	}
	return n
}

func containsString(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}

// resourceSelectorSpecs registers the resourceSelector phase: a self-contained flow (its own namespaces
// and root Snapshots) that asserts the selector narrows the manifest, CSD-expansion and (env-gated) PVC
// volume-data legs. It does not touch the shared `captured` tree.
func resourceSelectorSpecs() {
	Context("Phase 1b: resourceSelector include/exclude", func() {
		Context("include via matchLabels", func() {
			var ns string

			BeforeAll(func() {
				ns = uniqueNS("selector-inc")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()

				By("Creating the source namespace " + ns)
				Expect(ensureNamespace(ctx, ns)).To(Succeed())
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
					defer ccancel()
					deleteNamespace(cctx, ns)
				})

				By("Applying the labeled manifest + CSD source")
				Expect(applyObjects(ctx, buildSelectorSource(ns), ns)).To(Succeed())

				By("Creating the root Snapshot with resourceSelector.matchLabels group=keep")
				Expect(createRootSnapshotWithSelector(ctx, ns, rsRootInclude, map[string]interface{}{rsLabelKey: rsValueKeep}, nil)).To(Succeed())

				if !resourceSelectorPersisted(ctx, ns, rsRootInclude) {
					Skip("deployed Snapshot CRD has no spec.resourceSelector (controller image predates the feature); skipping resourceSelector e2e")
				}
			})

			It("the root Snapshot becomes Ready", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
				defer cancel()
				content, err := waitSnapshotReady(ctx, ns, rsRootInclude, suiteCfg.captureReadyTO)
				Expect(err).NotTo(HaveOccurred())
				Expect(content).NotTo(BeEmpty())
			})

			It("manifests-download contains only the selected ConfigMap", func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()

				path := coreSnapshotSubPath(ns, rsRootInclude, subManifestsDownload)
				body, err := aggGet(ctx, path, nil)
				Expect(err).NotTo(HaveOccurred(), "GET %s", path)
				objs, err := decodeManifestArray(body)
				Expect(err).NotTo(HaveOccurred())

				_, keep := findManifest(objs, "ConfigMap", rsCMKeep)
				Expect(keep).To(BeTrue(), "selected ConfigMap %s must be captured", rsCMKeep)
				_, drop := findManifest(objs, "ConfigMap", rsCMDrop)
				Expect(drop).To(BeFalse(), "ConfigMap %s must be excluded by the selector", rsCMDrop)
				_, noLabel := findManifest(objs, "ConfigMap", rsCMNoLabel)
				Expect(noLabel).To(BeFalse(), "unlabeled ConfigMap %s must not match a matchLabels include", rsCMNoLabel)
			})

			It("CSD expansion materializes only the selected DemoVirtualMachine", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
				defer cancel()

				// Gate on the definitive completion signal (SnapshotContent ChildrenReady=True) before
				// asserting an exact count. A plain Eventually(==1) could latch onto a transient partial
				// tree of a broken capture-all controller (keep expanded, drop/no-label not yet), masking
				// the bug. After ChildrenReady the expansion set is final, so the count is stable.
				content, err := waitSnapshotReady(ctx, ns, rsRootInclude, suiteCfg.captureReadyTO)
				Expect(err).NotTo(HaveOccurred())
				Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())

				var nodes []childRef
				Eventually(func(g Gomega) {
					nodes, err = walkSnapshotTree(ctx, ns, rsRootInclude)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(nodes).NotTo(BeEmpty(), "root Snapshot should publish childrenSnapshotRefs once ChildrenReady")
				}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())
				Consistently(func(g Gomega) {
					nodes, err = walkSnapshotTree(ctx, ns, rsRootInclude)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(countNodesOfKind(nodes, rsVMSnapshotKnd)).To(Equal(1), "exactly one DemoVirtualMachine should be expanded")
				}).WithTimeout(10 * time.Second).WithPolling(pollInterval).Should(Succeed())

				names, err := vmSnapshotManifestNames(ctx, ns, nodes)
				Expect(err).NotTo(HaveOccurred())
				Expect(containsString(names, rsVMKeep)).To(BeTrue(), "selected VM %s must be materialized", rsVMKeep)
				Expect(containsString(names, rsVMDrop)).To(BeFalse(), "VM %s must be excluded by the selector", rsVMDrop)
				Expect(containsString(names, rsVMNoLabel)).To(BeFalse(), "unlabeled VM %s must not match a matchLabels include", rsVMNoLabel)
			})
		})

		Context("exclude via matchExpressions (NotIn)", func() {
			var ns string

			BeforeAll(func() {
				ns = uniqueNS("selector-exc")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()

				By("Creating the source namespace " + ns)
				Expect(ensureNamespace(ctx, ns)).To(Succeed())
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
					defer ccancel()
					deleteNamespace(cctx, ns)
				})

				By("Applying the labeled manifest + CSD source")
				Expect(applyObjects(ctx, buildSelectorSource(ns), ns)).To(Succeed())

				By("Creating the root Snapshot with resourceSelector.matchExpressions group NotIn [drop]")
				matchExpressions := []interface{}{
					map[string]interface{}{
						"key":      rsLabelKey,
						"operator": "NotIn",
						"values":   []interface{}{rsValueDrop},
					},
				}
				Expect(createRootSnapshotWithSelector(ctx, ns, rsRootExclude, nil, matchExpressions)).To(Succeed())

				if !resourceSelectorPersisted(ctx, ns, rsRootExclude) {
					Skip("deployed Snapshot CRD has no spec.resourceSelector (controller image predates the feature); skipping resourceSelector e2e")
				}
			})

			It("the root Snapshot becomes Ready", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
				defer cancel()
				content, err := waitSnapshotReady(ctx, ns, rsRootExclude, suiteCfg.captureReadyTO)
				Expect(err).NotTo(HaveOccurred())
				Expect(content).NotTo(BeEmpty())
			})

			It("manifests-download drops only the excluded ConfigMap and keeps unlabeled ones", func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()

				path := coreSnapshotSubPath(ns, rsRootExclude, subManifestsDownload)
				body, err := aggGet(ctx, path, nil)
				Expect(err).NotTo(HaveOccurred(), "GET %s", path)
				objs, err := decodeManifestArray(body)
				Expect(err).NotTo(HaveOccurred())

				_, drop := findManifest(objs, "ConfigMap", rsCMDrop)
				Expect(drop).To(BeFalse(), "ConfigMap %s must be excluded by NotIn [drop]", rsCMDrop)
				_, keep := findManifest(objs, "ConfigMap", rsCMKeep)
				Expect(keep).To(BeTrue(), "ConfigMap %s (group!=drop) must be captured", rsCMKeep)
				_, noLabel := findManifest(objs, "ConfigMap", rsCMNoLabel)
				Expect(noLabel).To(BeTrue(), "unlabeled ConfigMap %s must pass a NotIn selector", rsCMNoLabel)
			})

			It("CSD expansion drops only the excluded DemoVirtualMachine and keeps unlabeled ones", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
				defer cancel()

				// Gate on ChildrenReady=True before asserting the exact count so a transient partial tree
				// cannot satisfy Equal(2) early (see the include leg for the rationale).
				content, err := waitSnapshotReady(ctx, ns, rsRootExclude, suiteCfg.captureReadyTO)
				Expect(err).NotTo(HaveOccurred())
				Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())

				var nodes []childRef
				Eventually(func(g Gomega) {
					nodes, err = walkSnapshotTree(ctx, ns, rsRootExclude)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(nodes).NotTo(BeEmpty(), "root Snapshot should publish childrenSnapshotRefs once ChildrenReady")
				}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())
				Consistently(func(g Gomega) {
					nodes, err = walkSnapshotTree(ctx, ns, rsRootExclude)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(countNodesOfKind(nodes, rsVMSnapshotKnd)).To(Equal(2), "vm-keep and vm-nolabel should be expanded, vm-drop excluded")
				}).WithTimeout(10 * time.Second).WithPolling(pollInterval).Should(Succeed())

				names, err := vmSnapshotManifestNames(ctx, ns, nodes)
				Expect(err).NotTo(HaveOccurred())
				Expect(containsString(names, rsVMDrop)).To(BeFalse(), "VM %s must be excluded by NotIn [drop]", rsVMDrop)
				Expect(containsString(names, rsVMKeep)).To(BeTrue(), "VM %s (group!=drop) must be materialized", rsVMKeep)
				Expect(containsString(names, rsVMNoLabel)).To(BeTrue(), "unlabeled VM %s must pass a NotIn selector", rsVMNoLabel)
			})
		})

		resourceSelectorVolumeDataSpecs()
	})
}

// labeledSelectorPVC builds a PVC on the provisioned StorageClass carrying the e2e selector label.
func labeledSelectorPVC(ns, name, sc, group string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
			"labels":    map[string]interface{}{rsLabelKey: group},
		},
		"spec": map[string]interface{}{
			"accessModes":      []interface{}{"ReadWriteOnce"},
			"storageClassName": sc,
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{"storage": "500Mi"},
			},
		},
	}}
}

// resourceSelectorVolumeDataSpecs registers the env-gated PVC volume-data selector spec: a kept PVC and a
// dropped PVC, with a matchLabels include selector. It asserts the kept PVC captures a data artifact while
// the dropped PVC is captured nowhere (no dataRef), mirroring the manifest/CSD legs for the volume-data leg.
func resourceSelectorVolumeDataSpecs() {
	Context("Phase 3b: resourceSelector over PVC volume data", func() {
		var (
			ns string
			sc string
		)

		BeforeAll(func() {
			if !suiteCfg.volumeData {
				Skip("E2E_VOLUME_DATA not set: skipping the resourceSelector volume-data spec")
			}
			sc = suiteCfg.storageClass
			ns = uniqueNS("selector-vol")

			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
			defer cancel()

			By("Provisioning a thin, snapshot-capable StorageClass via storage-e2e (" + sc + ")")
			_, err := testkit.EnsureDefaultStorageClass(ctx, suiteRestCfg, testkit.DefaultStorageClassConfig{
				StorageClassName:     sc,
				LVMType:              "Thin",
				ThinPoolName:         "thinpool",
				BaseKubeconfig:       suiteClusterResources.BaseKubeconfig,
				VMNamespace:          suiteCfg.vmNamespace,
				BaseStorageClassName: suiteCfg.baseStorageClass,
			})
			Expect(err).NotTo(HaveOccurred(), "provision default StorageClass")

			By("Wiring the StorageClass to a VolumeSnapshotClass for the local CSI driver")
			Expect(ensureStorageClassVolumeSnapshotClass(ctx, sc)).To(Succeed())

			By("Creating the source namespace and applying ConfigMap + labeled keep/drop PVCs")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			source := []*unstructured.Unstructured{
				labeledConfigMap(ns, rsVolConfigMap, rsValueKeep),
				labeledSelectorPVC(ns, rsVolPVCKeep, sc, rsValueKeep),
				labeledSelectorPVC(ns, rsVolPVCDrop, sc, rsValueDrop),
			}
			Expect(applyObjects(ctx, source, ns)).To(Succeed())

			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, ns)
			})

			By("Starting a probe Pod to bind both PVCs (WaitForFirstConsumer)")
			_, err = suiteClientset.CoreV1().Pods(ns).Create(ctx, probePodSpec(ns, rsVolProbePod, []string{rsVolPVCKeep, rsVolPVCDrop}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create probe pod")
			Expect(waitPodRunning(ctx, ns, rsVolProbePod, 10*time.Minute)).To(Succeed())

			By("Creating the root Snapshot with resourceSelector.matchLabels group=keep")
			Expect(createRootSnapshotWithSelector(ctx, ns, rsVolRoot, map[string]interface{}{rsLabelKey: rsValueKeep}, nil)).To(Succeed())

			if !resourceSelectorPersisted(ctx, ns, rsVolRoot) {
				Skip("deployed Snapshot CRD has no spec.resourceSelector (controller image predates the feature); skipping resourceSelector volume-data spec")
			}
		})

		It("captures the kept PVC and never the dropped PVC", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+10*time.Minute)
			defer cancel()

			By("Waiting for the Snapshot + bound SnapshotContent to become Ready")
			content, err := waitSnapshotReady(ctx, ns, rsVolRoot, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())

			By("Waiting for the kept PVC's data binding to be published")
			_, err = waitContentDataRefs(ctx, content, []string{rsVolPVCKeep}, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())

			// The dropped PVC's absence is a negative: a single point-in-time read could miss a dataRef that
			// a broken controller publishes a beat after the kept one. Content is already Ready (DataReady
			// gated above), so assert the dropped PVC stays absent across a short window rather than once.
			By("Asserting the dropped PVC is captured nowhere (no dataRef), and stays absent")
			Consistently(func(g Gomega) {
				bindings, werr := walkContentDataRefs(ctx, content)
				g.Expect(werr).NotTo(HaveOccurred())
				for _, b := range bindings {
					g.Expect(b.pvc).NotTo(Equal(rsVolPVCDrop), "dropped PVC %s must not be captured as volume data", rsVolPVCDrop)
				}
			}).WithTimeout(15 * time.Second).WithPolling(pollInterval).Should(Succeed())
		})
	})
}
