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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// The exclude-veto (state-snapshotter.deckhouse.io/exclude) is honored unconditionally at EVERY level of a
// snapshot tree, ANDed on top of any spec.resourceSelector (api ResolveResourceSelector). resourceSelector
// itself narrows which sources a root captures. These specs cover the two mechanisms together across all
// levels the POC exposes:
//
//   - the root namespace object (a plain ConfigMap),
//   - a domain child (a DemoVirtualMachine),
//   - a grandchild disk (a DemoVirtualDisk owned by a VM),
//   - a VM node's OWN manifest target (the companion Secret the POC materializes per DemoVirtualMachine and
//     the VM snapshot captures ALONGSIDE the VM object itself), and
//   - a PVC (orphan-capture + user-VolumeSnapshot adoption veto),
//
// plus the combo where a source matches the selector AND carries the veto (the veto wins). The domain veto
// outcome is asserted against the exact POC contract committed alongside these specs: the companion Secret
// is a core/v1 Secret whose name is read from DemoVirtualMachine.status.secretRef (never hardcoded), and a
// vetoed source is recorded in the capturing snapshot's status.captureState.domainSpecificController.excludedRefs
// (an ExcludedObjectRef with apiVersion/kind/name).
const envVetoSelector = "E2E_VETO_SELECTOR"

// companionSecret{Kind,APIVersion} are the exact identity the POC uses for the per-VM companion Secret
// (materialization_constants.go: secretKind/secretAPIVersion). The e2e reads it dynamically, so these
// literals pin the wire contract the excludedRefs entry and the manifests-download must carry.
const (
	companionSecretKind       = "Secret"
	companionSecretAPIVersion = "v1"
)

// demoVMSnapshotKnd / demoDiskSnapshotKnd are the child snapshot kinds a DemoVirtualMachine / DemoVirtualDisk
// source expands into (walkSnapshotTree ref kinds). rsVMSnapshotKnd already names the VM kind in
// resource_selector_test.go; the disk kind is spelled out here for the volume-data fixture.
const demoDiskSnapshotKnd = "DemoVirtualDiskSnapshot"

// Fixture A (manifest-only) object names: a rich namespace exercising the veto/selector on the root object,
// the domain VM child, and the VM's own companion-Secret manifest target.
const (
	vsCMPlain      = "cm-plain"        // kept: no veto, no selector label
	vsCMVeto       = "cm-veto"         // exclude-vetoed root ConfigMap
	vsVMPlain      = "vm-plain"        // kept manifest-only VM (companion Secret kept in the VM node)
	vsVMVeto       = "vm-veto"         // exclude-vetoed VM (no child; recorded in the ROOT excludedRefs)
	vsVMSecretVeto = "vm-secret-veto"  // kept VM whose companion Secret is exclude-vetoed at the manifest leg
	vsVMSelVeto    = "vm-sel-veto"     // selector-matching AND exclude-vetoed VM (combo: veto wins)
	vsVMSelKeep    = "vm-sel-keep"     // selector-matching, un-vetoed VM (captured by the selector root)
	vsRootA        = "veto-sel-root-a" // no-selector root over the whole namespace
	vsRootB        = "veto-sel-root-b" // matchLabels selector root (group=keep)
)

// Fixture B (data-backed) object names.
const (
	vsdVMDisk     = "vm-disk"         // VM whose disk is kept (disk child with real data)
	vsdVMDiskVeto = "vm-diskveto"     // VM whose disk is exclude-vetoed (no disk child)
	vsdDiskKept   = "disk-kept"       // kept DemoVirtualDisk (data captured)
	vsdDiskVeto   = "disk-veto"       // exclude-vetoed DemoVirtualDisk (dropped as a VM child)
	vsdPVCKept    = "pvc-kept"        // PVC backing disk-kept
	vsdPVCVeto    = "pvc-veto"        // PVC backing disk-veto (the PVC itself is NOT vetoed)
	vsdPVCOrphan  = "pvc-orphan-veto" // exclude-vetoed orphan PVC (never orphan-captured)
	vsdProbe      = "veto-sel-probe"  // probe Pod binding the PVCs (WaitForFirstConsumer)
	vsdOrphanVS   = "veto-sel-orphan-vs"
	vsdRoot       = "veto-sel-data-root"
)

