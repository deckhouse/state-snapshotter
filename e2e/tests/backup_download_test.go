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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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
	bkBackupDataDir    = "/backup"
)

type dataExportTarget struct {
	exportName   string
	group        string
	kind         string
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
					"requests": map[string]interface{}{"storage": "500Mi"},
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
				"size":                      "500Mi",
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

// writeBlockDataParallel creates one block-writer pod per PVC, waits for binding, writes random
// block data and records checksums concurrently. Safe with WaitForFirstConsumer: each pod is an
// independent first consumer for its PVC.
func writeBlockDataParallel(ctx context.Context, ns string, pvcs []string, checksums map[string]string) error {
	errCh := make(chan error, len(pvcs))
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, pvc := range pvcs {
		i, pvc := i, pvc
		podName := fmt.Sprintf("block-writer-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := suiteClientset.CoreV1().Pods(ns).Create(ctx, blockWriterPodSpec(ns, podName, pvc), metav1.CreateOptions{}); err != nil {
				errCh <- fmt.Errorf("create block writer pod for %s: %w", pvc, err)
				return
			}
			if err := waitPodRunning(ctx, ns, podName, 10*time.Minute); err != nil {
				errCh <- fmt.Errorf("wait block writer pod for %s: %w", pvc, err)
				return
			}
			sum, err := writeBlockAndChecksum(ctx, ns, podName, pvc)
			if err != nil {
				errCh <- err
				return
			}
			mu.Lock()
			checksums[pvc] = sum
			mu.Unlock()
			GinkgoWriter.Printf("  source checksum pvc=%s sha256=%s\n", pvc, sum)
			// Functional detach, not cleanup: the writer Pod must release the RWO PVC before the
			// volume can be snapshotted, so it deletes unconditionally regardless of keep-cluster knobs.
			forceDeletePod(ctx, ns, podName)
			if err := waitPodDeleted(ctx, ns, podName, 2*time.Minute); err != nil {
				errCh <- fmt.Errorf("delete block writer pod for %s: %w", pvc, err)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

func deletePod(ctx context.Context, ns, name string) {
	if cleanupSkipped() {
		GinkgoWriter.Printf("%s: keeping pod %s/%s\n", keepReason(), ns, name)
		return
	}
	forceDeletePod(ctx, ns, name)
}

// forceDeletePod deletes a Pod unconditionally, ignoring the keep-cluster knobs. Use it for functional
// detach steps (e.g. releasing an RWO PVC before snapshotting) where the test logic depends on the Pod
// actually being gone, as opposed to best-effort end-of-spec cleanup (use deletePod for that).
func forceDeletePod(ctx context.Context, ns, name string) {
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
	case "VolumeSnapshot":
		// Orphan-PVC visibility leaf: its captured PVC manifest is served by the generic-PVC extended
		// VolumeSnapshot connector (subresources.snapshot.storage.k8s.io), not the core/demo subresource.
		return vsConnectorSubPath(ns, ref.name, subManifestsDownload)
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

// forbiddenSnapshotKinds are dependent/runtime object kinds that a snapshot must never capture: their
// owning controller recreates them after restore, so their presence in any manifests-download payload
// means the capture allowlist leaked a dependent object (e.g. the demo VM's backing Pod). The download
// path returns raw manifests verbatim from the ManifestCheckpoint, so any such object here is a real
// capture regression rather than a download artefact.
var forbiddenSnapshotKinds = map[string]struct{}{
	"Pod": {},
}

func assertRawManifestsMatchLive(ctx context.Context, ns string, downloaded []unstructured.Unstructured) error {
	checked := 0
	for i := range downloaded {
		obj := &downloaded[i]
		if _, forbidden := forbiddenSnapshotKinds[obj.GetKind()]; forbidden {
			return fmt.Errorf(
				"dependent object %s/%s leaked into snapshot manifests-download: %s is recreated by its owner and must be excluded from capture",
				obj.GetKind(), obj.GetName(), obj.GetKind(),
			)
		}
		// The root's own-manifests now carry the namespace's own cluster-scoped Namespace object
		// (namespace-capture feature). It is cluster-scoped (empty metadata.namespace) and validated verbatim
		// by the dedicated namespace-capture spec, so it does not fit this namespaced raw-vs-live compare
		// (which keys on ns + a namespaced GVR). Skip it rather than trip the missing-GVR guard below.
		if obj.GetKind() == "Namespace" && obj.GetNamespace() == "" {
			continue
		}
		gvr, ok := gvrForLiveKind(obj.GetKind())
		if !ok {
			return fmt.Errorf("downloaded manifest %s/%s has no live GVR mapping for raw comparison", obj.GetKind(), obj.GetName())
		}
		if obj.GetNamespace() != ns {
			return fmt.Errorf("%s/%s metadata.namespace mismatch: want %q, got %q", obj.GetKind(), obj.GetName(), ns, obj.GetNamespace())
		}
		live, err := getResource(ctx, gvr, ns, obj.GetName())
		if err != nil {
			return fmt.Errorf("get live %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
		liveSum, liveJSON, err := canonicalManifestChecksum(strippedForRawCompare(live))
		if err != nil {
			return fmt.Errorf("checksum live %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
		downloadedSum, downloadedJSON, err := canonicalManifestChecksum(strippedForRawCompare(obj))
		if err != nil {
			return fmt.Errorf("checksum downloaded %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
		if liveSum != downloadedSum {
			return fmt.Errorf(
				"raw manifest %s/%s differs from live object\nlive_sha256=%s downloaded_sha256=%s\nlive=%s\ndownloaded=%s",
				obj.GetKind(), obj.GetName(), liveSum, downloadedSum,
				truncate(prettyManifestJSON(liveJSON), 4096),
				truncate(prettyManifestJSON(downloadedJSON), 4096),
			)
		}
		checked++
	}
	if checked == 0 {
		return fmt.Errorf("no downloaded manifests were checked against live objects")
	}
	return nil
}

// rawCompareVolatileMetadata are server-managed metadata fields that cannot stay stable between a
// point-in-time captured manifest and the live object read back later: resourceVersion is bumped on
// every write, and managedFields is server-side-apply bookkeeping whose timestamps/managers drift.
// Capture stores raw manifests verbatim (sanitization is a restore-path concern, see
// internal/usecase/restore/sanitizer.go), so the raw-vs-live checksum strips these from both sides to
// compare actual object content (spec/data/labels/annotations/ownerRefs) without false diffs.
//
// status is deliberately NOT stripped: the capture spec waits for every source object to reach its
// terminal Ready state before snapshotting (see "captures the Block-volume snapshot tree"), so the
// captured status equals the (now-stable) live status and is part of what this test verifies.
var rawCompareVolatileMetadata = [][]string{
	{"metadata", "resourceVersion"},
	{"metadata", "managedFields"},
}

// strippedForRawCompare returns a deep copy of obj's content with volatile server-managed metadata
// removed, so a faithful capture is not flagged as differing just because the live object advanced its
// resourceVersion (or rewrote managedFields) after the snapshot was taken.
func strippedForRawCompare(obj *unstructured.Unstructured) map[string]interface{} {
	clone := obj.DeepCopy()
	for _, path := range rawCompareVolatileMetadata {
		unstructured.RemoveNestedField(clone.Object, path...)
	}
	return clone.Object
}

func canonicalManifestChecksum(obj map[string]interface{}) (sum string, canonical []byte, err error) {
	canonical, err = json.Marshal(obj)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("%x", sha256.Sum256(canonical)), canonical, nil
}

func prettyManifestJSON(canonical []byte) []byte {
	var obj interface{}
	if err := json.Unmarshal(canonical, &obj); err != nil {
		return []byte(fmt.Sprintf("<unmarshal failed: %v>", err))
	}
	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return []byte(fmt.Sprintf("<marshal failed: %v>", err))
	}
	return out
}

// assertManifestsMatchLive verifies that each restored object's meaningful spec fields equal the live
// source object (looked up by name in ns). label prefixes every log line so parallel variants stay
// readable; per object it prints exactly which fields were compared and the only intended difference
// (the rewritten namespace), so a passing run produces a clear field-level diff against the original.
func assertManifestsMatchLive(ctx context.Context, label, ns string, downloaded []unstructured.Unstructured) error {
	logf := func(format string, args ...any) { GinkgoWriter.Printf("  ["+label+"] "+format+"\n", args...) }
	for i := range downloaded {
		obj := &downloaded[i]
		gvr, ok := gvrForLiveKind(obj.GetKind())
		if !ok {
			logf("manifest %s/%s: restored (kind has no field-level source comparison)", obj.GetKind(), obj.GetName())
			continue
		}
		live, err := getResource(ctx, gvr, ns, obj.GetName())
		if err != nil {
			return fmt.Errorf("get live %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
		var checked []string
		cmp := func(field, want, got string) error {
			if want != got {
				return fmt.Errorf("%s %s spec.%s mismatch: live=%q downloaded=%q", obj.GetKind(), obj.GetName(), field, want, got)
			}
			checked = append(checked, fmt.Sprintf("%s=%q", field, got))
			return nil
		}
		switch obj.GetKind() {
		case "ConfigMap":
			want, _, _ := unstructured.NestedStringMap(live.Object, "data")
			got, _, _ := unstructured.NestedStringMap(obj.Object, "data")
			if fmt.Sprint(want) != fmt.Sprint(got) {
				return fmt.Errorf("ConfigMap %s data mismatch: live=%v downloaded=%v", obj.GetName(), want, got)
			}
			checked = append(checked, fmt.Sprintf("data=%v", got))
		case "DemoVirtualMachine":
			want, _, _ := unstructured.NestedString(live.Object, "spec", "virtualDiskName")
			got, _, _ := unstructured.NestedString(obj.Object, "spec", "virtualDiskName")
			if err := cmp("virtualDiskName", want, got); err != nil {
				return err
			}
			if obj.GetName() == bkVMName && want != bkDiskAName {
				return fmt.Errorf("DemoVirtualMachine %s should reference %s, got %q", bkVMName, bkDiskAName, want)
			}
		case "DemoVirtualDisk":
			for _, key := range []string{"persistentVolumeClaimName", "storageClassName", "volumeMode", "size"} {
				want, _, _ := unstructured.NestedString(live.Object, "spec", key)
				got, _, _ := unstructured.NestedString(obj.Object, "spec", key)
				if err := cmp(key, want, got); err != nil {
					return err
				}
			}
		case "PersistentVolumeClaim":
			wantMode, _, _ := unstructured.NestedString(live.Object, "spec", "volumeMode")
			gotMode, _, _ := unstructured.NestedString(obj.Object, "spec", "volumeMode")
			if err := cmp("volumeMode", wantMode, gotMode); err != nil {
				return err
			}
			wantSC, _, _ := unstructured.NestedString(live.Object, "spec", "storageClassName")
			gotSC, _, _ := unstructured.NestedString(obj.Object, "spec", "storageClassName")
			if err := cmp("storageClassName", wantSC, gotSC); err != nil {
				return err
			}
			wantStor, _, _ := unstructured.NestedString(live.Object, "spec", "resources", "requests", "storage")
			gotStor, _, _ := unstructured.NestedString(obj.Object, "spec", "resources", "requests", "storage")
			if err := cmp("resources.requests.storage", wantStor, gotStor); err != nil {
				return err
			}
		}
		logf("manifest %s/%s matches source — checked {%s}; only diff: metadata.namespace %q -> %q",
			obj.GetKind(), obj.GetName(), strings.Join(checked, ", "), live.GetNamespace(), obj.GetNamespace())
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
	// Wait for the asynchronously-published orphan-PVC dataRef so we never miss a binding (see
	// waitContentDataRefs); reading once can race right after the snapshot children report Ready.
	bindings, err := waitContentDataRefs(ctx, rootContent, []string{bkPVCName, bkPVCDiskA, bkPVCDiskB}, suiteCfg.captureReadyTO)
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
			targetName, _, _ := unstructured.NestedString(content.Object, "status", "data", "source", "name")
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
			kind:         sk.kind,
			pvcName:      pvc,
			snapshotName: sk.name,
		}
		switch sk.kind {
		case "DemoVirtualDiskSnapshot":
			t.group = "demo.state-snapshotter.deckhouse.io"
		case "VolumeSnapshot":
			t.group = "snapshot.storage.k8s.io"
		default:
			return nil, fmt.Errorf("unsupported snapshot kind %q for PVC %q", sk.kind, pvc)
		}
		out = append(out, t)
	}
	return out, nil
}

func createDataExport(ctx context.Context, ns string, target dataExportTarget) error {
	de := &unstructured.Unstructured{Object: map[string]interface{}{
		// DataExport is served by storage-foundation, not state-snapshotter: derive the apiVersion from
		// dataExportGVR so the object body matches the resource endpoint (a mismatch is rejected by the
		// apiserver as "the API version in the data ... does not match the expected API version").
		"apiVersion": dataExportGVR.GroupVersion().String(),
		"kind":       "DataExport",
		"metadata": map[string]interface{}{
			"name":      target.exportName,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"ttl": "15m",
			"targetRef": map[string]interface{}{
				"group": target.group,
				"kind":  target.kind,
				"name":  target.snapshotName,
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
		// The data-exporter authorizes the download via a SubjectAccessReview against the DataExport's
		// own API group (storage-foundation.deckhouse.io) + the "download" subresource; the granted group
		// must match dataExportGVR.Group or the SAR denies and the exporter returns 403 (which curl -f
		// turns into an empty body).
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{dataExportGVR.Group},
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
	sizeLimit := resource.MustParse("64Mi")
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: bkBackupClientPod, Namespace: ns},
		Spec: corev1.PodSpec{
			ServiceAccountName: bkBackupClientSA,
			RestartPolicy:      corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name:    bkBackupClientCont,
				Image:   suiteCfg.backupClientImage,
				Command: []string{"sh", "-c", "sleep 360000"},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "backup",
					MountPath: bkBackupDataDir,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "backup",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &sizeLimit},
				},
			}},
		},
	}
}

// resolveBackupSnapRefs discovers snapshot leaf names and PVC bindings from the captured tree.
func resolveBackupSnapRefs(ctx context.Context, ns, rootSnap, rootContent string) error {
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
			diskBSnap = c
		}
	}
	if vmSnap.name == "" || diskBSnap.name == "" {
		return fmt.Errorf("expected VM snapshot and standalone disk snapshot among root children")
	}

	vmObj, err := getResource(ctx, demoVMSnapshotGVR, ns, vmSnap.name)
	if err != nil {
		return fmt.Errorf("get VM snapshot %s: %w", vmSnap.name, err)
	}
	var diskASnap string
	for _, c := range childSnapshotRefs(vmObj) {
		if c.kind == "DemoVirtualDiskSnapshot" {
			diskASnap = c.name
			break
		}
	}
	if diskASnap == "" {
		return fmt.Errorf("expected disk-a snapshot nested under VM snapshot %s", vmSnap.name)
	}

	// The orphan-PVC child volume node's dataRef is published asynchronously after its VolumeSnapshot
	// leaf becomes readyToUse, so poll the content tree until all three PVC bindings are linked under
	// the root content rather than reading once (which races right after waitChildrenReady).
	if _, err := waitContentDataRefs(ctx, rootContent, []string{bkPVCName, bkPVCDiskA, bkPVCDiskB}, suiteCfg.captureReadyTO); err != nil {
		return err
	}
	var orphanVS string
	list, lerr := suiteDyn.Resource(volumeSnapshotGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if lerr != nil {
		return fmt.Errorf("list VolumeSnapshots: %w", lerr)
	}
	for i := range list.Items {
		vs := &list.Items[i]
		contentName, _, _ := unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName")
		if contentName == "" {
			continue
		}
		content, gerr := getResource(ctx, snapshotContentGVR, "", contentName)
		if gerr != nil {
			continue
		}
		targetName, _, _ := unstructured.NestedString(content.Object, "status", "data", "source", "name")
		if targetName == bkPVCName {
			orphanVS = vs.GetName()
			break
		}
	}
	if orphanVS == "" {
		return fmt.Errorf("no VolumeSnapshot leaf found for orphan PVC %q", bkPVCName)
	}

	leafToPVC := map[string]string{
		diskASnap:      bkPVCDiskA,
		diskBSnap.name: bkPVCDiskB,
		orphanVS:       bkPVCName,
	}

	backup.vmSnapName = vmSnap.name
	backup.diskASnapName = diskASnap
	backup.diskBSnapName = diskBSnap.name
	backup.orphanVSName = orphanVS
	backup.leafToPVC = leafToPVC
	return nil
}

func downloadAndPersistBlock(ctx context.Context, ns, exportURL, destFile string) (string, error) {
	// status.url carries a trailing slash; trim it so we never emit "//api/v1/block" (Go's ServeMux would
	// 301-redirect the non-canonical path and curl, without -L, would hash the redirect body instead of
	// the device). sha256sum is the busybox/Alpine applet shipped by the default curlimages/curl image
	// (openssl CLI is NOT present there), and matches the source-side `dd | sha256sum` exactly.
	blockURL := strings.TrimRight(exportURL, "/") + "/api/v1/block"
	// -f makes curl fail (and suppress the body) on HTTP >=400 so an auth/SAR error doesn't get hashed as
	// if it were device bytes. We intentionally do NOT set pipefail: head closes the pipe after N bytes,
	// which SIGPIPEs curl by design; the pipeline's exit is sha256sum's (matching the source dd|sha256sum).
	dlCmd := fmt.Sprintf(
		`TOKEN=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token); curl -fksS -H "Authorization: Bearer $TOKEN" %q | head -c %d > %q && sha256sum %q | awk '{print $1}'`,
		blockURL, bkBlockBytes, destFile, destFile,
	)
	stdout, stderr, err := storagekube.ExecInPod(ctx, suiteRestCfg, ns, bkBackupClientPod, bkBackupClientCont, []string{"sh", "-c", dlCmd})
	if err != nil {
		return "", fmt.Errorf("download+persist block data: %w (stderr=%q)", err, stderr)
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
		var targets []dataExportTarget

		BeforeAll(func() {
			if !suiteCfg.volumeData {
				Skip("E2E_VOLUME_DATA not set: skipping the phase-4 backup download flow")
			}
			backup.sc = suiteCfg.storageClass
			backup.srcNS = uniqueNS("bk")
			backup.checksums = map[string]string{}
			backup.dataDir = bkBackupDataDir

			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
			defer cancel()

			By("Provisioning a thin, snapshot-capable default StorageClass via storage-e2e (" + backup.sc + ")")
			_, err := testkit.EnsureDefaultStorageClass(ctx, suiteRestCfg, testkit.DefaultStorageClassConfig{
				StorageClassName:     backup.sc,
				LVMType:              "Thin",
				ThinPoolName:         "thinpool",
				BaseKubeconfig:       suiteClusterResources.BaseKubeconfig,
				VMNamespace:          suiteCfg.vmNamespace,
				BaseStorageClassName: suiteCfg.baseStorageClass,
			})
			Expect(err).NotTo(HaveOccurred(), "provision default StorageClass")

			By("Wiring the StorageClass to a VolumeSnapshotClass for the local CSI driver")
			Expect(ensureStorageClassVolumeSnapshotClass(ctx, backup.sc)).To(Succeed())

			By("Creating the source namespace and applying the Block-volume demo source")
			Expect(ensureNamespace(ctx, backup.srcNS)).To(Succeed())
			Expect(applyObjects(ctx, buildBackupSource(backup.srcNS, backup.sc), backup.srcNS)).To(Succeed())

			// srcNS teardown is deferred to phase 5 (backup pod + persisted bytes must survive).

			// The provisioned StorageClass (sds-local-volume) is WaitForFirstConsumer: the demo scratch
			// PVCs and the orphan PVC only bind once a consumer Pod is scheduled. We therefore do NOT gate
			// on disk Ready / PVC Bound here (that would deadlock without a consumer); each per-PVC
			// block-writer Pod below is the first consumer that triggers binding, and waitPodRunning blocks
			// until the WaitForFirstConsumer bind completes.
			pvcs := []string{bkPVCName, bkPVCDiskA, bkPVCDiskB}
			By("Writing Block data and recording checksums for all PVCs in parallel")
			Expect(writeBlockDataParallel(ctx, backup.srcNS, pvcs, backup.checksums)).To(Succeed())

			By("Creating DemoVirtualMachine " + bkVMName + " attached to " + bkDiskAName)
			Expect(applyObjects(ctx, []*unstructured.Unstructured{buildBackupVM(backup.srcNS)}, backup.srcNS)).To(Succeed())
		})

		It("captures the Block-volume snapshot tree (Ready + expected topology)", func() {
			// Capture (LVM snapshot creation) is fast — bound by captureReadyTO, not the restore-path
			// snapshotReadyTO.
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+5*time.Minute)
			defer cancel()

			// Background capture timeline: surfaces where the Block-volume snapshot creation spends time.
			tl := startCaptureTimeline(backup.srcNS)
			defer tl.stop()

			// Snapshot captures status verbatim (point-in-time). Wait for every source object to reach its
			// terminal Ready state (status no longer changes) BEFORE snapshotting, so the Phase 4 raw
			// manifest-vs-live comparison stays sound: otherwise a captured status (e.g. VM phase=Pending,
			// disk still provisioning) would drift from the live object that keeps reconciling to Ready. The
			// VM's own Ready already implies its attached disk-a is usable, but disk-b is a standalone root
			// child, so all three are awaited explicitly.
			By("Waiting for the source objects to settle into a terminal Ready state before snapshotting")
			Expect(waitDemoDiskReady(ctx, backup.srcNS, bkDiskAName, suiteCfg.captureReadyTO)).
				To(Succeed(), "source DemoVirtualDisk %s Ready before snapshot", bkDiskAName)
			Expect(waitDemoDiskReady(ctx, backup.srcNS, bkDiskBName, suiteCfg.captureReadyTO)).
				To(Succeed(), "source DemoVirtualDisk %s Ready before snapshot", bkDiskBName)
			Expect(waitObjectCondition(ctx, demoVMGVR, backup.srcNS, bkVMName, condReady, "True", suiteCfg.captureReadyTO)).
				To(Succeed(), "source DemoVirtualMachine %s Ready before snapshot", bkVMName)

			By("Creating the root Snapshot over the Block-volume tree")
			Expect(createRootSnapshot(ctx, backup.srcNS, bkRootSnapshotName)).To(Succeed())

			By("Waiting for the Snapshot + bound SnapshotContent to become Ready")
			content, err := waitSnapshotReady(ctx, backup.srcNS, bkRootSnapshotName, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())
			backup.rootContent = content
			backup.rootSnap = bkRootSnapshotName

			By("Waiting for all child snapshots to reach Ready=True")
			nodes, err := walkSnapshotTree(ctx, backup.srcNS, bkRootSnapshotName)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).NotTo(BeEmpty())
			Expect(waitChildrenReady(ctx, backup.srcNS, nodes, suiteCfg.captureReadyTO)).To(Succeed())

			By("Resolving snapshot leaf names for the phase-5 import tree")
			Expect(resolveBackupSnapRefs(ctx, backup.srcNS, bkRootSnapshotName, content)).To(Succeed())

			By("Asserting disk-a is nested under the VM snapshot and disk-b is a standalone root child")
			Expect(assertBackupTopology(ctx, backup.srcNS, bkRootSnapshotName)).To(Succeed())
		})

		It("downloads manifests via the aggregated API and matches live cluster objects", func() {
			Expect(backup.rootContent).NotTo(BeEmpty(), "capture spec must have populated rootContent")

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			rootSnap := bkRootSnapshotName
			nodes, err := walkSnapshotTree(ctx, backup.srcNS, rootSnap)
			Expect(err).NotTo(HaveOccurred())

			allRefs := append([]childRef{{kind: "Snapshot", name: rootSnap}}, nodes...)
			for _, ref := range allRefs {
				path := manifestDownloadPath(backup.srcNS, ref)
				if path == "" {
					continue
				}
				By("Downloading manifests for " + ref.kind + "/" + ref.name)
				body, gerr := aggGet(ctx, path, nil)
				Expect(gerr).NotTo(HaveOccurred(), "GET %s", path)
				objs, derr := decodeManifestArray(body)
				Expect(derr).NotTo(HaveOccurred())
				Expect(objs).NotTo(BeEmpty(), "manifests-download for %s/%s", ref.kind, ref.name)
				Expect(assertRawManifestsMatchLive(ctx, backup.srcNS, objs)).To(Succeed())
			}
		})

		It("downloads volume bytes via DataExport and matches source checksums", func() {
			Expect(backup.rootContent).NotTo(BeEmpty(), "capture spec must have populated rootContent")

			// Budget: 3 legs x (dataTransferTO DataExport Ready + download) + 5m backup-pod start, with
			// headroom. A wedged DataExport fails on its own dataTransferTO deadline rather than dragging
			// the whole spec to a giant fixed cap.
			ctx, cancel := context.WithTimeout(context.Background(), 3*suiteCfg.dataTransferTO+15*time.Minute)
			defer cancel()

			By("Checking the extended-VS data surface is available (skip if the fork is absent)")
			hasLeaf, err := anyBoundSnapshotContent(ctx, backup.srcNS)
			Expect(err).NotTo(HaveOccurred())
			if !hasLeaf {
				Skip("no snapshot leaf exposes status.boundSnapshotContentName: extended-VS data surface unavailable")
			}

			By("Collecting DataExport targets from the captured content tree")
			targets, err = collectDataExportTargets(ctx, backup.srcNS, backup.rootContent)
			Expect(err).NotTo(HaveOccurred())
			Expect(targets).To(HaveLen(3), "expected data exports for orphan PVC + two demo disks")

			By("Creating backup-client RBAC and pod")
			Expect(ensureBackupClientRBAC(ctx, backup.srcNS)).To(Succeed())
			_, err = suiteClientset.CoreV1().Pods(backup.srcNS).Create(ctx, backupClientPodSpec(backup.srcNS), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create backup-client pod")
			Expect(waitPodRunning(ctx, backup.srcNS, bkBackupClientPod, 5*time.Minute)).To(Succeed())

			for _, target := range targets {
				By(fmt.Sprintf("Exporting PVC %s via DataExport %s (target %s/%s)", target.pvcName, target.exportName, target.kind, target.snapshotName))
				Expect(createDataExport(ctx, backup.srcNS, target)).To(Succeed())
				DeferCleanup(func(t dataExportTarget) func() {
					return func() {
						if cleanupSkipped() {
							GinkgoWriter.Printf("%s: keeping DataExport %s/%s\n", keepReason(), backup.srcNS, t.exportName)
							return
						}
						cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
						defer ccancel()
						deleteDataExport(cctx, backup.srcNS, t.exportName)
					}
				}(target))

				url, _, werr := waitDataExportReady(ctx, backup.srcNS, target.exportName, suiteCfg.dataTransferTO)
				Expect(werr).NotTo(HaveOccurred(), "DataExport %s Ready", target.exportName)
				GinkgoWriter.Printf("  DataExport %s url=%s\n", target.exportName, url)

				destFile := fmt.Sprintf("%s/%s.bin", backup.dataDir, target.pvcName)
				got, derr := downloadAndPersistBlock(ctx, backup.srcNS, url, destFile)
				Expect(derr).NotTo(HaveOccurred(), "download+persist block data for PVC %s", target.pvcName)
				want := backup.checksums[target.pvcName]
				Expect(want).NotTo(BeEmpty(), "source checksum for PVC %s", target.pvcName)
				Expect(got).To(Equal(want), "downloaded bytes for PVC %s must match source checksum", target.pvcName)

				deleteDataExport(ctx, backup.srcNS, target.exportName)
			}
			backup.ready = true
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
