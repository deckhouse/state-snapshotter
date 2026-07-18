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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

const (
	bkImportRootName   = "import-root"
	bkRestoreProbePod  = "restore-probe"
	bkRestoreProbeCont = "probe"
	bkProbeDevicePath  = "/dev/xvda"
	bkDataImportTTL    = "15m"
	vsAPIVersion       = "snapshot.storage.k8s.io/v1"

	bkImpNSVS   = "bk-imp-vs"
	bkImpNSDisk = "bk-imp-disk"
	bkImpNSVM   = "bk-imp-vm"
	bkImpNSFull = "bk-imp-full"
)

// importNode describes one node in an import subtree (nested via children).
type importNode struct {
	name       string
	kind       string
	group      string
	apiVersion string
	manifests  []byte
	children   []*importNode
	dataLeaf   bool
	pvcName    string
}

// importNodeOwnerRef returns the child->parent ownerRef the import binder uses to resolve the parent's
// SnapshotContent (mirrors d8-cli snapimport.parentOwnerReference). VolumeSnapshot leaves omit controller.
func importNodeOwnerRef(parentAPIVersion, parentKind, parentName string, parentUID types.UID, childIsVolumeSnapshot bool) metav1.OwnerReference {
	ref := metav1.OwnerReference{
		APIVersion: parentAPIVersion,
		Kind:       parentKind,
		Name:       parentName,
		UID:        parentUID,
	}
	if !childIsVolumeSnapshot {
		controller := true
		ref.Controller = &controller
	}
	return ref
}

func ownerReferencesField(refs []metav1.OwnerReference) []interface{} {
	out := make([]interface{}, 0, len(refs))
	for _, ref := range refs {
		m := map[string]interface{}{
			"apiVersion": ref.APIVersion,
			"kind":       ref.Kind,
			"name":       ref.Name,
			"uid":        string(ref.UID),
		}
		if ref.Controller != nil && *ref.Controller {
			m["controller"] = true
		}
		out = append(out, m)
	}
	return out
}

func getSnapshotUID(ctx context.Context, ns, name string) (types.UID, error) {
	obj, err := getResource(ctx, snapshotGVR, ns, name)
	if err != nil {
		return "", err
	}
	return obj.GetUID(), nil
}

func getImportNodeUID(ctx context.Context, ns string, node *importNode) (types.UID, error) {
	var gvr schema.GroupVersionResource
	switch node.kind {
	case "DemoVirtualMachineSnapshot":
		gvr = demoVMSnapshotGVR
	case "DemoVirtualDiskSnapshot":
		gvr = demoDiskSnapshotGVR
	case "VolumeSnapshot":
		gvr = volumeSnapshotGVR
	default:
		return "", fmt.Errorf("getImportNodeUID: unsupported kind %q", node.kind)
	}
	obj, err := getResource(ctx, gvr, ns, node.name)
	if err != nil {
		return "", err
	}
	return obj.GetUID(), nil
}

// createImportVMSnapshot creates an import-mode DemoVirtualMachineSnapshot (structural node, no DataImport).
func createImportVMSnapshot(ctx context.Context, ns, name string, ownerRefs []metav1.OwnerReference) error {
	meta := map[string]interface{}{
		"name":      name,
		"namespace": ns,
	}
	if len(ownerRefs) > 0 {
		meta["ownerReferences"] = ownerReferencesField(ownerRefs)
	}
	snap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualMachineSnapshot",
		"metadata":   meta,
		"spec": map[string]interface{}{
			"mode": "Import",
		},
	}}
	_, err := suiteDyn.Resource(demoVMSnapshotGVR).Namespace(ns).Create(ctx, snap, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// createImportDiskSnapshot creates an import-mode DemoVirtualDiskSnapshot. Import is signalled by the
// enum marker spec.mode: Import — no sourceRef (the live disk is absent on import) and no DataImport name
// on the leaf: the binder finds the DataImport by reverse-lookup on spec.snapshotRef.
func createImportDiskSnapshot(ctx context.Context, ns, name string, ownerRefs []metav1.OwnerReference) error {
	meta := map[string]interface{}{
		"name":      name,
		"namespace": ns,
	}
	if len(ownerRefs) > 0 {
		meta["ownerReferences"] = ownerReferencesField(ownerRefs)
	}
	snap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualDiskSnapshot",
		"metadata":   meta,
		"spec": map[string]interface{}{
			"mode": "Import",
		},
	}}
	_, err := suiteDyn.Resource(demoDiskSnapshotGVR).Namespace(ns).Create(ctx, snap, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func createImportVolumeSnapshot(ctx context.Context, ns, name string, ownerRefs []metav1.OwnerReference) error {
	meta := map[string]interface{}{
		"name":      name,
		"namespace": ns,
	}
	if len(ownerRefs) > 0 {
		meta["ownerReferences"] = ownerReferencesField(ownerRefs)
	}
	// Import is the same enum spec.mode: Import as on every other snapshot kind (the fork CRD hosts the
	// field); source is omitted entirely — it is required only when mode != Import (symmetry: the data
	// artifact comes from the owning DataImport, there is no CSI source).
	vs := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": vsAPIVersion,
		"kind":       "VolumeSnapshot",
		"metadata":   meta,
		"spec": map[string]interface{}{
			"mode": "Import",
		},
	}}
	_, err := suiteDyn.Resource(volumeSnapshotGVR).Namespace(ns).Create(ctx, vs, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func createDataImport(ctx context.Context, ns, name, apiVersion, kind, leafName, storageClassName, size, volumeMode string) error {
	storageParams := map[string]interface{}{
		"storageClassName": storageClassName,
		"size":             size,
	}
	if volumeMode != "" {
		storageParams["volumeMode"] = volumeMode
	}
	spec := map[string]interface{}{
		"ttl":  bkDataImportTTL,
		"mode": "PopulateData",
		"snapshotRef": map[string]interface{}{
			"apiVersion": apiVersion,
			"kind":       kind,
			"name":       leafName,
		},
		"storageParams": storageParams,
	}
	di := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "storage-foundation.deckhouse.io/v1alpha1",
		"kind":       "DataImport",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
		},
		"spec": spec,
	}}
	_, err := suiteDyn.Resource(dataImportGVR).Namespace(ns).Create(ctx, di, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func sourcePVCScratchParams(ctx context.Context, ns, pvcName string) (storageClassName, size, volumeMode string, err error) {
	pvc, gErr := suiteClientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pvcName, metav1.GetOptions{})
	if gErr != nil {
		return "", "", "", fmt.Errorf("get source PVC %s/%s for DataImport params: %w", ns, pvcName, gErr)
	}
	if pvc.Spec.StorageClassName != nil {
		storageClassName = *pvc.Spec.StorageClassName
	}
	if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		size = q.String()
	}
	if pvc.Spec.VolumeMode != nil {
		volumeMode = string(*pvc.Spec.VolumeMode)
	}
	return storageClassName, size, volumeMode, nil
}