// --- shared helpers --------------------------------------------------------

// excludedRef mirrors storagev1alpha1.ExcludedObjectRef on the wire (apiVersion/kind/name), decoded from a
// snapshot's status.captureState.domainSpecificController.excludedRefs via the dynamic client.
type excludedRef struct {
	apiVersion string
	kind       string
	name       string
}

// nodeExcludedRefs reads status.captureState.domainSpecificController.excludedRefs off any snapshot object
// (the root Snapshot via snapshotGVR, or a domain node via demoVMSnapshotGVR) — the domain's DIRECT exclusion
// vetoes recorded at that node while enumerating its children / planning its own manifest targets.
func nodeExcludedRefs(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) ([]excludedRef, error) {
	obj, err := getResource(ctx, gvr, ns, name)
	if err != nil {
		return nil, err
	}
	raw, _, _ := unstructured.NestedSlice(obj.Object, "status", "captureState", "domainSpecificController", "excludedRefs")
	out := make([]excludedRef, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		av, _, _ := unstructured.NestedString(m, "apiVersion")
		k, _, _ := unstructured.NestedString(m, "kind")
		n, _, _ := unstructured.NestedString(m, "name")
		out = append(out, excludedRef{apiVersion: av, kind: k, name: n})
	}
	return out, nil
}

// findExcludedRef returns the first excludedRef matching the full {apiVersion, kind, name} identity and
// whether it was found. The exact ExcludedObjectRef contract carries apiVersion (companion Secret -> "v1",
// DemoVirtualMachine/DemoVirtualDisk -> the demo GroupVersion), so matching MUST include it — a wrong
// apiVersion is a contract violation, not a match.
func findExcludedRef(refs []excludedRef, apiVersion, kind, name string) (excludedRef, bool) {
	for _, r := range refs {
		if r.apiVersion == apiVersion && r.kind == kind && r.name == name {
			return r, true
		}
	}
	return excludedRef{}, false
}

// hasExcludedRef reports whether refs contains an entry with the exact {apiVersion, kind, name} identity.
func hasExcludedRef(refs []excludedRef, apiVersion, kind, name string) bool {
	_, ok := findExcludedRef(refs, apiVersion, kind, name)
	return ok
}

// demoVMSecretRefName waits until a DemoVirtualMachine publishes status.secretRef.name (the deterministic
// companion Secret the POC materializes for every VM) and returns that name. The name is NEVER hardcoded:
// the veto specs veto/inspect exactly the Secret the controller published.
func demoVMSecretRefName(ctx context.Context, ns, vmName string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		vm, err := getResource(ctx, demoVMGVR, ns, vmName)
		if err == nil {
			name, _, _ := unstructured.NestedString(vm.Object, "status", "secretRef", "name")
			if name != "" {
				return name, nil
			}
			last = "status.secretRef.name empty"
		} else {
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timeout waiting for DemoVirtualMachine %s/%s status.secretRef.name; last: %s", ns, vmName, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return "", ctx.Err()
		}
	}
}

// labelCompanionSecret reads a DemoVirtualMachine's status.secretRef and patches the exclude veto onto THAT
// companion Secret (a merge patch that only adds the label key, preserving the demo-managed labels the
// controller validates). It returns the Secret name so the caller can assert its exclusion. The veto is set
// BEFORE the root Snapshot is created so the VM snapshot honors it when freezing its manifest targets.
func labelCompanionSecret(ctx context.Context, ns, vmName string) (string, error) {
	secretName, err := demoVMSecretRefName(ctx, ns, vmName, suiteCfg.captureReadyTO)
	if err != nil {
		return "", err
	}
	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, storagev1alpha1.ExcludeLabelKey, "true"))
	if _, err := suiteClientset.CoreV1().Secrets(ns).Patch(ctx, secretName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return "", fmt.Errorf("patch exclude veto onto Secret %s/%s: %w", ns, secretName, err)
	}
	return secretName, nil
}

