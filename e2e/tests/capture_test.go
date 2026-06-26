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

// Demo source object names (PVC-free manifest-only variant of docs/.../snapshot-tree-demo/01-source.yaml).
const (
	srcConfigMapName  = "demo-snapshot-cm"
	srcVMName         = "vm-1"
	srcDiskVMName     = "disk-vm"
	srcDiskStandalone = "disk-standalone"
	rootSnapshotName  = "demo-tree"
)

// capturedTree holds the shared state produced by captureSpecs (phase 1) and consumed by the
// aggregated-API, restore, import and GC specs that run after it in the Ordered container.
type capturedTree struct {
	namespace   string // source namespace the demo tree was applied into
	rootSnap    string // root Snapshot name
	rootContent string // root SnapshotContent name (status.boundSnapshotContentName)
}

var captured capturedTree

// captureTL records the capture state-transition timeline for the phase-1 manifest capture (started in
// BeforeAll, stopped in AfterAll) so bottlenecks in snapshot creation are visible in the test log.
var captureTL *captureTimeline

// buildManifestOnlySource returns the PVC-free demo source: a ConfigMap (manifest leg) plus the demo
// inventory (one VM owning a disk, plus a standalone disk). The disks set spec.size (required by the CEL
// scratch-provisioning rule) but leave spec.persistentVolumeClaimName empty, so the disk controller keeps
// them manifest-only (no PVC). This yields a pure manifest tree where VolumesReady is vacuously true.
func buildManifestOnlySource(ns string) []*unstructured.Unstructured {
	configMap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      srcConfigMapName,
			"namespace": ns,
		},
		"data": map[string]interface{}{"demo": "tree"},
	}}
	vm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualMachine",
		"metadata": map[string]interface{}{
			"name":      srcVMName,
			"namespace": ns,
		},
		"spec": map[string]interface{}{"virtualDiskName": srcDiskVMName},
	}}
	diskVM := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualDisk",
		"metadata": map[string]interface{}{
			"name":      srcDiskVMName,
			"namespace": ns,
		},
		"spec": map[string]interface{}{"size": "1Gi"},
	}}
	diskStandalone := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualDisk",
		"metadata": map[string]interface{}{
			"name":      srcDiskStandalone,
			"namespace": ns,
		},
		"spec": map[string]interface{}{"size": "1Gi"},
	}}
	return []*unstructured.Unstructured{configMap, vm, diskVM, diskStandalone}
}

// createRootSnapshot creates the empty-spec root Snapshot (dynamic discovery of manifests + demo tree).
func createRootSnapshot(ctx context.Context, ns, name string) error {
	snap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "storage.deckhouse.io/v1alpha1",
		"kind":       "Snapshot",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
		},
		"spec": map[string]interface{}{},
	}}
	_, err := suiteDyn.Resource(snapshotGVR).Namespace(ns).Create(ctx, snap, metav1.CreateOptions{})
	return err
}

// captureSpecs registers the phase-1 capture specs: apply a PVC-free demo source, create the root
// Snapshot, and assert the whole snapshot tree reaches Ready.
func captureSpecs() {
	Context("Phase 1: manifest-only capture", func() {
		BeforeAll(func() {
			captured.namespace = uniqueNS("src")
			captured.rootSnap = rootSnapshotName

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			By("Creating the source namespace " + captured.namespace)
			Expect(ensureNamespace(ctx, captured.namespace)).To(Succeed())

			// Start the background capture timeline before applying the source so the demo-CR reconcile and
			// the whole snapshot creation chain are timed (see AfterAll for stop).
			captureTL = startCaptureTimeline(captured.namespace)

			By("Applying the PVC-free demo source (ConfigMap + demo VM/disks)")
			Expect(applyObjects(ctx, buildManifestOnlySource(captured.namespace), captured.namespace)).To(Succeed())

			By("Creating the root Snapshot " + captured.rootSnap)
			Expect(createRootSnapshot(ctx, captured.namespace, captured.rootSnap)).To(Succeed())
		})

		AfterAll(func() {
			captureTL.stop()
		})

		It("captures the demo snapshot tree (root Snapshot + SnapshotContent Ready)", func() {
			// Snapshot creation (capture) must complete quickly, so both waits below are bounded by the
			// short captureReadyTO (fail fast) instead of the generous restore-path snapshotReadyTO.
			// Budget for the two sequential waits plus a buffer for the intervening GETs.
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
			defer cancel()

			By("Waiting for the root Snapshot to become Ready")
			content, err := waitSnapshotReady(ctx, captured.namespace, captured.rootSnap, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(content).NotTo(BeEmpty())
			captured.rootContent = content
			GinkgoWriter.Printf("root SnapshotContent: %s\n", content)

			By("Waiting for the bound SnapshotContent to reach all leg conditions (Manifests/Volumes/Children/Ready)")
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())
		})

		It("populates the demo child snapshot tree (childrenSnapshotRefs) with Ready nodes", func() {
			// Snapshot creation: bound the tree walk and children readiness by the short captureReadyTO
			// (fail fast) rather than the restore-path snapshotReadyTO.
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
			defer cancel()

			By("Walking status.childrenSnapshotRefs from the root Snapshot")
			var nodes []childRef
			Eventually(func(g Gomega) {
				var err error
				nodes, err = walkSnapshotTree(ctx, captured.namespace, captured.rootSnap)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(nodes).NotTo(BeEmpty(), "root Snapshot should have demo child snapshots")
			}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())

			By("Asserting the demo inventory is represented in the tree")
			kinds := map[string]int{}
			for _, n := range nodes {
				kinds[n.kind]++
				GinkgoWriter.Printf("  child snapshot: %s/%s\n", n.kind, n.name)
			}
			Expect(kinds["DemoVirtualMachineSnapshot"]).To(BeNumerically(">=", 1), "expected a DemoVirtualMachineSnapshot")
			Expect(kinds["DemoVirtualDiskSnapshot"]).To(BeNumerically(">=", 2), "expected disk snapshots for disk-vm + disk-standalone")

			By("Asserting every demo child snapshot is Ready=True")
			Expect(waitChildrenReady(ctx, captured.namespace, nodes, suiteCfg.captureReadyTO)).To(Succeed())
		})
	})
}