func deleteDataImport(ctx context.Context, ns, name string) {
	_ = suiteDyn.Resource(dataImportGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
}

// createAbandonedImportPVC creates a self-contained CreatePVC DataImport (it provisions its own PVC via
// pvcTemplate, needing no snapshotRef leaf) with an explicit short idle-TTL. The idle-expiry spec never
// uploads to it, so the importer server comes up and then idle-expires — exercising the DI-specific
// spec.ttl→--ttl plumbing and the controller/populator teardown interplay end to end.
//
// waitForFirstConsumer=false is REQUIRED here: for a WaitForFirstConsumer StorageClass (the e2e suite's
// SC), the controller creates a load ("dummy") pod to bind the PVC only when waitForFirstConsumer is
// false (NeedConsumer = scWffc && !waitForFirstConsumer, pvc.go). An abandoned import has no real
// consumer, so with true the PVC would never bind, the server would never come up, and the spec would
// hang — the opposite of the intended idle-expiry.
func createAbandonedImportPVC(ctx context.Context, ns, name, storageClassName, size, volumeMode, ttl string) error {
	pvcSpec := map[string]interface{}{
		"accessModes":      []interface{}{"ReadWriteOnce"},
		"storageClassName": storageClassName,
		"resources":        map[string]interface{}{"requests": map[string]interface{}{"storage": size}},
	}
	if volumeMode != "" {
		pvcSpec["volumeMode"] = volumeMode
	}
	di := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "storage-foundation.deckhouse.io/v1alpha1",
		"kind":       "DataImport",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"ttl":  ttl,
			"mode": "CreatePVC",
			// false → the controller creates a load pod to bind a WaitForFirstConsumer PVC (there is no real
			// consumer for an abandoned import). See the doc comment above.
			"waitForFirstConsumer": false,
			"pvcTemplate": map[string]interface{}{
				"metadata": map[string]interface{}{"name": name},
				"spec":     pvcSpec,
			},
		},
	}}
	_, err := suiteDyn.Resource(dataImportGVR).Namespace(ns).Create(ctx, di, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// countDataManagerServerPods counts the importer/exporter server pods that belong to the named
// DataImport/DataExport (they run in the data-manager namespace and carry the storage-manager-name
// annotation). It is used to assert server-infrastructure teardown after idle expiry.
func countDataManagerServerPods(ctx context.Context, storageManagerName string) (int, error) {
	pods, err := suiteClientset.CoreV1().Pods(d8DataManagerNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range pods.Items {
		if pods.Items[i].Annotations["storage-foundation.deckhouse.io/storage-manager-name"] == storageManagerName {
			n++
		}
	}
	return n, nil
}

func waitDataImportReady(ctx context.Context, ns, name string, timeout time.Duration) (url, ca string, err error) {
	deadline := time.Now().Add(timeout)
	var last string
	var polls int
	for {
		obj, gerr := getResource(ctx, dataImportGVR, ns, name)
		if gerr == nil {
			st, reason, found := conditionStatus(obj, "Ready")
			volMode, _, _ := unstructured.NestedString(obj.Object, "status", "volumeMode")
			if found && st == "True" {
				url, _, _ = unstructured.NestedString(obj.Object, "status", "url")
				ca, _, _ = unstructured.NestedString(obj.Object, "status", "ca")
				if url != "" && volMode == "Block" && ca != "" {
					return url, ca, nil
				}
				last = fmt.Sprintf("Ready=True but url/volumeMode/ca incomplete (url=%q volumeMode=%q ca=%t)", url, volMode, ca != "")
			} else {
				last = fmt.Sprintf("Ready=%v reason=%q volumeMode=%q", st, reason, volMode)
			}
		} else {
			last = fmt.Sprintf("get err=%v", gerr)
		}
		polls++
		if polls == 1 || polls%12 == 0 {
			GinkgoWriter.Printf("  DataImport %s/%s wait: %s\n", ns, name, last)
		}
		if time.Now().After(deadline) {
			dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Minute)
			dumpStuckDataImportDiagnostics(dctx, ns, name)
			dcancel()
			return "", "", fmt.Errorf("timeout waiting for DataImport %s/%s Ready; last: %s", ns, name, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return "", "", ctx.Err()
		}
	}
}

func waitDataImportCompleted(ctx context.Context, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		obj, gerr := getResource(ctx, dataImportGVR, ns, name)
		if gerr == nil {
			st, reason, found := conditionStatus(obj, "Completed")
			artifact, artFound, _ := unstructured.NestedMap(obj.Object, "status", "data", "artifact")
			// New status model: a completed DataImport also carries status.phase=Completed and a
			// status.completionTimestamp (stamped once by the controller at the terminal transition; the GC
			// measures retention from it). The controller writes all three in the same status update, so
			// gating on them together does not add a race.
			phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
			completionTS, _, _ := unstructured.NestedString(obj.Object, "status", "completionTimestamp")
			// The DataImport catalog is exactly {Ready, UploadFinished, Completed} — no standalone Expired
			// and no stale/legacy condition type survives (a completed object is terminal and stable, so
			// this is not racy). extra lists any out-of-catalog types for diagnostics.
			catalogOK, extra := conditionsWithinCatalog(obj, "Ready", "UploadFinished", "Completed")
			if found && st == "True" && artFound && len(artifact) > 0 && phase == "Completed" && completionTS != "" && catalogOK {
				return nil
			}
			last = fmt.Sprintf("Completed=%v reason=%q artifact=%v phase=%q completionTimestamp=%q extraConditions=%v", st, reason, artFound, phase, completionTS, extra)
		} else {
			last = fmt.Sprintf("get err=%v", gerr)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for DataImport %s/%s Completed; last: %s", ns, name, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// conditionsWithinCatalog reports whether every status condition type on obj is in the allowed set,
// returning the list of out-of-catalog types found (empty when clean). It pins the DataImport/DataExport
// condition catalog end to end: no legacy "Expired" condition and no foreign type may survive.
func conditionsWithinCatalog(obj *unstructured.Unstructured, allowed ...string) (bool, []string) {
	allow := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		allow[a] = struct{}{}
	}
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	var extra []string
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _, _ := unstructured.NestedString(m, "type")
		if _, ok := allow[typ]; !ok {
			extra = append(extra, typ)
		}
	}
	return len(extra) == 0, extra
}

// waitDataImportPhase polls a DataImport until status.phase equals wantPhase (controller-owned).
func waitDataImportPhase(ctx context.Context, ns, name, wantPhase string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		obj, gerr := getResource(ctx, dataImportGVR, ns, name)
		if gerr == nil {
			phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
			if phase == wantPhase {
				return nil
			}
			last = fmt.Sprintf("phase=%q", phase)
		} else {
			last = fmt.Sprintf("get err=%v", gerr)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for DataImport %s/%s phase=%s; last: %s", ns, name, wantPhase, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

func ensureUploadRBAC(ctx context.Context, importNS, clientSANamespace, clientSAName string) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: bkBackupClientSA, Namespace: importNS},
		// The data-importer authorizes the upload via a SubjectAccessReview against the DataImport's own
		// API group (storage-foundation.deckhouse.io) + the "download" subresource; the granted group must
		// match dataImportGVR.Group or the SAR denies and the importer returns 403 on the PUT.
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{dataImportGVR.Group},
			Resources: []string{"dataimports/download"},
			Verbs:     []string{"create"},
		}},
	}
	if _, err := suiteClientset.RbacV1().Roles(importNS).Create(ctx, role, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create upload Role: %w", err)
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bkBackupClientSA, Namespace: importNS},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      clientSAName,
			Namespace: clientSANamespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     bkBackupClientSA,
		},
	}
	if _, err := suiteClientset.RbacV1().RoleBindings(importNS).Create(ctx, binding, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create upload RoleBinding: %w", err)
	}
	return nil
}