// demoVMSnapshotManifests downloads the manifests-download payload of a single DemoVirtualMachineSnapshot
// node (the VM object plus its own manifest targets, e.g. the companion Secret).
func demoVMSnapshotManifests(ctx context.Context, ns, nodeName string) ([]unstructured.Unstructured, error) {
	body, err := aggGet(ctx, demoSubPath(ns, resDemoVMSnapshots, nodeName, subManifestsDownload), nil)
	if err != nil {
		return nil, err
	}
	return decodeManifestArray(body)
}

// findVMSnapshotNodeForSource locates the DemoVirtualMachineSnapshot node in a walked tree that captured the
// source DemoVirtualMachine named vmName (matched by scanning each node's manifests-download), returning the
// node ref and its decoded manifests.
func findVMSnapshotNodeForSource(ctx context.Context, ns string, nodes []childRef, vmName string) (childRef, []unstructured.Unstructured, bool, error) {
	for _, n := range nodes {
		if n.kind != rsVMSnapshotKnd {
			continue
		}
		objs, err := demoVMSnapshotManifests(ctx, ns, n.name)
		if err != nil {
			return childRef{}, nil, false, err
		}
		if _, ok := findManifest(objs, "DemoVirtualMachine", vmName); ok {
			return n, objs, true, nil
		}
	}
	return childRef{}, nil, false, nil
}

// diskSnapshotSourceNames returns the set of DemoVirtualDisk source names captured across all
// DemoVirtualDiskSnapshot nodes in a walked tree (mirrors vmSnapshotManifestNames for the disk level).
func diskSnapshotSourceNames(ctx context.Context, ns string, nodes []childRef) ([]string, error) {
	var names []string
	for _, n := range nodes {
		if n.kind != demoDiskSnapshotKnd {
			continue
		}
		body, err := aggGet(ctx, demoSubPath(ns, resDemoDiskSnapshots, n.name, subManifestsDownload), nil)
		if err != nil {
			return nil, err
		}
		objs, err := decodeManifestArray(body)
		if err != nil {
			return nil, err
		}
		for i := range objs {
			if objs[i].GetKind() == "DemoVirtualDisk" {
				names = append(names, objs[i].GetName())
			}
		}
	}
	return names, nil
}

// waitDemoVMReady waits for a DemoVirtualMachine to reach Ready=True (companion Secret materialized +
// status.secretRef published), failing fast on the terminal Failed phase like waitDemoDiskReady.
func waitDemoVMReady(ctx context.Context, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		obj, err := getResource(ctx, demoVMGVR, ns, name)
		if err == nil {
			st, reason, found := conditionStatus(obj, condReady)
			if found && st == "True" {
				return nil
			}
			phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
			if phase == "Failed" {
				return fmt.Errorf("DemoVirtualMachine %s/%s entered terminal Failed phase (Ready.status=%q reason=%q)", ns, name, st, reason)
			}
			last = fmt.Sprintf("phase=%q Ready.status=%q reason=%q", phase, st, reason)
		} else {
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for DemoVirtualMachine %s/%s Ready=True; last: %s", ns, name, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// vetoConfigMap builds a ConfigMap with optional labels (the exclude veto and/or the selector group).
func vetoConfigMap(ns, name string, labels map[string]interface{}) *unstructured.Unstructured {
	meta := map[string]interface{}{"name": name, "namespace": ns}
	if len(labels) > 0 {
		meta["labels"] = labels
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   meta,
		"data":       map[string]interface{}{"demo": "veto"},
	}}
}

// vetoDemoVM builds a DemoVirtualMachine with optional labels; diskName == "" yields a manifest-only VM,
// otherwise it links the VM to a DemoVirtualDisk (spec.virtualDiskName) for the data-backed fixture.
func vetoDemoVM(ns, name, diskName string, labels map[string]interface{}) *unstructured.Unstructured {
	meta := map[string]interface{}{"name": name, "namespace": ns}
	if len(labels) > 0 {
		meta["labels"] = labels
	}
	spec := map[string]interface{}{}
	if diskName != "" {
		spec["virtualDiskName"] = diskName
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualMachine",
		"metadata":   meta,
		"spec":       spec,
	}}
}

