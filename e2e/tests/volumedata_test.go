/*
Copyright 2025 Flant JSC

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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// Phase-3 source object names (full PVC variant of docs/.../snapshot-tree-demo/01-source.yaml). All three
// PVCs exercise a distinct data-capture path: demo-pvc is a root orphan PVC (CSI VolumeSnapshot leaf),
// demo-pvc-disk is nested under DemoVirtualDisk/disk-vm (domain VolumeCaptureRequest path), and
// demo-pvc-standalone backs the standalone DemoVirtualDisk/disk-standalone (domain path, direct root child).
const (
	vdRootSnapshotName = "vol-tree"
	vdConfigMapName    = "demo-snapshot-cm"
	vdPVCRoot          = "demo-pvc"
	vdPVCDisk          = "demo-pvc-disk"
	vdPVCStandalone    = "demo-pvc-standalone"
	vdVMName           = "vm-1"
	vdDiskVM           = "disk-vm"
	vdDiskStandalone   = "disk-standalone"
	vdProbePod         = "vol-probe"
	vdProbeContainer   = "probe"

	// localCSIDriver is the sds-local-volume CSI driver; used to create a VolumeSnapshotClass when the
	// module does not ship a default one. Confirmed against the plan; provisioned SC uses this driver.
	localCSIDriver = "local.csi.state-snapshotter.deckhouse.io"

	// annStorageClassVSC is the StorageClass annotation the capture path resolves the VolumeSnapshotClass
	// through (PVC -> StorageClass -> annotation -> VolumeSnapshotClass), mirroring
	// pkg/snapshot.AnnotationStorageClassVolumeSnapshotClass. The cluster default class is NOT consulted,
	// so the provisioned SC MUST carry this annotation for the data leg to capture.
	annStorageClassVSC = "state-snapshotter.deckhouse.io/volumesnapshotclass"

	vdMarkerFile = "marker"
)

// volBinding maps a captured source PVC to the durable VolumeSnapshotContent artifact backing its data,
// plus the captured volume mode (the VRR requires it; no implicit default).
type volBinding struct {
	pvc        string
	vsc        string
	volumeMode string
}

// vsConnectorSubPath builds the generic-PVC extended VolumeSnapshot connector subresource path
// (subresources.snapshot.storage.k8s.io/v1), keyed by a real CSI VolumeSnapshot name.
func vsConnectorSubPath(ns, name, sub string) string {
	return fmt.Sprintf("/apis/%s/%s/namespaces/%s/volumesnapshots/%s/%s", vsConnectorGroup, vsConnectorVer, ns, name, sub)
}

// buildVolumeSource returns the full demo source (minus the bind Pod, which the suite builds separately as a
// shell-capable probe): ConfigMap + three PVCs on the provisioned SC + the demo inventory wiring disk-vm and
// disk-standalone to their backing PVCs.
func buildVolumeSource(ns, sc string) []*unstructured.Unstructured {
	pvc := func(name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "PersistentVolumeClaim",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
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
	configMap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      vdConfigMapName,
			"namespace": ns,
		},
		"data": map[string]interface{}{"demo": "tree"},
	}}
	vm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualMachine",
		"metadata": map[string]interface{}{
			"name":      vdVMName,
			"namespace": ns,
		},
		"spec": map[string]interface{}{"virtualDiskName": vdDiskVM},
	}}
	diskVM := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualDisk",
		"metadata": map[string]interface{}{
			"name":      vdDiskVM,
			"namespace": ns,
		},
		// size + storageClassName satisfy the scratch-provisioning guards (the disk adopts the
		// pre-created PVC vdPVCDisk; the values mirror that PVC).
		"spec": map[string]interface{}{
			"persistentVolumeClaimName": vdPVCDisk,
			"size":                      "500Mi",
			"storageClassName":          sc,
		},
	}}
	diskStandalone := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualDisk",
		"metadata": map[string]interface{}{
			"name":      vdDiskStandalone,
			"namespace": ns,
		},
		// Standalone disk (attached to no VM) backed by its own pre-created PVC, so it captures real
		// volume data as a direct root-child data leaf — mirroring disk-vm's adopt-the-PVC wiring. A
		// data-backed disk must never be manifest-only in the volume-data flow.
		"spec": map[string]interface{}{
			"persistentVolumeClaimName": vdPVCStandalone,
			"size":                      "500Mi",
			"storageClassName":          sc,
		},
	}}
	return []*unstructured.Unstructured{configMap, pvc(vdPVCRoot), pvc(vdPVCDisk), pvc(vdPVCStandalone), vm, diskVM, diskStandalone}
}

// probePodSpec builds a long-lived shell-capable Pod (the probe image must ship sh/echo/cat) mounting the
// named PVCs at /mnt/<pvc>, so the suite can write/read marker bytes via pods/exec.
func probePodSpec(ns, name string, pvcs []string) *corev1.Pod {
	var mounts []corev1.VolumeMount
	var volumes []corev1.Volume
	for _, p := range pvcs {
		mounts = append(mounts, corev1.VolumeMount{Name: p, MountPath: "/mnt/" + p})
		volumes = append(volumes, corev1.Volume{
			Name: p,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: p},
			},
		})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name:         vdProbeContainer,
				Image:        suiteCfg.probeImage,
				Command:      []string{"sh", "-c", "sleep 360000"},
				VolumeMounts: mounts,
			}},
			Volumes: volumes,
		},
	}
}

// markerVolumePath is the in-pod path of the marker file for a probe pod mounting pvc at /mnt/<pvc>.
func markerVolumePath(pvc string) string {
	return "/mnt/" + pvc + "/" + vdMarkerFile
}

// waitPodRunning blocks until the pod reports phase Running.
func waitPodRunning(ctx context.Context, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		pod, err := suiteClientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			if pod.Status.Phase == corev1.PodRunning {
				return nil
			}
			last = fmt.Sprintf("phase=%s", pod.Status.Phase)
		} else {
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for pod %s/%s Running; last: %s", ns, name, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// waitPVCBoundWithPVFix waits for a PVC to reach Bound. It works around the current provisioner image,
// which can create the restored PV without a storageClassName: once the PVC has a bound PV name, an empty
// PV storageClassName is patched to sc so binding can complete (mirrors the demo restore hack script).
func waitPVCBoundWithPVFix(ctx context.Context, ns, pvc, sc string, timeout time.Duration) error {
	cs := suiteClientset
	deadline := time.Now().Add(timeout)
	patched := false
	var last string
	for {
		p, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pvc, metav1.GetOptions{})
		if err == nil {
			if p.Status.Phase == corev1.ClaimBound {
				return nil
			}
			last = fmt.Sprintf("phase=%s", p.Status.Phase)
			if !patched && p.Spec.VolumeName != "" {
				pv, perr := cs.CoreV1().PersistentVolumes().Get(ctx, p.Spec.VolumeName, metav1.GetOptions{})
				if perr == nil && pv.Spec.StorageClassName == "" {
					pv.Spec.StorageClassName = sc
					if _, uerr := cs.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{}); uerr == nil {
						patched = true
					}
				}
			}
		} else {
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for PVC %s/%s Bound; last: %s", ns, pvc, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// walkContentDataRefs BFS-walks the SnapshotContent tree from rootContent and collects every node's
// status.dataRef that points at a VolumeSnapshotContent artifact (the durable data leg per Variant A:
// at most one dataRef per node, multiple volumes appear as child volume-node contents).
func walkContentDataRefs(ctx context.Context, rootContent string) ([]volBinding, error) {
	queue := []string{rootContent}
	seen := map[string]bool{}
	var out []volBinding
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true

		obj, err := getResource(ctx, snapshotContentGVR, "", name)
		if err != nil {
			return nil, fmt.Errorf("get SnapshotContent %s: %w", name, err)
		}
		if dr, ok, _ := unstructured.NestedMap(obj.Object, "status", "dataRef"); ok {
			artifactKind, _, _ := unstructured.NestedString(dr, "artifact", "kind")
			artifactName, _, _ := unstructured.NestedString(dr, "artifact", "name")
			targetName, _, _ := unstructured.NestedString(dr, "target", "name")
			volumeMode, _, _ := unstructured.NestedString(dr, "volumeMode")
			if artifactKind == "VolumeSnapshotContent" && artifactName != "" {
				out = append(out, volBinding{pvc: targetName, vsc: artifactName, volumeMode: volumeMode})
			}
		}
		queue = append(queue, childContentNames(obj)...)
	}
	return out, nil
}

// waitContentDataRefs polls the SnapshotContent tree until every wantPVC has a published dataRef
// binding. The orphan-PVC child volume node's dataRef is published asynchronously by the controller
// after its CSI VolumeSnapshot leaf becomes readyToUse, so walking the tree immediately after the
// snapshot children report Ready can race (the orphan binding is not yet linked under the root content).
func waitContentDataRefs(ctx context.Context, rootContent string, wantPVCs []string, timeout time.Duration) ([]volBinding, error) {
	deadline := time.Now().Add(timeout)
	var last []volBinding
	var lastErr error
	for {
		bindings, err := walkContentDataRefs(ctx, rootContent)
		if err != nil {
			lastErr = err
		} else {
			last = bindings
			have := map[string]bool{}
			for _, b := range bindings {
				have[b.pvc] = true
			}
			missing := ""
			for _, p := range wantPVCs {
				if !have[p] {
					missing = p
					break
				}
			}
			if missing == "" {
				return bindings, nil
			}
			lastErr = fmt.Errorf("no data binding yet for captured PVC %q", missing)
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("timeout waiting for content dataRefs covering %v: %w", wantPVCs, lastErr)
		}
		if !sleepCtx(ctx, pollInterval) {
			return last, ctx.Err()
		}
	}
}

// createVolumeRestoreRequest creates a storage-foundation VRR that materializes targetPVC from a
// VolumeSnapshotContent artifact into restoreNS. volumeMode is required by the VRR CRD (the executor
// builds CSI VolumeCapabilities from it); it falls back to Filesystem, matching PVCs created without an
// explicit spec.volumeMode.
func createVolumeRestoreRequest(ctx context.Context, restoreNS, targetPVC, vsc, sc, volumeMode string) error {
	if volumeMode == "" {
		volumeMode = "Filesystem"
	}
	vrr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "VolumeRestoreRequest",
		"metadata": map[string]interface{}{
			"name":      "restore-" + targetPVC,
			"namespace": restoreNS,
		},
		"spec": map[string]interface{}{
			"sourceRef": map[string]interface{}{
				"kind": "VolumeSnapshotContent",
				"name": vsc,
			},
			// targetRef carries only kind+name: restore is never cross-namespace, so the foundation VRR
			// controller derives the target namespace from metadata.namespace (set to restoreNS above).
			"targetRef": map[string]interface{}{
				"kind": "PersistentVolumeClaim",
				"name": targetPVC,
			},
			"storageClassName": sc,
			"volumeMode":       volumeMode,
		},
	}}
	_, err := suiteDyn.Resource(volumeRestoreRequestGVR).Namespace(restoreNS).Create(ctx, vrr, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// resolveLocalVolumeSnapshotClass returns the name of a VolumeSnapshotClass for the local CSI driver,
// reusing a module-shipped one if present and otherwise creating a dedicated class.
func resolveLocalVolumeSnapshotClass(ctx context.Context) (string, error) {
	list, err := suiteDyn.Resource(storagekube.VolumeSnapshotClassGVR).List(ctx, metav1.ListOptions{})
	if err == nil {
		for i := range list.Items {
			if drv, _, _ := unstructured.NestedString(list.Items[i].Object, "driver"); drv == localCSIDriver {
				return list.Items[i].GetName(), nil
			}
		}
	}
	const name = "e2e-local-thin"
	if cerr := storagekube.CreateVolumeSnapshotClass(ctx, suiteRestCfg, storagekube.VolumeSnapshotClassConfig{
		Name:           name,
		Driver:         localCSIDriver,
		DeletionPolicy: "Delete",
	}); cerr != nil {
		return "", cerr
	}
	return name, nil
}

// ensureStorageClassVolumeSnapshotClass guarantees the provisioned StorageClass carries the
// state-snapshotter.deckhouse.io/volumesnapshotclass annotation pointing at an existing, driver-matching
// VolumeSnapshotClass. The capture path resolves the class exclusively through this annotation (never the
// cluster default), so without it the data leg fails even though a default class exists.
func ensureStorageClassVolumeSnapshotClass(ctx context.Context, scName string) error {
	sc, err := suiteClientset.StorageV1().StorageClasses().Get(ctx, scName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get StorageClass %s: %w", scName, err)
	}
	// If the SC already points at an existing VolumeSnapshotClass, keep the module's wiring as-is.
	if cur := sc.Annotations[annStorageClassVSC]; cur != "" {
		if _, gerr := suiteDyn.Resource(storagekube.VolumeSnapshotClassGVR).Get(ctx, cur, metav1.GetOptions{}); gerr == nil {
			GinkgoWriter.Printf("StorageClass %s already wired to VolumeSnapshotClass %s\n", scName, cur)
			return nil
		}
	}
	vscName, err := resolveLocalVolumeSnapshotClass(ctx)
	if err != nil {
		return err
	}
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, annStorageClassVSC, vscName))
	if _, err := suiteClientset.StorageV1().StorageClasses().Patch(ctx, scName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("annotate StorageClass %s with %s=%s: %w", scName, annStorageClassVSC, vscName, err)
	}
	GinkgoWriter.Printf("wired StorageClass %s -> VolumeSnapshotClass %s\n", scName, vscName)
	return nil
}

// volumeDataSpecs registers the phase-3 full volume-data flow specs (env-gated by E2E_VOLUME_DATA): it
// provisions a thin, snapshot-capable StorageClass via the storage-e2e helper, captures a real PVC tree
// with marker bytes, and asserts the data round-trips through a VolumeRestoreRequest restore.
func volumeDataSpecs() {
	Context("Phase 3: full volume-data flow", func() {
		var (
			srcNS       string
			sc          string
			bindings    []volBinding
			rootContent string
			// per-PVC marker content written into the source volumes, verified after restore.
			markers = map[string]string{}
		)

		BeforeAll(func() {
			if !suiteCfg.volumeData {
				Skip("E2E_VOLUME_DATA not set: skipping the phase-3 volume-data flow")
			}
			sc = suiteCfg.storageClass
			srcNS = uniqueNS("vol")
			markers[vdPVCRoot] = "root-" + fmt.Sprintf("%d", time.Now().UnixNano())
			markers[vdPVCDisk] = "disk-" + fmt.Sprintf("%d", time.Now().UnixNano())
			markers[vdPVCStandalone] = "standalone-" + fmt.Sprintf("%d", time.Now().UnixNano())

			// SC provisioning + module enablement is the slow part of phase 3.
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
			defer cancel()

			By("Provisioning a thin, snapshot-capable default StorageClass via storage-e2e (" + sc + ")")
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

			By("Creating the source namespace and applying the full PVC source")
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			Expect(applyObjects(ctx, buildVolumeSource(srcNS, sc), srcNS)).To(Succeed())

			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, srcNS)
			})

			By("Starting the source probe Pod and waiting for it to run (binds all three PVCs)")
			_, err = suiteClientset.CoreV1().Pods(srcNS).Create(ctx, probePodSpec(srcNS, vdProbePod, []string{vdPVCRoot, vdPVCDisk, vdPVCStandalone}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create source probe pod")
			Expect(waitPodRunning(ctx, srcNS, vdProbePod, 10*time.Minute)).To(Succeed())

			By("Writing marker bytes into all three source PVCs")
			writeCmd := fmt.Sprintf("printf %%s %q > %s && printf %%s %q > %s && printf %%s %q > %s && sync",
				markers[vdPVCRoot], markerVolumePath(vdPVCRoot),
				markers[vdPVCDisk], markerVolumePath(vdPVCDisk),
				markers[vdPVCStandalone], markerVolumePath(vdPVCStandalone))
			_, _, err = storagekube.ExecInPod(ctx, suiteRestCfg, srcNS, vdProbePod, vdProbeContainer, []string{"sh", "-c", writeCmd})
			Expect(err).NotTo(HaveOccurred(), "write marker bytes")
		})

		It("captures the volume data (VolumeReady + dataRef artifacts populated)", func() {
			// Capture (LVM snapshot creation) is fast — bound by captureReadyTO, not the restore-path
			// snapshotReadyTO. Only the restore/data-upload waits keep the generous budget.
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+5*time.Minute)
			defer cancel()

			// Background capture timeline: surfaces where the volume-data snapshot creation spends time.
			tl := startCaptureTimeline(srcNS)
			defer tl.stop()

			By("Creating the root Snapshot over the PVC tree")
			Expect(createRootSnapshot(ctx, srcNS, vdRootSnapshotName)).To(Succeed())

			By("Waiting for the Snapshot + bound SnapshotContent (incl. VolumeReady) to become Ready")
			content, err := waitSnapshotReady(ctx, srcNS, vdRootSnapshotName, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())
			rootContent = content

			By("Walking the content tree to collect data artifacts (PVC -> VolumeSnapshotContent)")
			bindings, err = walkContentDataRefs(ctx, content)
			Expect(err).NotTo(HaveOccurred())
			for _, b := range bindings {
				GinkgoWriter.Printf("  dataRef: pvc=%s vsc=%s\n", b.pvc, b.vsc)
			}
			Expect(bindings).NotTo(BeEmpty(), "expected at least one captured volume data artifact")
		})

		It("serves the generic-PVC VolumeSnapshot connector manifests-download", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			By("Finding a captured CSI VolumeSnapshot that resolves to a SnapshotContent")
			list, err := suiteDyn.Resource(volumeSnapshotGVR).Namespace(srcNS).List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred(), "list VolumeSnapshots in source namespace")
			var vsName string
			for i := range list.Items {
				if bound, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "boundSnapshotContentName"); bound != "" {
					vsName = list.Items[i].GetName()
					break
				}
			}
			if vsName == "" {
				Skip("no captured VolumeSnapshot exposes status.boundSnapshotContentName; connector read not applicable")
			}

			By("Reading the connector manifests-download for VolumeSnapshot " + vsName)
			path := vsConnectorSubPath(srcNS, vsName, subManifestsDownload)
			body, err := aggGet(ctx, path, nil)
			Expect(err).NotTo(HaveOccurred(), "GET %s", path)
			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			Expect(objs).NotTo(BeEmpty(), "connector manifests-download should return the bound PVC node")
		})

		It("round-trips the volume data through a VolumeRestoreRequest restore", func() {
			Expect(rootContent).NotTo(BeEmpty(), "capture spec must have populated the root content")
			Expect(bindings).NotTo(BeEmpty(), "capture spec must have collected data bindings")

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			restoreNS := uniqueNS("vol-restore")
			By("Creating the restore namespace " + restoreNS)
			Expect(ensureNamespace(ctx, restoreNS)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, restoreNS)
			})

			By("Applying the non-volume restored manifests (PVCs/Pods restored separately)")
			path := coreSnapshotSubPath(srcNS, vdRootSnapshotName, subManifestsRestore)
			body, err := aggGet(ctx, path, map[string]string{"targetNamespace": restoreNS})
			Expect(err).NotTo(HaveOccurred(), "GET %s", path)
			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			var applyable []*unstructured.Unstructured
			for i := range objs {
				switch objs[i].GetKind() {
				case "PersistentVolumeClaim", "Pod":
					continue
				}
				applyable = append(applyable, &objs[i])
			}
			Expect(applyObjects(ctx, applyable, restoreNS)).To(Succeed())

			By("Restoring each captured PVC via a VolumeRestoreRequest and waiting for Bound + Ready")
			restored := make([]string, 0, len(bindings))
			for _, b := range bindings {
				if _, ok := markers[b.pvc]; !ok {
					// Only the two known source PVCs carry markers; restore just those for verification.
					continue
				}
				Expect(createVolumeRestoreRequest(ctx, restoreNS, b.pvc, b.vsc, sc, b.volumeMode)).To(Succeed())
				Expect(waitPVCBoundWithPVFix(ctx, restoreNS, b.pvc, sc, 10*time.Minute)).To(Succeed())
				Expect(waitObjectCondition(ctx, volumeRestoreRequestGVR, restoreNS, "restore-"+b.pvc, condReady, "True", 10*time.Minute)).
					To(Succeed(), "VRR restore-%s Ready", b.pvc)
				restored = append(restored, b.pvc)
			}
			Expect(restored).NotTo(BeEmpty(), "expected to restore at least one marker-bearing PVC")

			By("Mounting the restored PVCs and asserting the marker bytes survived")
			_, err = suiteClientset.CoreV1().Pods(restoreNS).Create(ctx, probePodSpec(restoreNS, vdProbePod, restored), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create restore probe pod")
			Expect(waitPodRunning(ctx, restoreNS, vdProbePod, 10*time.Minute)).To(Succeed())

			for _, pvc := range restored {
				got, err := storagekube.ReadFileFromPod(ctx, suiteRestCfg, restoreNS, vdProbePod, vdProbeContainer, markerVolumePath(pvc))
				Expect(err).NotTo(HaveOccurred(), "read marker from restored PVC %s", pvc)
				Expect(got).To(Equal(markers[pvc]), "restored PVC %s must preserve its marker bytes", pvc)
			}
		})
	})
}