func uploadBlockData(ctx context.Context, importNS, importURL, srcFile string) error {
	blockURL := strings.TrimRight(importURL, "/") + "/api/v1/block"
	finishedURL := strings.TrimRight(importURL, "/") + "/api/v1/finished"
	putCmd := fmt.Sprintf(
		`TOKEN=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token); curl -fksS -X PUT -H "Authorization: Bearer $TOKEN" -H "X-Content-Length: %d" -H "X-Offset: 0" -H "X-Attribute-Permissions: 0644" -H "X-Attribute-Uid: 0" -H "X-Attribute-Gid: 0" --data-binary @%q %q`,
		bkBlockBytes, srcFile, blockURL,
	)
	stdout, stderr, err := storagekube.ExecInPod(ctx, suiteRestCfg, backup.srcNS, bkBackupClientPod, bkBackupClientCont, []string{"sh", "-c", putCmd})
	if err != nil {
		return fmt.Errorf("PUT block data: %w (stdout=%q stderr=%q)", err, stdout, stderr)
	}
	postCmd := fmt.Sprintf(
		`TOKEN=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token); curl -fksS -X POST -H "Authorization: Bearer $TOKEN" %q`,
		finishedURL,
	)
	stdout, stderr, err = storagekube.ExecInPod(ctx, suiteRestCfg, backup.srcNS, bkBackupClientPod, bkBackupClientCont, []string{"sh", "-c", postCmd})
	if err != nil {
		return fmt.Errorf("POST finished for DataImport in %s: %w (stdout=%q stderr=%q)", importNS, err, stdout, stderr)
	}
	return nil
}