// vetoDemoDisk builds a PVC-backed DemoVirtualDisk with optional labels (the exclude veto).
func vetoDemoDisk(ns, name, pvc, sc string, labels map[string]interface{}) *unstructured.Unstructured {
	meta := map[string]interface{}{"name": name, "namespace": ns}
	if len(labels) > 0 {
		meta["labels"] = labels
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualDisk",
		"metadata":   meta,
		"spec": map[string]interface{}{
			"persistentVolumeClaimName": pvc,
			"size":                      "500Mi",
			"storageClassName":          sc,
		},
	}}
}

// --- registration ----------------------------------------------------------

// vetoSelectorSpecs registers the exclude-veto + resourceSelector coverage (default-on opt-out gate
// E2E_VETO_SELECTOR). Fixture A is manifest-only (no volume data); Fixture B is data-backed and additionally
// gated by suiteCfg.volumeData. Both use their own namespaces and root Snapshots (no shared tree).
func vetoSelectorSpecs() {
	Context("Phase 1c/3e: exclude-veto + resourceSelector across tree levels", func() {
		vetoSelectorManifestSpecs()
		vetoSelectorVolumeDataSpecs()
	})
}

// --- Fixture A: manifest-only ----------------------------------------------

func vetoSelectorManifestSpecs() {
	Context("Fixture A: manifest-only veto/selector", func() {
		var (
			ns                 string
			vmPlainSecret      string // companion Secret of vm-plain (kept in the VM node)
			vmSecretVetoSecret string // companion Secret of vm-secret-veto (vetoed at the manifest leg)
			selectorPersisted  bool   // false against a controller image predating spec.resourceSelector
		)

		BeforeAll(func() {
			if !envEnabledByDefault(os.Getenv(envVetoSelector)) {
				Skip(envVetoSelector + "=false: skipping the veto/selector specs (they run by default)")
			}
			ns = uniqueNS("p1c-veto-sel")

			ctx, cancel := context.WithTimeout(context.Background(), 3*suiteCfg.captureReadyTO+10*time.Minute)
			defer cancel()

			By("Creating the source namespace " + ns)
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, ns)
			})

			veto := map[string]interface{}{storagev1alpha1.ExcludeLabelKey: "true"}
			keep := map[string]interface{}{rsLabelKey: rsValueKeep}
			vetoAndKeep := map[string]interface{}{storagev1alpha1.ExcludeLabelKey: "true", rsLabelKey: rsValueKeep}

			By("Applying the rich manifest-only source (plain + vetoed ConfigMaps/VMs + selector combos)")
			source := []*unstructured.Unstructured{
				vetoConfigMap(ns, vsCMPlain, nil),
				vetoConfigMap(ns, vsCMVeto, veto),
				vetoDemoVM(ns, vsVMPlain, "", nil),
				vetoDemoVM(ns, vsVMVeto, "", veto),
				vetoDemoVM(ns, vsVMSecretVeto, "", nil),
				vetoDemoVM(ns, vsVMSelVeto, "", vetoAndKeep),
				vetoDemoVM(ns, vsVMSelKeep, "", keep),
			}
			Expect(applyObjects(ctx, source, ns)).To(Succeed())

			By("Waiting for every DemoVirtualMachine to reach Ready (companion Secret materialized)")
			for _, vm := range []string{vsVMPlain, vsVMVeto, vsVMSecretVeto, vsVMSelVeto, vsVMSelKeep} {
				Expect(waitDemoVMReady(ctx, ns, vm, suiteCfg.captureReadyTO)).To(Succeed(), "DemoVirtualMachine %s Ready", vm)
			}

			By("Recording vm-plain's companion Secret and vetoing vm-secret-veto's companion Secret BEFORE capture")
			var err error
			vmPlainSecret, err = demoVMSecretRefName(ctx, ns, vsVMPlain, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred(), "read %s status.secretRef", vsVMPlain)
			vmSecretVetoSecret, err = labelCompanionSecret(ctx, ns, vsVMSecretVeto)
			Expect(err).NotTo(HaveOccurred(), "veto %s companion Secret", vsVMSecretVeto)

			By("Creating Root A (no selector) and Root B (matchLabels group=keep)")
			Expect(createRootSnapshot(ctx, ns, vsRootA)).To(Succeed())
			Expect(createRootSnapshotWithSelector(ctx, ns, vsRootB, map[string]interface{}{rsLabelKey: rsValueKeep}, nil)).To(Succeed())
			selectorPersisted = resourceSelectorPersisted(ctx, ns, vsRootB)
		})

		It("brings Root A (no selector) to Ready with its content", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
			defer cancel()
			content, err := waitSnapshotReady(ctx, ns, vsRootA, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(content).NotTo(BeEmpty())
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())
		})

		It("keeps un-vetoed objects: cm-plain at the root, vm-plain's companion Secret in the VM node but deduped from the root", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
			defer cancel()

			content, err := waitSnapshotReady(ctx, ns, vsRootA, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())

			By("Locating the vm-plain DemoVirtualMachineSnapshot node in the Root A tree")
			var node childRef
			var nodeObjs []unstructured.Unstructured
			Eventually(func(g Gomega) {
				nodes, werr := walkSnapshotTree(ctx, ns, vsRootA)
				g.Expect(werr).NotTo(HaveOccurred())
				var found bool
				node, nodeObjs, found, werr = findVMSnapshotNodeForSource(ctx, ns, nodes, vsVMPlain)
				g.Expect(werr).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue(), "vm-plain must expand into a DemoVirtualMachineSnapshot child")
			}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())

			By("Asserting the VM node captures its companion Secret alongside the VM object (POC manifest leg)")
			_, hasVMManifest := findManifest(nodeObjs, "DemoVirtualMachine", vsVMPlain)
			Expect(hasVMManifest).To(BeTrue(), "the VM node %s must carry the DemoVirtualMachine %s manifest", node.name, vsVMPlain)
			secretManifest, hasSecret := findManifest(nodeObjs, companionSecretKind, vmPlainSecret)
			Expect(hasSecret).To(BeTrue(), "the VM node %s must carry the companion Secret %s in its manifest leg", node.name, vmPlainSecret)
			Expect(secretManifest.GetAPIVersion()).To(Equal(companionSecretAPIVersion),
				"the kept companion Secret %s manifest must carry the core/v1 apiVersion (exact wire contract)", vmPlainSecret)

			By("Asserting the root own-manifests keep cm-plain but drop the VM and its subtree-captured Secret (coverage-dedup)")
			body, err := aggGet(ctx, coreSnapshotSubPath(ns, vsRootA, subManifestsDownload), nil)
			Expect(err).NotTo(HaveOccurred())
			rootObjs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			_, hasCMPlain := findManifest(rootObjs, "ConfigMap", vsCMPlain)
			Expect(hasCMPlain).To(BeTrue(), "un-vetoed ConfigMap %s must be captured at the root", vsCMPlain)
			_, rootHasVM := findManifest(rootObjs, "DemoVirtualMachine", vsVMPlain)
			Expect(rootHasVM).To(BeFalse(), "VM %s is captured by its domain child; the root must not re-capture it", vsVMPlain)
			_, rootHasSecret := findManifest(rootObjs, companionSecretKind, vmPlainSecret)
			Expect(rootHasSecret).To(BeFalse(), "companion Secret %s is captured by the VM node; coverage-dedup must drop it from the root", vmPlainSecret)
		})

		It("vetoes the root ConfigMap and the domain VM: absent from capture and recorded in the root excludedRefs", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
			defer cancel()

			content, err := waitSnapshotReady(ctx, ns, vsRootA, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())

			By("Asserting the vetoed VM produced no DemoVirtualMachineSnapshot child")
			var nodes []childRef
			Eventually(func(g Gomega) {
				nodes, err = walkSnapshotTree(ctx, ns, vsRootA)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(nodes).NotTo(BeEmpty(), "Root A must publish its child snapshot tree")
			}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())
			vmNames, err := vmSnapshotManifestNames(ctx, ns, nodes)
			Expect(err).NotTo(HaveOccurred())
			Expect(containsString(vmNames, vsVMVeto)).To(BeFalse(), "exclude-vetoed VM %s must not be captured", vsVMVeto)

			By("Asserting the vetoed VM is recorded in the ROOT domainSpecificController.excludedRefs (exact {apiVersion,kind,name})")
			refs, err := nodeExcludedRefs(ctx, snapshotGVR, ns, vsRootA)
			Expect(err).NotTo(HaveOccurred())
			Expect(hasExcludedRef(refs, demoGroupVersion, "DemoVirtualMachine", vsVMVeto)).To(BeTrue(),
				"vetoed VM %s must be recorded in the root excludedRefs as {apiVersion=%s, kind=DemoVirtualMachine}", vsVMVeto, demoGroupVersion)

			By("Asserting the vetoed ConfigMap is absent from the root own-manifests")
			body, err := aggGet(ctx, coreSnapshotSubPath(ns, vsRootA, subManifestsDownload), nil)
			Expect(err).NotTo(HaveOccurred())
			rootObjs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			_, hasCMVeto := findManifest(rootObjs, "ConfigMap", vsCMVeto)
			Expect(hasCMVeto).To(BeFalse(), "exclude-vetoed ConfigMap %s must be dropped from the root manifests", vsCMVeto)
		})

		It("vetoes a VM's companion Secret at the manifest leg: absent from the VM node and recorded in the VM snapshot excludedRefs", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
			defer cancel()

			content, err := waitSnapshotReady(ctx, ns, vsRootA, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())

			By("Locating the vm-secret-veto DemoVirtualMachineSnapshot node (the VM itself is kept)")
			var node childRef
			var nodeObjs []unstructured.Unstructured
			Eventually(func(g Gomega) {
				nodes, werr := walkSnapshotTree(ctx, ns, vsRootA)
				g.Expect(werr).NotTo(HaveOccurred())
				var found bool
				node, nodeObjs, found, werr = findVMSnapshotNodeForSource(ctx, ns, nodes, vsVMSecretVeto)
				g.Expect(werr).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue(), "vm-secret-veto must still expand into a DemoVirtualMachineSnapshot child (only its Secret is vetoed)")
			}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())

			By("Asserting the vetoed companion Secret is dropped from the VM node manifests but the VM itself remains")
			_, hasVMManifest := findManifest(nodeObjs, "DemoVirtualMachine", vsVMSecretVeto)
			Expect(hasVMManifest).To(BeTrue(), "the VM object itself must always be captured (never vetoed at this level)")
			_, hasSecret := findManifest(nodeObjs, companionSecretKind, vmSecretVetoSecret)
			Expect(hasSecret).To(BeFalse(), "the exclude-vetoed companion Secret %s must be dropped from the VM node manifests", vmSecretVetoSecret)

			By("Asserting the vetoed Secret is recorded in the VM snapshot's domainSpecificController.excludedRefs (exact {apiVersion,kind,name} POC contract)")
			refs, err := nodeExcludedRefs(ctx, demoVMSnapshotGVR, ns, node.name)
			Expect(err).NotTo(HaveOccurred())
			Expect(hasExcludedRef(refs, companionSecretAPIVersion, companionSecretKind, vmSecretVetoSecret)).To(BeTrue(),
				"vetoed companion Secret %s must be recorded in the VM snapshot excludedRefs as {apiVersion=%s, kind=%s} (core/v1 Secret contract)", vmSecretVetoSecret, companionSecretAPIVersion, companionSecretKind)
		})

		It("brings Root B (matchLabels selector) to Ready with its content", func() {
			if !selectorPersisted {
				Skip("deployed Snapshot CRD has no spec.resourceSelector (controller image predates the feature); skipping selector root")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
			defer cancel()
			content, err := waitSnapshotReady(ctx, ns, vsRootB, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(content).NotTo(BeEmpty())
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())
		})

		It("combo veto+selector: the selector captures the un-vetoed match but the veto wins over the vetoed match", func() {
			if !selectorPersisted {
				Skip("deployed Snapshot CRD has no spec.resourceSelector (controller image predates the feature); skipping selector root")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+time.Minute)
			defer cancel()

			content, err := waitSnapshotReady(ctx, ns, vsRootB, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())

			var nodes []childRef
			Eventually(func(g Gomega) {
				nodes, err = walkSnapshotTree(ctx, ns, vsRootB)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(nodes).NotTo(BeEmpty(), "Root B must publish its child snapshot tree")
			}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())

			By("Asserting the selector captured the un-vetoed matching VM and excluded the non-matching + vetoed ones")
			vmNames, err := vmSnapshotManifestNames(ctx, ns, nodes)
			Expect(err).NotTo(HaveOccurred())
			Expect(containsString(vmNames, vsVMSelKeep)).To(BeTrue(), "selector-matching un-vetoed VM %s must be captured", vsVMSelKeep)
			Expect(containsString(vmNames, vsVMSelVeto)).To(BeFalse(), "the veto must win over the selector match for VM %s", vsVMSelVeto)
			Expect(containsString(vmNames, vsVMPlain)).To(BeFalse(), "non-matching VM %s must not be captured by the selector root", vsVMPlain)

			By("Asserting the vetoed selector match is recorded in Root B's domainSpecificController.excludedRefs (exact {apiVersion,kind,name})")
			refs, err := nodeExcludedRefs(ctx, snapshotGVR, ns, vsRootB)
			Expect(err).NotTo(HaveOccurred())
			Expect(hasExcludedRef(refs, demoGroupVersion, "DemoVirtualMachine", vsVMSelVeto)).To(BeTrue(),
				"the vetoed selector-matching VM %s must be recorded in the root excludedRefs as {apiVersion=%s, kind=DemoVirtualMachine} (veto wins)", vsVMSelVeto, demoGroupVersion)
		})
	})
}

