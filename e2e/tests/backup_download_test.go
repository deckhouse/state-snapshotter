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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// Phase-4 backup-download source object names (Block volume variant).
const (
	bkRootSnapshotName = "bk-tree"
	bkConfigMapName    = "demo-snapshot-cm"
	bkPVCName          = "bk-pvc"
	bkDiskAName        = "disk-a"
	bkDiskBName        = "disk-b"
	bkPVCDiskA         = "bk-pvc-disk-a"
	bkPVCDiskB         = "bk-pvc-disk-b"
	bkVMName           = "vm-1"
	bkBackupClientPod  = "backup-client"
	bkBackupClientSA   = "backup-client"
	bkBackupClientCont = "client"
	bkBlockWriterCont  = "writer"
	bkBlockMiB         = 8
	bkBlockBytes       = bkBlockMiB * 1024 * 1024
)

type dataExportTarget struct {
	exportName   string
	group        string
	resource     string
	snapshotName string
	pvcName      string
}

// buildBackupSource returns the Block-volume demo source: ConfigMap + orphan Block PVC +
// two Block DemoVirtualDisks (scratch PVCs) without the VM (created after data is written).
func buildBackupSource(ns, sc string) []*unstructured.Unstructured {
	blockPVC := func(name string) *unstructured.Unstructured {
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
				"volumeMode":       "Block",
				"resources": map[string]interface{}{
					"requests": map[string]interface{}{"storage": "1Gi"},
				},
			},
		}}
	}
	disk := func(name, pvcName string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": demoGroupVersion,
			"kind":       "DemoVirtualDisk",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				"persistentVolumeClaimName": pvcName,
				"size":                      "1Gi",
				"storageClassName":          sc,
				"volumeMode":                "Block",
			},
		}}
	}
	configMap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      bkConfigMapName,
			"namespace": ns,
		},
		"data": map[string]interface{}{"demo": "backup-tree"},
	}}
	return []*unstructured.Unstructured{
		configMap,
		blockPVC(bkPVCName),
		disk(bkDiskAName, bkPVCDiskA),
		disk(bkDiskBName, bkPVCDiskB),
	}
}

func buildBackupVM(ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualMachine",
		"metadata": map[string]interface{}{
			"name":      bkVMName,
			"namespace": ns,
		},
		"spec": map[string]interface{}{"virtualDiskName": bkDiskAName},
	}}
}

func blockWriterPodSpec(ns, podName, pvcName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    bkBlockWriterCont,
				Image:   suiteCfg.probeImage,
				Command: []string{"sh", "-c", "sleep 3600"},
				VolumeDevices: []corev1.VolumeDevice{{
					Name:       pvcName,
					DevicePath: "/dev/xvda",
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: pvcName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
				},
			}},
		},
	}
}

func writeBlockAndChecksum(ctx context.Context, ns, podName, pvcName string) (string, error) {
	writeCmd := fmt.Sprintf(
		"dd if=/dev/urandom of=/dev/xvda bs=1M count=%d conv=fsync 2>/dev/null && dd if=/dev/xvda bs=1M count=%d 2>/dev/null | sha256sum | awk '{print $1}'",
		bkBlockMiB, bkBlockMiB,
	)
	stdout, stderr, err := storagekube.ExecInPod(ctx, suiteRestCfg, ns, podName, bkBlockWriterCont, []string{"sh", "-c", writeCmd})
	if err != nil {
		return "", fmt.Errorf("write+checksum on PVC %s: %w (stderr=%q)", pvcName, err, stderr)
	}
	sum := strings.TrimSpace(stdout)
	if len(sum) != 64 {
		return "", fmt.Errorf("unexpected sha256 output for PVC %s: %q (stderr=%q)", pvcName, sum, stderr)
	}
	return sum, nil
}

func deletePod(ctx context.Context, ns, name string) {
	_ = suiteClientset.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{})
}