func readBlockChecksum(ctx context.Context, ns, podName, container, devicePath string) (string, error) {
	cmd := fmt.Sprintf(
		"dd if=%s bs=1M count=%d 2>/dev/null | sha256sum | awk '{print $1}'",
		devicePath, bkBlockMiB,
	)
	stdout, stderr, err := storagekube.ExecInPod(ctx, suiteRestCfg, ns, podName, container, []string{"sh", "-c", cmd})
	if err != nil {
		return "", fmt.Errorf("read block checksum from %s: %w (stderr=%q)", devicePath, err, stderr)
	}
	sum := strings.TrimSpace(stdout)
	if len(sum) != 64 {
		return "", fmt.Errorf("unexpected sha256 from %s: %q (stderr=%q)", devicePath, sum, stderr)
	}
	return sum, nil
}

func restoreProbePodSpec(ns, podName, pvc, devicePath string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:          bkRestoreProbeCont,
				Image:         suiteCfg.probeImage,
				Command:       []string{"sh", "-c", "sleep 3600"},
				VolumeDevices: []corev1.VolumeDevice{{Name: pvc, DevicePath: devicePath}},
			}},
			Volumes: []corev1.Volume{{
				Name: pvc,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc},
				},
			}},
		},
	}
}

func restoreProbePodName(pvc string) string { return bkRestoreProbePod + "-" + pvc }

func emptyJSONArray() []byte { return []byte("[]") }

func fetchVMSnapManifests(ctx context.Context, name string) ([]byte, error) {
	return aggGet(ctx, demoSubPath(backup.srcNS, resDemoVMSnapshots, name, subManifestsDownload), nil)
}

func fetchDiskSnapManifests(ctx context.Context, name string) ([]byte, error) {
	return aggGet(ctx, demoSubPath(backup.srcNS, resDemoDiskSnapshots, name, subManifestsDownload), nil)
}

func fetchVSManifests(ctx context.Context, name string) ([]byte, error) {
	return aggGet(ctx, vsConnectorSubPath(backup.srcNS, name, subManifestsDownload), nil)
}

func fetchRootSnapManifests(ctx context.Context) ([]byte, error) {
	return aggGet(ctx, coreSnapshotSubPath(backup.srcNS, backup.rootSnap, subManifestsDownload), nil)
}

func newDiskImportNode(ctx context.Context, snapName string) (*importNode, error) {
	manifests, err := fetchDiskSnapManifests(ctx, snapName)
	if err != nil {
		return nil, fmt.Errorf("GET disk snapshot %s manifests: %w", snapName, err)
	}
	pvcName := backup.leafToPVC[snapName]
	if pvcName == "" {
		return nil, fmt.Errorf("no source PVC mapped for disk snapshot %q", snapName)
	}
	return &importNode{
		name:       snapName,
		kind:       "DemoVirtualDiskSnapshot",
		group:      "demo.state-snapshotter.deckhouse.io",
		apiVersion: demoGroupVersion,
		manifests:  manifests,
		dataLeaf:   true,
		pvcName:    pvcName,
	}, nil
}

func newVSImportNode(ctx context.Context, vsName string) (*importNode, error) {
	manifests, err := fetchVSManifests(ctx, vsName)
	if err != nil {
		return nil, fmt.Errorf("GET VolumeSnapshot %s manifests: %w", vsName, err)
	}
	pvcName := backup.leafToPVC[vsName]
	if pvcName == "" {
		return nil, fmt.Errorf("no source PVC mapped for VolumeSnapshot %q", vsName)
	}
	return &importNode{
		name:       vsName,
		kind:       "VolumeSnapshot",
		group:      "snapshot.storage.k8s.io",
		apiVersion: vsAPIVersion,
		manifests:  manifests,
		dataLeaf:   true,
		pvcName:    pvcName,
	}, nil
}

func newVMImportNode(ctx context.Context, snapName string, children ...*importNode) (*importNode, error) {
	manifests, err := fetchVMSnapManifests(ctx, snapName)
	if err != nil {
		return nil, fmt.Errorf("GET VM snapshot %s manifests: %w", snapName, err)
	}
	return &importNode{
		name:       snapName,
		kind:       "DemoVirtualMachineSnapshot",
		group:      "demo.state-snapshotter.deckhouse.io",
		apiVersion: demoGroupVersion,
		manifests:  manifests,
		children:   children,
	}, nil
}

func importNodeChildRefs(children []*importNode) []childRef {
	refs := make([]childRef, 0, len(children))
	for _, c := range children {
		refs = append(refs, childRef{apiVersion: c.apiVersion, kind: c.kind, name: c.name})
	}
	return refs
}

func importNodeUploadPath(ns string, node *importNode) string {
	switch node.kind {
	case "DemoVirtualMachineSnapshot":
		return coreGenericSubPath(ns, resDemoVMSnapshots, node.name, subManifestsUpload)
	case "DemoVirtualDiskSnapshot":
		return coreGenericSubPath(ns, resDemoDiskSnapshots, node.name, subManifestsUpload)
	case "VolumeSnapshot":
		return vsConnectorSubPath(ns, node.name, subManifestsUpload)
	default:
		return ""
	}
}