// --- Fixture B: data-backed ------------------------------------------------

func vetoSelectorVolumeDataSpecs() {
	Context("Fixture B: data-backed veto (disk child + orphan PVC)", func() {
		var (
			ns       string
			sc       string
			vscClass string
		)

		BeforeAll(func() {
			if !envEnabledByDefault(os.Getenv(envVetoSelector)) {
				Skip(envVetoSelector + "=false: skipping the veto/selector specs (they run by default)")
			}
			if !suiteCfg.volumeData {
				Skip("E2E_VOLUME_DATA=false: skipping the data-backed veto fixture (it runs by default)")
			}
			sc = suiteCfg.storageClass
			ns = uniqueNS("p3e-veto-sel-data")

			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
			defer cancel()

			By("Ensuring the thin, snapshot-capable StorageClass + VolumeSnapshotClass wiring (idempotent)")
			_, err := testkit.EnsureDefaultStorageClass(ctx, suiteRestCfg, testkit.DefaultStorageClassConfig{
				StorageClassName:     sc,
				LVMType:              "Thin",
				ThinPoolName:         "thinpool",
				BaseKubeconfig:       suiteClusterResources.BaseKubeconfig,
				VMNamespace:          suiteCfg.vmNamespace,
				BaseStorageClassName: suiteCfg.baseStorageClass,
			})
			Expect(err).NotTo(HaveOccurred(), "ensure StorageClass")
			Expect(ensureStorageClassVolumeSnapshotClass(ctx, sc)).To(Succeed())
			vscClass, err = resolveLocalVolumeSnapshotClass(ctx)
			Expect(err).NotTo(HaveOccurred(), "resolve VolumeSnapshotClass")

			By("Creating the namespace and the data-backed source (kept vs vetoed disk + vetoed orphan PVC)")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, ns)
			})
			veto := map[string]interface{}{storagev1alpha1.ExcludeLabelKey: "true"}
			source := []*unstructured.Unstructured{
				buildPlainPVC(ns, vsdPVCKept, sc, nil),
				buildPlainPVC(ns, vsdPVCVeto, sc, nil),
				buildPlainPVC(ns, vsdPVCOrphan, sc, veto),
				vetoDemoVM(ns, vsdVMDisk, vsdDiskKept, nil),
				vetoDemoVM(ns, vsdVMDiskVeto, vsdDiskVeto, nil),
				vetoDemoDisk(ns, vsdDiskKept, vsdPVCKept, sc, nil),
				vetoDemoDisk(ns, vsdDiskVeto, vsdPVCVeto, sc, veto),
			}
			Expect(applyObjects(ctx, source, ns)).To(Succeed())

			By("Binding the PVCs with a probe Pod (WaitForFirstConsumer StorageClass)")
			_, err = suiteClientset.CoreV1().Pods(ns).Create(ctx, probePodSpec(ns, vsdProbe, []string{vsdPVCKept, vsdPVCVeto, vsdPVCOrphan}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create probe pod")
			Expect(waitPodRunning(ctx, ns, vsdProbe, 10*time.Minute)).To(Succeed())

			By("Creating the root Snapshot over the data-backed tree")
			Expect(createRootSnapshot(ctx, ns, vsdRoot)).To(Succeed())
		})

		It("captures the kept disk's data but vetoes the vetoed disk (no disk child; recorded in the VM snapshot excludedRefs)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+10*time.Minute)
			defer cancel()

			By("Waiting for the root Snapshot + bound SnapshotContent to become Ready")
			content, err := waitSnapshotReady(ctx, ns, vsdRoot, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())

			By("Waiting for the kept PVC's data binding to be published")
			_, err = waitContentDataRefs(ctx, content, []string{vsdPVCKept}, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())

			By("Asserting only the kept disk expanded into a DemoVirtualDiskSnapshot")
			var nodes []childRef
			Eventually(func(g Gomega) {
				nodes, err = walkSnapshotTree(ctx, ns, vsdRoot)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(nodes).NotTo(BeEmpty(), "the data-backed root must publish its child tree")
			}).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())
			diskNames, err := diskSnapshotSourceNames(ctx, ns, nodes)
			Expect(err).NotTo(HaveOccurred())
			Expect(containsString(diskNames, vsdDiskKept)).To(BeTrue(), "kept disk %s must expand into a DemoVirtualDiskSnapshot", vsdDiskKept)
			Expect(containsString(diskNames, vsdDiskVeto)).To(BeFalse(), "exclude-vetoed disk %s must not be captured", vsdDiskVeto)

			By("Asserting the vetoed disk is recorded in its owning VM snapshot's domainSpecificController.excludedRefs")
			node, _, found, err := findVMSnapshotNodeForSource(ctx, ns, nodes, vsdVMDiskVeto)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), "the VM %s (whose disk is vetoed) must still be captured", vsdVMDiskVeto)
			refs, err := nodeExcludedRefs(ctx, demoVMSnapshotGVR, ns, node.name)
			Expect(err).NotTo(HaveOccurred())
			Expect(hasExcludedRef(refs, demoGroupVersion, "DemoVirtualDisk", vsdDiskVeto)).To(BeTrue(),
				"vetoed disk %s must be recorded in the VM snapshot excludedRefs as {apiVersion=%s, kind=DemoVirtualDisk}", vsdDiskVeto, demoGroupVersion)
		})

		It("never orphan-captures the vetoed PVC and does not adopt its user VolumeSnapshot (managed=false)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.snapshotReadyTO+5*time.Minute)
			defer cancel()

			content, err := waitSnapshotReady(ctx, ns, vsdRoot, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())

			By("Creating a user VolumeSnapshot over the vetoed orphan PVC " + vsdPVCOrphan)
			_, err = suiteDyn.Resource(volumeSnapshotGVR).Namespace(ns).Create(ctx, buildUserVolumeSnapshot(ns, vsdOrphanVS, vsdPVCOrphan, vscClass), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create user VolumeSnapshot over the vetoed PVC")

			By("Asserting the adoption veto latched managed=false and no state-snapshotter content was bound")
			Expect(waitVSManagedLabel(ctx, ns, vsdOrphanVS, managedFalse, suiteCfg.snapshotReadyTO)).To(Succeed())
			Consistently(func(g Gomega) {
				vs, gerr := getResource(ctx, volumeSnapshotGVR, ns, vsdOrphanVS)
				g.Expect(gerr).NotTo(HaveOccurred())
				bound, _, _ := unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName")
				g.Expect(bound).To(BeEmpty(), "a VolumeSnapshot on a vetoed PVC must NOT get a state-snapshotter SnapshotContent")
			}).WithTimeout(30 * time.Second).WithPolling(pollInterval).Should(Succeed())

			By("Asserting the vetoed orphan PVC is never captured as root volume data")
			Consistently(func(g Gomega) {
				bindings, werr := walkContentDataRefs(ctx, content)
				g.Expect(werr).NotTo(HaveOccurred())
				for _, b := range bindings {
					g.Expect(b.pvc).NotTo(Equal(vsdPVCOrphan), "exclude-vetoed orphan PVC %s must never be captured as volume data", vsdPVCOrphan)
				}
			}).WithTimeout(15 * time.Second).WithPolling(pollInterval).Should(Succeed())
		})
	})
}