func assertBackupTopology(ctx context.Context, ns, rootSnap string) error {
	root, err := getResource(ctx, snapshotGVR, ns, rootSnap)
	if err != nil {
		return fmt.Errorf("get root Snapshot: %w", err)
	}
	rootChildren := childSnapshotRefs(root)

	var vmSnap childRef
	var diskBSnap childRef
	for _, c := range rootChildren {
		switch c.kind {
		case "DemoVirtualMachineSnapshot":
			vmSnap = c
		case "DemoVirtualDiskSnapshot":
			// Standalone disk-b snapshot is a direct root child (disk-a is nested under the VM).
			diskBSnap = c
		}
	}
	if vmSnap.name == "" {
		return fmt.Errorf("expected a DemoVirtualMachineSnapshot among root children")
	}
	if diskBSnap.name == "" {
		return fmt.Errorf("expected a standalone DemoVirtualDiskSnapshot among root children")
	}

	vmObj, err := getResource(ctx, demoVMSnapshotGVR, ns, vmSnap.name)
	if err != nil {
		return fmt.Errorf("get VM snapshot %s: %w", vmSnap.name, err)
	}
	vmChildren := childSnapshotRefs(vmObj)
	var diskAFound bool
	for _, c := range vmChildren {
		if c.kind != "DemoVirtualDiskSnapshot" {
			continue
		}
		// manifests-download on the nested disk snapshot should contain disk-a.
		path := coreGenericSubPath(ns, resDemoDiskSnapshots, c.name, subManifestsDownload)
		body, aerr := aggGet(ctx, path, nil)
		if aerr != nil {
			return fmt.Errorf("GET %s: %w", path, aerr)
		}
		objs, derr := decodeManifestArray(body)
		if derr != nil {
			return derr
		}
		if _, ok := findManifest(objs, "DemoVirtualDisk", bkDiskAName); ok {
			diskAFound = true
			break
		}
	}
	if !diskAFound {
		return fmt.Errorf("expected disk-a DemoVirtualDiskSnapshot nested under VM snapshot %s", vmSnap.name)
	}

	// Standalone disk-b: root child manifests-download should reference disk-b.
	path := coreGenericSubPath(ns, resDemoDiskSnapshots, diskBSnap.name, subManifestsDownload)
	body, err := aggGet(ctx, path, nil)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	objs, err := decodeManifestArray(body)
	if err != nil {
		return err
	}
	if _, ok := findManifest(objs, "DemoVirtualDisk", bkDiskBName); !ok {
		return fmt.Errorf("standalone root child snapshot %s should contain DemoVirtualDisk %s", diskBSnap.name, bkDiskBName)
	}
	return nil
}

func manifestDownloadPath(ns string, ref childRef) string {
	switch ref.kind {
	case "Snapshot":
		return coreSnapshotSubPath(ns, ref.name, subManifestsDownload)
	case "DemoVirtualMachineSnapshot":
		return coreGenericSubPath(ns, resDemoVMSnapshots, ref.name, subManifestsDownload)
	case "DemoVirtualDiskSnapshot":
		return coreGenericSubPath(ns, resDemoDiskSnapshots, ref.name, subManifestsDownload)
	default:
		return ""
	}
}

func gvrForLiveKind(kind string) (schema.GroupVersionResource, bool) {
	switch kind {
	case "ConfigMap":
		return configMapGVR, true
	case "PersistentVolumeClaim":
		return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"}, true
	case "DemoVirtualMachine":
		return demoVMGVR, true
	case "DemoVirtualDisk":
		return demoDiskGVR, true
	default:
		return schema.GroupVersionResource{}, false
	}
}