func postUploadWithRetry(ctx context.Context, path string, body []byte) error {
	deadline := time.Now().Add(2 * time.Minute)
	var lastErr error
	for {
		err := aggPost(ctx, path, body)
		if err == nil {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("POST %s: %w", path, lastErr)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

func materializeImportNode(ctx context.Context, ns string, node *importNode, parentRef *metav1.OwnerReference) error {
	var ownerRefs []metav1.OwnerReference
	if parentRef != nil {
		ownerRefs = []metav1.OwnerReference{*parentRef}
	}
	var createErr error
	switch node.kind {
	case "DemoVirtualMachineSnapshot":
		createErr = createImportVMSnapshot(ctx, ns, node.name, ownerRefs)
	case "DemoVirtualDiskSnapshot":
		createErr = createImportDiskSnapshot(ctx, ns, node.name, ownerRefs)
	case "VolumeSnapshot":
		createErr = createImportVolumeSnapshot(ctx, ns, node.name, ownerRefs)
	default:
		return fmt.Errorf("materializeImportNode: unsupported kind %q", node.kind)
	}
	if createErr != nil {
		return createErr
	}
	uid, err := getImportNodeUID(ctx, ns, node)
	if err != nil {
		return err
	}
	if node.dataLeaf {
		scName, scSize, scMode, perr := sourcePVCScratchParams(ctx, backup.srcNS, node.pvcName)
		if perr != nil {
			return perr
		}
		if err := createDataImport(ctx, ns, node.name, node.apiVersion, node.kind, node.name, scName, scSize, scMode); err != nil {
			return err
		}
	}
	for _, child := range node.children {
		childRef := importNodeOwnerRef(node.apiVersion, node.kind, node.name, uid, child.kind == "VolumeSnapshot")
		if err := materializeImportNode(ctx, ns, child, &childRef); err != nil {
			return err
		}
	}
	return nil
}

func materializeImportTree(ctx context.Context, ns, rootName string, rootUID types.UID, children []*importNode) error {
	for _, child := range children {
		parentRef := importNodeOwnerRef("state-snapshotter.deckhouse.io/v1alpha1", "Snapshot", rootName, rootUID, child.kind == "VolumeSnapshot")
		if err := materializeImportNode(ctx, ns, child, &parentRef); err != nil {
			return err
		}
	}
	return nil
}

func uploadImportNode(ctx context.Context, ns string, node *importNode) error {
	for _, child := range node.children {
		if err := uploadImportNode(ctx, ns, child); err != nil {
			return err
		}
	}
	body, err := buildUploadBody(node.manifests, importNodeChildRefs(node.children))
	if err != nil {
		return err
	}
	path := importNodeUploadPath(ns, node)
	if path == "" {
		return fmt.Errorf("upload path for %s/%s is empty", node.kind, node.name)
	}
	return postUploadWithRetry(ctx, path, body)
}

func uploadImportTree(ctx context.Context, ns, rootName string, rootManifests []byte, children []*importNode) error {
	for _, child := range children {
		if err := uploadImportNode(ctx, ns, child); err != nil {
			return err
		}
	}
	body, err := buildUploadBody(rootManifests, importNodeChildRefs(children))
	if err != nil {
		return err
	}
	path := coreSnapshotSubPath(ns, rootName, subManifestsUpload)
	return postUploadWithRetry(ctx, path, body)
}

func collectDataLeaves(nodes []*importNode) []*importNode {
	var out []*importNode
	var walk func(*importNode)
	walk = func(n *importNode) {
		if n.dataLeaf {
			out = append(out, n)
		}
		for _, c := range n.children {
			walk(c)
		}
	}
	for _, n := range nodes {
		walk(n)
	}
	return out
}

func uploadDataLeaves(ctx context.Context, importNS string, leaves []*importNode) error {
	for _, leaf := range leaves {
		url, _, werr := waitDataImportReady(ctx, importNS, leaf.name, suiteCfg.dataTransferTO)
		if werr != nil {
			return fmt.Errorf("DataImport %s Ready: %w", leaf.name, werr)
		}
		srcFile := fmt.Sprintf("%s/%s.bin", backup.dataDir, leaf.pvcName)
		if err := uploadBlockData(ctx, importNS, url, srcFile); err != nil {
			return err
		}
		if err := waitDataImportCompleted(ctx, importNS, leaf.name, suiteCfg.dataTransferTO); err != nil {
			return err
		}
	}
	return nil
}

func verifyRestoredBlockData(ctx context.Context, label, importNS string, verifyPVCs []string) error {
	logf := func(format string, args ...any) { GinkgoWriter.Printf("  ["+label+"] "+format+"\n", args...) }
	for _, pvc := range verifyPVCs {
		podName := restoreProbePodName(pvc)
		_, err := suiteClientset.CoreV1().Pods(importNS).Create(ctx, restoreProbePodSpec(importNS, podName, pvc, bkProbeDevicePath), metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create restore probe pod for PVC %s: %w", pvc, err)
		}
	}
	for _, pvc := range verifyPVCs {
		podName := restoreProbePodName(pvc)
		if err := waitPodRunning(ctx, importNS, podName, 15*time.Minute); err != nil {
			return fmt.Errorf("probe pod for PVC %s Running: %w", pvc, err)
		}
		got, rerr := readBlockChecksum(ctx, importNS, podName, bkRestoreProbeCont, bkProbeDevicePath)
		if rerr != nil {
			return fmt.Errorf("checksum restored PVC %s: %w", pvc, rerr)
		}
		want := backup.checksums[pvc]
		if want == "" {
			return fmt.Errorf("source checksum for PVC %s is empty", pvc)
		}
		if got != want {
			return fmt.Errorf("restored PVC %s bytes mismatch: got %s want %s", pvc, got, want)
		}
		logf("data PVC %s: restored sha256=%s == source sha256=%s — MATCH", pvc, got, want)
	}
	return nil
}

// logImportTree prints the subtree being imported, one indented line per node, marking structural nodes
// vs data leaves (with their source PVC) so the captured topology of each variant is visible.
func logImportTree(label string, nodes []*importNode, depth int) {
	indent := strings.Repeat("  ", depth)
	for _, n := range nodes {
		role := "structural"
		if n.dataLeaf {
			role = fmt.Sprintf("data leaf -> PVC %s", n.pvcName)
		}
		GinkgoWriter.Printf("  [%s] %s%s/%s (%s)\n", label, indent, n.kind, n.name, role)
		if len(n.children) > 0 {
			logImportTree(label, n.children, depth+1)
		}
	}
}

func dataLeafSummary(leaves []*importNode) string {
	if len(leaves) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(leaves))
	for _, l := range leaves {
		parts = append(parts, fmt.Sprintf("%s(PVC %s)", l.name, l.pvcName))
	}
	return strings.Join(parts, ", ")
}

func restoredObjSummary(objs []unstructured.Unstructured) string {
	parts := make([]string, 0, len(objs))
	for i := range objs {
		parts = append(parts, fmt.Sprintf("%s/%s", objs[i].GetKind(), objs[i].GetName()))
	}
	return strings.Join(parts, ", ")
}

// runImportVariant imports one subtree variant into importNS and verifies manifests + block data.
// label prefixes every log line so the four parallel variants stay readable in interleaved output.
func runImportVariant(ctx context.Context, label, importNS string, rootManifests []byte, children []*importNode, verifyPVCs []string) error {
	logf := func(format string, args ...any) { GinkgoWriter.Printf("  ["+label+"] "+format+"\n", args...) }

	logf("START ns=%s — importing tree:", importNS)
	logImportTree(label, children, 1)

	if err := createImportRootSnapshot(ctx, importNS, bkImportRootName); err != nil {
		return fmt.Errorf("create import-root: %w", err)
	}
	rootUID, err := getSnapshotUID(ctx, importNS, bkImportRootName)
	if err != nil {
		return fmt.Errorf("read import-root UID: %w", err)
	}
	if err := materializeImportTree(ctx, importNS, bkImportRootName, rootUID, children); err != nil {
		return fmt.Errorf("materialize import tree: %w", err)
	}
	if err := uploadImportTree(ctx, importNS, bkImportRootName, rootManifests, children); err != nil {
		return fmt.Errorf("upload manifests: %w", err)
	}
	leaves := collectDataLeaves(children)
	logf("manifests uploaded; streaming %d volume data leaf(s): %s", len(leaves), dataLeafSummary(leaves))
	if err := uploadDataLeaves(ctx, importNS, leaves); err != nil {
		return fmt.Errorf("upload volume bytes: %w", err)
	}
	content, err := waitSnapshotReady(ctx, importNS, bkImportRootName, suiteCfg.snapshotReadyTO)
	if err != nil {
		return fmt.Errorf("import-root Ready: %w", err)
	}
	if err := waitSnapshotContentReady(ctx, content, suiteCfg.snapshotReadyTO); err != nil {
		return fmt.Errorf("import-root content Ready: %w", err)
	}
	nodes, err := walkSnapshotTree(ctx, importNS, bkImportRootName)
	if err != nil {
		return fmt.Errorf("walk import tree: %w", err)
	}
	if err := waitChildrenReady(ctx, importNS, nodes, suiteCfg.snapshotReadyTO); err != nil {
		return fmt.Errorf("import children Ready: %w", err)
	}
	logf("import-root Snapshot + bound content %s Ready; all %d tree node(s) Ready", content, len(nodes))
	restorePath := coreSnapshotSubPath(importNS, bkImportRootName, subManifestsRestore)
	body, err := aggGet(ctx, restorePath, map[string]string{"targetNamespace": importNS})
	if err != nil {
		return fmt.Errorf("GET %s: %w", restorePath, err)
	}
	objs, err := decodeManifestArray(body)
	if err != nil {
		return err
	}
	if len(objs) == 0 {
		return fmt.Errorf("restore returned no manifests")
	}
	logf("restore returned %d manifest(s): %s", len(objs), restoredObjSummary(objs))
	ptrs := make([]*unstructured.Unstructured, 0, len(objs))
	for i := range objs {
		ptrs = append(ptrs, &objs[i])
	}
	if err := applyObjects(ctx, ptrs, importNS); err != nil {
		return fmt.Errorf("apply restored manifests: %w", err)
	}
	if err := assertManifestsMatchLive(ctx, label, backup.srcNS, objs); err != nil {
		return fmt.Errorf("manifests match live: %w", err)
	}
	for _, pvc := range verifyPVCs {
		switch pvc {
		case bkPVCDiskA:
			if err := waitDemoDiskReady(ctx, importNS, bkDiskAName, 15*time.Minute); err != nil {
				return fmt.Errorf("restored DemoVirtualDisk %s Ready: %w", bkDiskAName, err)
			}
		case bkPVCDiskB:
			if err := waitDemoDiskReady(ctx, importNS, bkDiskBName, 15*time.Minute); err != nil {
				return fmt.Errorf("restored DemoVirtualDisk %s Ready: %w", bkDiskBName, err)
			}
		}
	}
	if err := verifyRestoredBlockData(ctx, label, importNS, verifyPVCs); err != nil {
		return err
	}
	logf("PASSED — %d manifest(s) verified against source, %d volume(s) checksum-matched", len(objs), len(verifyPVCs))
	return nil
}

func cleanupImportVariantNS(ctx context.Context, importNS string, verifyPVCs []string) {
	for _, pvc := range verifyPVCs {
		deletePod(ctx, importNS, restoreProbePodName(pvc))
	}
	cleanupImportRootTree(ctx, importNS, bkImportRootName, 5*time.Minute)
	deleteNamespace(ctx, importNS)
}

func collectBoundSnapshotContentNames(ctx context.Context) (map[string]struct{}, error) {
	bound := make(map[string]struct{})
	for _, gvr := range []schema.GroupVersionResource{demoDiskSnapshotGVR, demoVMSnapshotGVR, volumeSnapshotGVR} {
		list, err := suiteDyn.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", gvr.Resource, err)
		}
		for i := range list.Items {
			name, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "boundSnapshotContentName")
			if name != "" {
				bound[name] = struct{}{}
			}
		}
	}
	snapList, err := suiteDyn.Resource(snapshotGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	for i := range snapList.Items {
		name, _, _ := unstructured.NestedString(snapList.Items[i].Object, "status", "boundSnapshotContentName")
		if name != "" {
			bound[name] = struct{}{}
		}
	}
	return bound, nil
}

func contentNameMatchesLeafPrefix(contentName string, leafNames []string) bool {
	for _, leaf := range leafNames {
		prefix := leaf + "-content-"
		if strings.HasPrefix(contentName, prefix) {
			return true
		}
	}
	return false
}

func sweepOrphanedLeafSnapshotContents(ctx context.Context, leafNames []string) error {
	if len(leafNames) == 0 {
		return nil
	}
	bound, err := collectBoundSnapshotContentNames(ctx)
	if err != nil {
		return err
	}
	list, err := suiteDyn.Resource(snapshotContentGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list SnapshotContents: %w", err)
	}
	for i := range list.Items {
		contentName := list.Items[i].GetName()
		if !contentNameMatchesLeafPrefix(contentName, leafNames) {
			continue
		}
		if _, live := bound[contentName]; live {
			continue
		}
		mcp, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "manifestCheckpointName")
		GinkgoWriter.Printf("sweeping orphaned SnapshotContent %s (MCP %q)\n", contentName, mcp)
		if derr := suiteDyn.Resource(snapshotContentGVR).Delete(ctx, contentName, metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
			return fmt.Errorf("delete orphaned SnapshotContent %s: %w", contentName, derr)
		}
		if mcp != "" {
			if derr := suiteDyn.Resource(manifestCheckpointGVR).Delete(ctx, mcp, metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
				return fmt.Errorf("delete orphaned ManifestCheckpoint %s: %w", mcp, derr)
			}
		}
	}
	return nil
}