func assertManifestsMatchLive(ctx context.Context, ns string, downloaded []unstructured.Unstructured) error {
	for i := range downloaded {
		obj := &downloaded[i]
		gvr, ok := gvrForLiveKind(obj.GetKind())
		if !ok {
			continue
		}
		live, err := getResource(ctx, gvr, ns, obj.GetName())
		if err != nil {
			return fmt.Errorf("get live %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
		switch obj.GetKind() {
		case "ConfigMap":
			want, _, _ := unstructured.NestedStringMap(live.Object, "data")
			got, _, _ := unstructured.NestedStringMap(obj.Object, "data")
			if fmt.Sprint(want) != fmt.Sprint(got) {
				return fmt.Errorf("ConfigMap %s data mismatch: live=%v downloaded=%v", obj.GetName(), want, got)
			}
		case "DemoVirtualMachine":
			want, _, _ := unstructured.NestedString(live.Object, "spec", "virtualDiskName")
			got, _, _ := unstructured.NestedString(obj.Object, "spec", "virtualDiskName")
			if want != got {
				return fmt.Errorf("DemoVirtualMachine %s virtualDiskName mismatch: live=%q downloaded=%q", obj.GetName(), want, got)
			}
			if obj.GetName() == bkVMName && want != bkDiskAName {
				return fmt.Errorf("DemoVirtualMachine %s should reference %s, got %q", bkVMName, bkDiskAName, want)
			}
		case "DemoVirtualDisk":
			for _, key := range []string{"persistentVolumeClaimName", "storageClassName", "volumeMode", "size"} {
				want, _, _ := unstructured.NestedString(live.Object, "spec", key)
				got, _, _ := unstructured.NestedString(obj.Object, "spec", key)
				if want != got {
					return fmt.Errorf("DemoVirtualDisk %s spec.%s mismatch: live=%q downloaded=%q", obj.GetName(), key, want, got)
				}
			}
		case "PersistentVolumeClaim":
			wantMode, _, _ := unstructured.NestedString(live.Object, "spec", "volumeMode")
			gotMode, _, _ := unstructured.NestedString(obj.Object, "spec", "volumeMode")
			if wantMode != gotMode {
				return fmt.Errorf("PVC %s volumeMode mismatch: live=%q downloaded=%q", obj.GetName(), wantMode, gotMode)
			}
			wantSC, _, _ := unstructured.NestedString(live.Object, "spec", "storageClassName")
			gotSC, _, _ := unstructured.NestedString(obj.Object, "spec", "storageClassName")
			if wantSC != gotSC {
				return fmt.Errorf("PVC %s storageClassName mismatch: live=%q downloaded=%q", obj.GetName(), wantSC, gotSC)
			}
			wantStor, _, _ := unstructured.NestedString(live.Object, "spec", "resources", "requests", "storage")
			gotStor, _, _ := unstructured.NestedString(obj.Object, "spec", "resources", "requests", "storage")
			if wantStor != gotStor {
				return fmt.Errorf("PVC %s resources.requests.storage mismatch: live=%q downloaded=%q", obj.GetName(), wantStor, gotStor)
			}
		}
	}
	return nil
}

// anyBoundSnapshotContent reports whether any captured snapshot leaf (demo disk or generic VolumeSnapshot)
// exposes status.boundSnapshotContentName. When false, the extended-VS data surface (storage-foundation
// fork) is unavailable, so the DataExport snapshot resolver cannot work and the data-download spec skips
// (mirroring the phase-3 connector skip in volumedata_test.go).
func anyBoundSnapshotContent(ctx context.Context, ns string) (bool, error) {
	for _, gvr := range []schema.GroupVersionResource{demoDiskSnapshotGVR, volumeSnapshotGVR} {
		list, err := suiteDyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, fmt.Errorf("list %s: %w", gvr.Resource, err)
		}
		for i := range list.Items {
			if v, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "boundSnapshotContentName"); v != "" {
				return true, nil
			}
		}
	}
	return false, nil
}

func collectDataExportTargets(ctx context.Context, ns, rootContent string) ([]dataExportTarget, error) {
	bindings, err := walkContentDataRefs(ctx, rootContent)
	if err != nil {
		return nil, err
	}
	pvcToBinding := map[string]volBinding{}
	for _, b := range bindings {
		pvcToBinding[b.pvc] = b
	}

	type snapKey struct {
		kind string
		name string
	}
	pvcToSnap := map[string]snapKey{}

	listKinds := []struct {
		gvr  schema.GroupVersionResource
		kind string
	}{
		{demoDiskSnapshotGVR, "DemoVirtualDiskSnapshot"},
		{volumeSnapshotGVR, "VolumeSnapshot"},
	}
	for _, lk := range listKinds {
		list, lerr := suiteDyn.Resource(lk.gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
		if lerr != nil {
			return nil, fmt.Errorf("list %s: %w", lk.kind, lerr)
		}
		for i := range list.Items {
			snap := &list.Items[i]
			contentName, _, _ := unstructured.NestedString(snap.Object, "status", "boundSnapshotContentName")
			if contentName == "" {
				continue
			}
			content, gerr := getResource(ctx, snapshotContentGVR, "", contentName)
			if gerr != nil {
				continue
			}
			targetName, _, _ := unstructured.NestedString(content.Object, "status", "dataRef", "target", "name")
			if targetName == "" {
				continue
			}
			// First writer wins: demo disk snapshots are listed before generic VolumeSnapshots, so a
			// domain-owned PVC keeps its demo leaf even if a generic VS ever resolves to the same PVC.
			if _, exists := pvcToSnap[targetName]; !exists {
				pvcToSnap[targetName] = snapKey{kind: lk.kind, name: snap.GetName()}
			}
		}
	}

	var out []dataExportTarget
	for pvc, b := range pvcToBinding {
		sk, ok := pvcToSnap[pvc]
		if !ok {
			return nil, fmt.Errorf("no snapshot leaf found for captured PVC %q (vsc=%s)", pvc, b.vsc)
		}
		t := dataExportTarget{
			exportName:   "export-" + sk.name,
			pvcName:      pvc,
			snapshotName: sk.name,
		}
		switch sk.kind {
		case "DemoVirtualDiskSnapshot":
			t.group = "demo.state-snapshotter.deckhouse.io"
			t.resource = "demovirtualdisksnapshots"
		case "VolumeSnapshot":
			t.group = "snapshot.storage.k8s.io"
			t.resource = "volumesnapshots"
		default:
			return nil, fmt.Errorf("unsupported snapshot kind %q for PVC %q", sk.kind, pvc)
		}
		out = append(out, t)
	}
	return out, nil
}

func createDataExport(ctx context.Context, ns string, target dataExportTarget) error {
	de := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "storage.deckhouse.io/v1alpha1",
		"kind":       "DataExport",
		"metadata": map[string]interface{}{
			"name":      target.exportName,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"ttl": "15m",
			"targetRef": map[string]interface{}{
				"group":    target.group,
				"resource": target.resource,
				"name":     target.snapshotName,
			},
		},
	}}
	_, err := suiteDyn.Resource(dataExportGVR).Namespace(ns).Create(ctx, de, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func waitDataExportReady(ctx context.Context, ns, name string, timeout time.Duration) (url, ca string, err error) {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		obj, gerr := getResource(ctx, dataExportGVR, ns, name)
		if gerr == nil {
			st, reason, found := conditionStatus(obj, "Ready")
			if found && st == "True" {
				url, _, _ = unstructured.NestedString(obj.Object, "status", "url")
				ca, _, _ = unstructured.NestedString(obj.Object, "status", "ca")
				if url != "" {
					return url, ca, nil
				}
				last = "Ready=True but status.url is empty"
			} else {
				last = fmt.Sprintf("Ready=%v reason=%q", st, reason)
			}
		} else {
			last = fmt.Sprintf("get err=%v", gerr)
		}
		if time.Now().After(deadline) {
			return "", "", fmt.Errorf("timeout waiting for DataExport %s/%s Ready; last: %s", ns, name, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return "", "", ctx.Err()
		}
	}
}

func deleteDataExport(ctx context.Context, ns, name string) {
	_ = suiteDyn.Resource(dataExportGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
}

func ensureBackupClientRBAC(ctx context.Context, ns string) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: bkBackupClientSA, Namespace: ns},
	}
	if _, err := suiteClientset.CoreV1().ServiceAccounts(ns).Create(ctx, sa, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create ServiceAccount: %w", err)
	}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: bkBackupClientSA, Namespace: ns},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"storage.deckhouse.io"},
			Resources: []string{"dataexports/download"},
			Verbs:     []string{"create"},
		}},
	}
	if _, err := suiteClientset.RbacV1().Roles(ns).Create(ctx, role, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create Role: %w", err)
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bkBackupClientSA, Namespace: ns},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      bkBackupClientSA,
			Namespace: ns,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     bkBackupClientSA,
		},
	}
	if _, err := suiteClientset.RbacV1().RoleBindings(ns).Create(ctx, binding, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create RoleBinding: %w", err)
	}
	return nil
}

func backupClientPodSpec(ns string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: bkBackupClientPod, Namespace: ns},
		Spec: corev1.PodSpec{
			ServiceAccountName: bkBackupClientSA,
			RestartPolicy:      corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name:    bkBackupClientCont,
				Image:   suiteCfg.backupClientImage,
				Command: []string{"sh", "-c", "sleep 360000"},
			}},
		},
	}
}