func cleanupImportRootTree(ctx context.Context, importNS, rootName string, timeout time.Duration) {
	GinkgoHelper()
	if cleanupSkipped() {
		GinkgoWriter.Printf("%s: keeping import-root tree %s/%s\n", keepReason(), importNS, rootName)
		return
	}
	snap, err := getResource(ctx, snapshotGVR, importNS, rootName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		GinkgoWriter.Printf("cleanup import-root: get Snapshot failed: %v\n", err)
		return
	}
	rootContent, _, _ := unstructured.NestedString(snap.Object, "status", "boundSnapshotContentName")
	childContents := []string(nil)
	if rootContent != "" {
		if co, gerr := getResource(ctx, snapshotContentGVR, "", rootContent); gerr == nil {
			childContents = childContentNames(co)
		}
	}
	if derr := suiteDyn.Resource(snapshotGVR).Namespace(importNS).Delete(ctx, rootName, metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
		GinkgoWriter.Printf("cleanup import-root: delete Snapshot failed: %v\n", derr)
		return
	}
	if rootContent != "" {
		assertResourceGone(ctx, snapshotContentGVR, "", rootContent, timeout)
		for _, cc := range childContents {
			assertResourceGone(ctx, snapshotContentGVR, "", cc, timeout)
		}
	}
}