func downloadBlockChecksum(ctx context.Context, ns, exportURL string) (string, error) {
	// status.url carries a trailing slash; trim it so we never emit "//api/v1/block" (Go's ServeMux would
	// 301-redirect the non-canonical path and curl, without -L, would hash the redirect body instead of
	// the device). sha256sum is the busybox/Alpine applet shipped by the default curlimages/curl image
	// (openssl CLI is NOT present there), and matches the source-side `dd | sha256sum` exactly.
	blockURL := strings.TrimRight(exportURL, "/") + "/api/v1/block"
	// -f makes curl fail (and suppress the body) on HTTP >=400 so an auth/SAR error doesn't get hashed as
	// if it were device bytes. We intentionally do NOT set pipefail: head closes the pipe after N bytes,
	// which SIGPIPEs curl by design; the pipeline's exit is sha256sum's (matching the source dd|sha256sum).
	dlCmd := fmt.Sprintf(
		`TOKEN=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token); curl -fksS -H "Authorization: Bearer $TOKEN" %q | head -c %d | sha256sum | awk '{print $1}'`,
		blockURL, bkBlockBytes,
	)
	stdout, stderr, err := storagekube.ExecInPod(ctx, suiteRestCfg, ns, bkBackupClientPod, bkBackupClientCont, []string{"sh", "-c", dlCmd})
	if err != nil {
		return "", fmt.Errorf("download block checksum: %w (stderr=%q)", err, stderr)
	}
	sum := strings.TrimSpace(stdout)
	if len(sum) != 64 {
		return "", fmt.Errorf("unexpected downloaded sha256: %q (stderr=%q)", sum, stderr)
	}
	return sum, nil
}

// backupDownloadSpecs registers the phase-4 backup-system download flow (env-gated by E2E_VOLUME_DATA):
// capture a Block-volume demo tree, download manifests via the aggregated API and volume bytes via
// SVDM DataExport from an in-cluster backup pod, then verify against live cluster state.
func backupDownloadSpecs() {
	Context("Phase 4: backup-system HTTP download", func() {
		var (
			srcNS       string
			sc          string
			rootContent string
			checksums   = map[string]string{}
			targets     []dataExportTarget
		)

		BeforeAll(func() {
			if !suiteCfg.volumeData {
				Skip("E2E_VOLUME_DATA not set: skipping the phase-4 backup download flow")
			}
			sc = suiteCfg.storageClass
			srcNS = uniqueNS("bk")

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

			By("Creating the source namespace and applying the Block-volume demo source")
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			Expect(applyObjects(ctx, buildBackupSource(srcNS, sc), srcNS)).To(Succeed())

			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, srcNS)
			})

			// The provisioned StorageClass (sds-local-volume) is WaitForFirstConsumer: the demo scratch
			// PVCs and the orphan PVC only bind once a consumer Pod is scheduled. We therefore do NOT gate
			// on disk Ready / PVC Bound here (that would deadlock without a consumer); each per-PVC
			// block-writer Pod below is the first consumer that triggers binding, and waitPodRunning blocks
			// until the WaitForFirstConsumer bind completes.
			pvcs := []string{bkPVCName, bkPVCDiskA, bkPVCDiskB}
			for i, pvc := range pvcs {
				podName := fmt.Sprintf("block-writer-%d", i)
				By("Writing Block data and recording checksum for PVC " + pvc)
				_, err = suiteClientset.CoreV1().Pods(srcNS).Create(ctx, blockWriterPodSpec(srcNS, podName, pvc), metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred(), "create block writer pod for %s", pvc)
				Expect(waitPodRunning(ctx, srcNS, podName, 10*time.Minute)).To(Succeed())
				sum, werr := writeBlockAndChecksum(ctx, srcNS, podName, pvc)
				Expect(werr).NotTo(HaveOccurred(), "write+checksum PVC %s", pvc)
				checksums[pvc] = sum
				GinkgoWriter.Printf("  source checksum pvc=%s sha256=%s\n", pvc, sum)
				deletePod(ctx, srcNS, podName)
				Expect(waitPodDeleted(ctx, srcNS, podName, 2*time.Minute)).To(Succeed())
			}

			By("Creating DemoVirtualMachine " + bkVMName + " attached to " + bkDiskAName)
			Expect(applyObjects(ctx, []*unstructured.Unstructured{buildBackupVM(srcNS)}, srcNS)).To(Succeed())
		})

		It("captures the Block-volume snapshot tree (Ready + expected topology)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.snapshotReadyTO+5*time.Minute)
			defer cancel()

			By("Creating the root Snapshot over the Block-volume tree")
			Expect(createRootSnapshot(ctx, srcNS, bkRootSnapshotName)).To(Succeed())

			By("Waiting for the Snapshot + bound SnapshotContent to become Ready")
			content, err := waitSnapshotReady(ctx, srcNS, bkRootSnapshotName, suiteCfg.snapshotReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.snapshotReadyTO)).To(Succeed())
			rootContent = content

			By("Waiting for all child snapshots to reach Ready=True")
			nodes, err := walkSnapshotTree(ctx, srcNS, bkRootSnapshotName)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).NotTo(BeEmpty())
			Expect(waitChildrenReady(ctx, srcNS, nodes, suiteCfg.snapshotReadyTO)).To(Succeed())

			By("Asserting disk-a is nested under the VM snapshot and disk-b is a standalone root child")
			Expect(assertBackupTopology(ctx, srcNS, bkRootSnapshotName)).To(Succeed())
		})

		It("downloads manifests via the aggregated API and matches live cluster objects", func() {
			Expect(rootContent).NotTo(BeEmpty(), "capture spec must have populated rootContent")

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			rootSnap := bkRootSnapshotName
			nodes, err := walkSnapshotTree(ctx, srcNS, rootSnap)
			Expect(err).NotTo(HaveOccurred())

			allRefs := append([]childRef{{kind: "Snapshot", name: rootSnap}}, nodes...)
			for _, ref := range allRefs {
				path := manifestDownloadPath(srcNS, ref)
				if path == "" {
					continue
				}
				By("Downloading manifests for " + ref.kind + "/" + ref.name)
				body, gerr := aggGet(ctx, path, nil)
				Expect(gerr).NotTo(HaveOccurred(), "GET %s", path)
				objs, derr := decodeManifestArray(body)
				Expect(derr).NotTo(HaveOccurred())
				Expect(objs).NotTo(BeEmpty(), "manifests-download for %s/%s", ref.kind, ref.name)
				Expect(assertManifestsMatchLive(ctx, srcNS, objs)).To(Succeed())
			}
		})

		It("downloads volume bytes via DataExport and matches source checksums", func() {
			Expect(rootContent).NotTo(BeEmpty(), "capture spec must have populated rootContent")

			// Budget: 3 legs x (15m DataExport Ready + download) + 5m backup-pod start, with headroom.
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
			defer cancel()

			By("Checking the extended-VS data surface is available (skip if the fork is absent)")
			hasLeaf, err := anyBoundSnapshotContent(ctx, srcNS)
			Expect(err).NotTo(HaveOccurred())
			if !hasLeaf {
				Skip("no snapshot leaf exposes status.boundSnapshotContentName: extended-VS data surface unavailable")
			}

			By("Collecting DataExport targets from the captured content tree")
			targets, err = collectDataExportTargets(ctx, srcNS, rootContent)
			Expect(err).NotTo(HaveOccurred())
			Expect(targets).To(HaveLen(3), "expected data exports for orphan PVC + two demo disks")

			By("Creating backup-client RBAC and pod")
			Expect(ensureBackupClientRBAC(ctx, srcNS)).To(Succeed())
			_, err = suiteClientset.CoreV1().Pods(srcNS).Create(ctx, backupClientPodSpec(srcNS), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create backup-client pod")
			Expect(waitPodRunning(ctx, srcNS, bkBackupClientPod, 5*time.Minute)).To(Succeed())

			for _, target := range targets {
				By(fmt.Sprintf("Exporting PVC %s via DataExport %s (target %s/%s)", target.pvcName, target.exportName, target.resource, target.snapshotName))
				Expect(createDataExport(ctx, srcNS, target)).To(Succeed())
				DeferCleanup(func(t dataExportTarget) func() {
					return func() {
						cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
						defer ccancel()
						deleteDataExport(cctx, srcNS, t.exportName)
					}
				}(target))

				url, _, werr := waitDataExportReady(ctx, srcNS, target.exportName, 15*time.Minute)
				Expect(werr).NotTo(HaveOccurred(), "DataExport %s Ready", target.exportName)
				GinkgoWriter.Printf("  DataExport %s url=%s\n", target.exportName, url)

				got, derr := downloadBlockChecksum(ctx, srcNS, url)
				Expect(derr).NotTo(HaveOccurred(), "download block data for PVC %s", target.pvcName)
				want := checksums[target.pvcName]
				Expect(want).NotTo(BeEmpty(), "source checksum for PVC %s", target.pvcName)
				Expect(got).To(Equal(want), "downloaded bytes for PVC %s must match source checksum", target.pvcName)

				// Release each exporter (VRR + export PVC + exporter pod) before the next leg to keep peak
				// resource pressure low (plan step 7); the DeferCleanup above remains a safety net.
				deleteDataExport(ctx, srcNS, target.exportName)
			}
		})
	})
}

func waitPodDeleted(ctx context.Context, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := suiteClientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for pod %s/%s deletion", ns, name)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}