type importVariantSpec struct {
	label  string
	nsRole string
	build  func(context.Context) ([]byte, []*importNode, []string, error)
}

// importVariantsSpecs registers phase-5: four parallel import variants (any tree node) in separate namespaces.
func importVariantsSpecs() {
	Context("Phase 5: import any tree node (4 parallel variants)", func() {
		BeforeAll(func() {
			if !suiteCfg.volumeData {
				Skip("E2E_VOLUME_DATA not set: skipping the phase-5 import variants flow")
			}
			if !backup.ready {
				Skip("phase-4 backup download did not complete (extended-VS surface or download skipped)")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			Expect(sweepOrphanedLeafSnapshotContents(ctx, []string{
				backup.diskASnapName, backup.diskBSnapName, backup.orphanVSName, backup.vmSnapName,
			})).To(Succeed())
		})

		It("imports VolumeSnapshot, DemoVirtualDiskSnapshot, DemoVirtualMachineSnapshot, and full ns in parallel", func() {
			// The four variants run in parallel; the longest (full namespace) streams three data leaves
			// sequentially, each bounded by two dataTransferTO waits (Ready + Completed), then does the
			// restore-path Snapshot/content/children readiness at snapshotReadyTO. Budget the parent for
			// that worst-case single-variant path plus setup/upload overhead so a wedged DataImport fails on
			// its own dataTransferTO deadline rather than dragging on a giant fixed cap.
			ctx, cancel := context.WithTimeout(context.Background(), 6*suiteCfg.dataTransferTO+3*suiteCfg.snapshotReadyTO+10*time.Minute)
			defer cancel()

			specs := []importVariantSpec{
				{
					label:  "VolumeSnapshot",
					nsRole: bkImpNSVS,
					build: func(ctx context.Context) ([]byte, []*importNode, []string, error) {
						vs, err := newVSImportNode(ctx, backup.orphanVSName)
						if err != nil {
							return nil, nil, nil, err
						}
						return emptyJSONArray(), []*importNode{vs}, []string{bkPVCName}, nil
					},
				},
				{
					label:  "DemoVirtualDiskSnapshot",
					nsRole: bkImpNSDisk,
					build: func(ctx context.Context) ([]byte, []*importNode, []string, error) {
						disk, err := newDiskImportNode(ctx, backup.diskBSnapName)
						if err != nil {
							return nil, nil, nil, err
						}
						return emptyJSONArray(), []*importNode{disk}, []string{bkPVCDiskB}, nil
					},
				},
				{
					label:  "DemoVirtualMachineSnapshot",
					nsRole: bkImpNSVM,
					build: func(ctx context.Context) ([]byte, []*importNode, []string, error) {
						diskA, err := newDiskImportNode(ctx, backup.diskASnapName)
						if err != nil {
							return nil, nil, nil, err
						}
						vm, err := newVMImportNode(ctx, backup.vmSnapName, diskA)
						if err != nil {
							return nil, nil, nil, err
						}
						return emptyJSONArray(), []*importNode{vm}, []string{bkPVCDiskA}, nil
					},
				},
				{
					label:  "full namespace",
					nsRole: bkImpNSFull,
					build: func(ctx context.Context) ([]byte, []*importNode, []string, error) {
						rootManifests, err := fetchRootSnapManifests(ctx)
						if err != nil {
							return nil, nil, nil, fmt.Errorf("GET root manifests: %w", err)
						}
						diskA, err := newDiskImportNode(ctx, backup.diskASnapName)
						if err != nil {
							return nil, nil, nil, err
						}
						vm, err := newVMImportNode(ctx, backup.vmSnapName, diskA)
						if err != nil {
							return nil, nil, nil, err
						}
						diskB, err := newDiskImportNode(ctx, backup.diskBSnapName)
						if err != nil {
							return nil, nil, nil, err
						}
						vs, err := newVSImportNode(ctx, backup.orphanVSName)
						if err != nil {
							return nil, nil, nil, err
						}
						children := []*importNode{vm, diskB, vs}
						verifyPVCs := []string{bkPVCName, bkPVCDiskA, bkPVCDiskB}
						return rootManifests, children, verifyPVCs, nil
					},
				},
			}

			type variantRun struct {
				ns         string
				verifyPVCs []string
			}
			runs := make([]variantRun, len(specs))
			for i, spec := range specs {
				runs[i].ns = uniqueNS(spec.nsRole)
			}

			defer func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Minute)
				defer ccancel()
				deletePod(cctx, backup.srcNS, bkBackupClientPod)
				for i := range runs {
					cleanupImportVariantNS(cctx, runs[i].ns, runs[i].verifyPVCs)
				}
				deleteNamespace(cctx, backup.srcNS)
				phase5ImportNS = ""
			}()

			for i := range specs {
				setupCtx, setupCancel := context.WithTimeout(ctx, 2*time.Minute)
				Expect(ensureNamespace(setupCtx, runs[i].ns)).To(Succeed())
				Expect(ensureUploadRBAC(setupCtx, runs[i].ns, backup.srcNS, bkBackupClientSA)).To(Succeed())
				setupCancel()
			}
			phase5ImportNS = runs[0].ns

			errCh := make(chan error, len(specs))
			var wg sync.WaitGroup
			for i, spec := range specs {
				wg.Add(1)
				go func() {
					defer GinkgoRecover()
					defer wg.Done()
					rootManifests, children, verifyPVCs, berr := spec.build(ctx)
					if berr != nil {
						errCh <- fmt.Errorf("%s build: %w", spec.label, berr)
						return
					}
					runs[i].verifyPVCs = verifyPVCs
					if err := runImportVariant(ctx, spec.label, runs[i].ns, rootManifests, children, verifyPVCs); err != nil {
						errCh <- fmt.Errorf("%s: %w", spec.label, err)
					}
				}()
			}
			wg.Wait()
			close(errCh)
			for err := range errCh {
				Expect(err).NotTo(HaveOccurred())
			}
		})
	})
}
