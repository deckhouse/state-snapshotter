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
	"encoding/json"
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
	"k8s.io/apimachinery/pkg/types"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

const (
	bkImportRootName   = "import-root"
	bkRestoreProbePod  = "restore-probe"
	bkRestoreProbeCont = "probe"
	bkDataImportTTL    = "15m"
	bkDataArtifactType = "VolumeSnapshotContent"
	vsAPIVersion       = "snapshot.storage.k8s.io/v1"
)

type leafImportSpec struct {
	name      string
	kind      string
	group     string
	resource  string
	pvcName   string
	manifests []byte
}

func mergeManifestBodies(parts ...[]byte) ([]byte, error) {
	var all []unstructured.Unstructured
	for _, p := range parts {
		objs, err := decodeManifestArray(p)
		if err != nil {
			return nil, err
		}
		all = append(all, objs...)
	}
	return json.Marshal(all)
}

// importRootOwnerRef returns the child->parent Snapshot ownerRef the generic import binder uses
// to resolve a leaf's parent SnapshotContent (mirrors d8-cli snapimport.parentOwnerReference).
func importRootOwnerRef(rootName string, rootUID types.UID, volumeSnapshotLeaf bool) []metav1.OwnerReference {
	ref := metav1.OwnerReference{
		APIVersion: "storage.deckhouse.io/v1alpha1",
		Kind:       "Snapshot",
		Name:       rootName,
		UID:        rootUID,
	}
	if !volumeSnapshotLeaf {
		controller := true
		ref.Controller = &controller
	}
	return []metav1.OwnerReference{ref}
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

func createImportDiskSnapshot(ctx context.Context, ns, name, dataImportName string, sourceRef map[string]interface{}, ownerRefs []metav1.OwnerReference) error {
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
			"sourceRef":  sourceRef,
			"dataSource": map[string]interface{}{"name": dataImportName},
		},
	}}
	_, err := suiteDyn.Resource(demoDiskSnapshotGVR).Namespace(ns).Create(ctx, snap, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func createImportVolumeSnapshot(ctx context.Context, ns, name, dataImportName string, ownerRefs []metav1.OwnerReference) error {
	meta := map[string]interface{}{
		"name":      name,
		"namespace": ns,
	}
	if len(ownerRefs) > 0 {
		meta["ownerReferences"] = ownerReferencesField(ownerRefs)
	}
	vs := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": vsAPIVersion,
		"kind":       "VolumeSnapshot",
		"metadata":   meta,
		"spec": map[string]interface{}{
			"source": map[string]interface{}{
				"dataImportName": dataImportName,
			},
		},
	}}
	_, err := suiteDyn.Resource(volumeSnapshotGVR).Namespace(ns).Create(ctx, vs, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func createDataImport(ctx context.Context, ns, name, group, resource, leafName string) error {
	di := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "storage.deckhouse.io/v1alpha1",
		"kind":       "DataImport",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"ttl":              bkDataImportTTL,
			"dataArtifactType": bkDataArtifactType,
			"targetRef": map[string]interface{}{
				"group":    group,
				"resource": resource,
				"name":     leafName,
			},
		},
	}}
	_, err := suiteDyn.Resource(dataImportGVR).Namespace(ns).Create(ctx, di, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func deleteDataImport(ctx context.Context, ns, name string) {
	_ = suiteDyn.Resource(dataImportGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
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
			artifact, artFound, _ := unstructured.NestedMap(obj.Object, "status", "dataArtifactRef")
			if found && st == "True" && artFound && len(artifact) > 0 {
				return nil
			}
			last = fmt.Sprintf("Completed=%v reason=%q artifact=%v", st, reason, artFound)
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

func ensureUploadRBAC(ctx context.Context, importNS, clientSANamespace, clientSAName string) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: bkBackupClientSA, Namespace: importNS},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"storage.deckhouse.io"},
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
	// The importer's CheckRequiredHeaders middleware rejects ANY PUT lacking these five headers
	// (X-Content-Length, X-Offset, X-Attribute-Permissions/Uid/Gid) with HTTP 400 before the block
	// handler runs; the block handler ignores the X-Attribute-* values, so any non-empty values work.
	putCmd := fmt.Sprintf(
		`TOKEN=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token); curl -fksS -X PUT -H "Authorization: Bearer $TOKEN" -H "X-Content-Length: %d" -H "X-Offset: 0" -H "X-Attribute-Permissions: 0644" -H "X-Attribute-Uid: 0" -H "X-Attribute-Gid: 0" --data-binary @%q %q`,
		bkBlockBytes, srcFile, blockURL,
	)
	stdout, stderr, err := storagekube.ExecInPod(ctx, suiteRestCfg, backup.srcNS, bkBackupClientPod, bkBackupClientCont, []string{"sh", "-c", putCmd})
	if err != nil {
		return fmt.Errorf("PUT block data: %w (stdout=%q stderr=%q)", err, stdout, stderr)
	}
	// POST /finished must NOT carry the X-Attribute-* headers: the middleware only short-circuits PUTs,
	// so a fully-populated non-PUT would slip past its (return-less) guard and double-invoke the handler.
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

func restoreProbePodSpec(ns string, pvcs []string, devicePaths []string) *corev1.Pod {
	devices := make([]corev1.VolumeDevice, 0, len(pvcs))
	volumes := make([]corev1.Volume, 0, len(pvcs))
	for i, pvc := range pvcs {
		devices = append(devices, corev1.VolumeDevice{Name: pvc, DevicePath: devicePaths[i]})
		volumes = append(volumes, corev1.Volume{
			Name: pvc,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc},
			},
		})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: bkRestoreProbePod, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:          bkRestoreProbeCont,
				Image:         suiteCfg.probeImage,
				Command:       []string{"sh", "-c", "sleep 3600"},
				VolumeDevices: devices,
			}},
			Volumes: volumes,
		},
	}
}

func buildLeafImports(ctx context.Context) ([]leafImportSpec, []byte, []childRef, error) {
	srcNS := backup.srcNS

	rootManifests, err := aggGet(ctx, coreSnapshotSubPath(srcNS, backup.rootSnap, subManifestsDownload), nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("GET root manifests: %w", err)
	}
	vmManifests, err := aggGet(ctx, coreGenericSubPath(srcNS, resDemoVMSnapshots, backup.vmSnapName, subManifestsDownload), nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("GET VM snapshot manifests: %w", err)
	}
	importRootManifests, err := mergeManifestBodies(rootManifests, vmManifests)
	if err != nil {
		return nil, nil, nil, err
	}

	diskAManifests, err := aggGet(ctx, coreGenericSubPath(srcNS, resDemoDiskSnapshots, backup.diskASnapName, subManifestsDownload), nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("GET disk-a manifests: %w", err)
	}
	diskBManifests, err := aggGet(ctx, coreGenericSubPath(srcNS, resDemoDiskSnapshots, backup.diskBSnapName, subManifestsDownload), nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("GET disk-b manifests: %w", err)
	}
	orphanManifests, err := aggGet(ctx, vsConnectorSubPath(srcNS, backup.orphanVSName, subManifestsDownload), nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("GET orphan VS manifests: %w", err)
	}

	// pvcName comes from the leaf->srcPVC map resolved during phase-4 capture (single source of truth);
	// it keys the /backup/<pvc>.bin upload source and the restored-PVC checksum lookup.
	leaves := []leafImportSpec{
		{
			name:      backup.diskASnapName,
			kind:      "DemoVirtualDiskSnapshot",
			group:     "demo.state-snapshotter.deckhouse.io",
			resource:  "demovirtualdisksnapshots",
			pvcName:   backup.leafToPVC[backup.diskASnapName],
			manifests: diskAManifests,
		},
		{
			name:      backup.diskBSnapName,
			kind:      "DemoVirtualDiskSnapshot",
			group:     "demo.state-snapshotter.deckhouse.io",
			resource:  "demovirtualdisksnapshots",
			pvcName:   backup.leafToPVC[backup.diskBSnapName],
			manifests: diskBManifests,
		},
		{
			name:      backup.orphanVSName,
			kind:      "VolumeSnapshot",
			group:     "snapshot.storage.k8s.io",
			resource:  "volumesnapshots",
			pvcName:   backup.leafToPVC[backup.orphanVSName],
			manifests: orphanManifests,
		},
	}
	for i := range leaves {
		if leaves[i].pvcName == "" {
			return nil, nil, nil, fmt.Errorf("no source PVC mapped for leaf %q (resolveBackupSnapRefs not run?)", leaves[i].name)
		}
	}

	childRefs := []childRef{
		{apiVersion: demoGroupVersion, kind: "DemoVirtualDiskSnapshot", name: backup.diskASnapName},
		{apiVersion: demoGroupVersion, kind: "DemoVirtualDiskSnapshot", name: backup.diskBSnapName},
		{apiVersion: vsAPIVersion, kind: "VolumeSnapshot", name: backup.orphanVSName},
	}
	return leaves, importRootManifests, childRefs, nil
}

func leafUploadPath(importNS string, leaf leafImportSpec) string {
	switch leaf.kind {
	case "DemoVirtualDiskSnapshot":
		return coreGenericSubPath(importNS, resDemoDiskSnapshots, leaf.name, subManifestsUpload)
	case "VolumeSnapshot":
		return vsConnectorSubPath(importNS, leaf.name, subManifestsUpload)
	default:
		return ""
	}
}

func diskSnapshotSourceRef(ctx context.Context, ns, snapName string) (map[string]interface{}, error) {
	obj, err := getResource(ctx, demoDiskSnapshotGVR, ns, snapName)
	if err != nil {
		return nil, err
	}
	ref, found, _ := unstructured.NestedMap(obj.Object, "spec", "sourceRef")
	if !found || ref == nil {
		return nil, fmt.Errorf("disk snapshot %s has no spec.sourceRef", snapName)
	}
	return ref, nil
}

// collectBoundSnapshotContentNames scans demo disk snapshots and VolumeSnapshots cluster-wide and
// returns the set of status.boundSnapshotContentName values currently in use (including the capture
// source tree, so we do not sweep live content).
func collectBoundSnapshotContentNames(ctx context.Context) (map[string]struct{}, error) {
	bound := make(map[string]struct{})
	for _, gvr := range []schema.GroupVersionResource{demoDiskSnapshotGVR, volumeSnapshotGVR} {
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

// sweepOrphanedLeafSnapshotContents best-effort deletes cluster-scoped SnapshotContent (and its MCP)
// left over from prior failed import runs. Leaf snapshot names are stable across runs, but each run
// materializes a new {leaf}-content-{uid[:8]}; stale contents from an old leaf UID are not bound to
// any live snapshot and only clutter diagnostics.
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

// cleanupImportRootTree deletes the import-root Snapshot and waits for its import-mode content tree
// (root + direct child contents) to be reclaimed before the import namespace is removed.
func cleanupImportRootTree(ctx context.Context, importNS, rootName string, timeout time.Duration) {
	GinkgoHelper()
	if cleanupSkippedOnFailure() {
		GinkgoWriter.Printf("E2E_KEEP_CLUSTER_ON_FAILURE: keeping import-root tree %s/%s (spec failed)\n", importNS, rootName)
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

// backupRestoreSpecs registers phase-5 backup-system restore: import the snapshot tree into a fresh
// namespace via manifests-and-children-refs-upload, upload volume bytes via SVDM DataImport, restore-apply,
// and verify manifests + block checksums against the phase-4 source.
func backupRestoreSpecs() {
	Context("Phase 5: backup-system restore into another namespace", func() {
		var importNS string

		BeforeAll(func() {
			if !suiteCfg.volumeData {
				Skip("E2E_VOLUME_DATA not set: skipping the phase-5 backup restore flow")
			}
			if !backup.ready {
				Skip("phase-4 backup download did not complete (extended-VS surface or download skipped)")
			}
			importNS = uniqueNS("bk-restore")

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			Expect(ensureNamespace(ctx, importNS)).To(Succeed())
			Expect(ensureUploadRBAC(ctx, importNS, backup.srcNS, bkBackupClientSA)).To(Succeed())
			Expect(sweepOrphanedLeafSnapshotContents(ctx, []string{
				backup.diskASnapName, backup.diskBSnapName, backup.orphanVSName,
			})).To(Succeed())

			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer ccancel()
				deletePod(cctx, backup.srcNS, bkBackupClientPod)
				deletePod(cctx, importNS, bkRestoreProbePod)
				cleanupImportRootTree(cctx, importNS, bkImportRootName, 5*time.Minute)
				deleteNamespace(cctx, importNS)
				deleteNamespace(cctx, backup.srcNS)
				phase5ImportNS = ""
			})
		})

		It("imports the snapshot tree and restores workload objects with data", func() {
			// Budget: 3 x (15m DataImport + upload) + import tree + restore materialization.
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Minute)
			defer cancel()

			leaves, rootManifests, childRefs, err := buildLeafImports(ctx)
			Expect(err).NotTo(HaveOccurred())

			By("Creating import-mode snapshot CRs and DataImport resources for each data leaf")
			Expect(createImportRootSnapshot(ctx, importNS, bkImportRootName)).To(Succeed())
			phase5ImportNS = importNS
			rootUID, err := getSnapshotUID(ctx, importNS, bkImportRootName)
			Expect(err).NotTo(HaveOccurred(), "read import-root UID")
			for _, leaf := range leaves {
				Expect(createDataImport(ctx, importNS, leaf.name, leaf.group, leaf.resource, leaf.name)).To(Succeed())
				DeferCleanup(func(name string) func() {
					return func() {
						if cleanupSkippedOnFailure() {
							GinkgoWriter.Printf("E2E_KEEP_CLUSTER_ON_FAILURE: keeping DataImport %s/%s (spec failed)\n", importNS, name)
							return
						}
						cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
						defer ccancel()
						deleteDataImport(cctx, importNS, name)
					}
				}(leaf.name))

				volumeSnapshotLeaf := leaf.kind == "VolumeSnapshot"
				ownerRefs := importRootOwnerRef(bkImportRootName, rootUID, volumeSnapshotLeaf)
				switch leaf.kind {
				case "DemoVirtualDiskSnapshot":
					sourceRef, serr := diskSnapshotSourceRef(ctx, backup.srcNS, leaf.name)
					Expect(serr).NotTo(HaveOccurred(), "read sourceRef for %s", leaf.name)
					Expect(createImportDiskSnapshot(ctx, importNS, leaf.name, leaf.name, sourceRef, ownerRefs)).To(Succeed())
				case "VolumeSnapshot":
					Expect(createImportVolumeSnapshot(ctx, importNS, leaf.name, leaf.name, ownerRefs)).To(Succeed())
				}
			}

			By("Uploading leaf manifests via manifests-and-children-refs-upload (leaves first)")
			for _, leaf := range leaves {
				body, berr := buildUploadBody(leaf.manifests, nil)
				Expect(berr).NotTo(HaveOccurred())
				path := leafUploadPath(importNS, leaf)
				Expect(path).NotTo(BeEmpty())
				Eventually(func() error {
					_, perr := aggPost(ctx, path, body)
					return perr
				}).WithContext(ctx).WithTimeout(2*time.Minute).WithPolling(pollInterval).
					Should(Succeed(), "POST %s", path)
			}

			By("Uploading the reshaped root manifests (VM folded in) with direct child refs")
			rootBody, err := buildUploadBody(rootManifests, childRefs)
			Expect(err).NotTo(HaveOccurred())
			rootPath := coreSnapshotSubPath(importNS, bkImportRootName, subManifestsUpload)
			Eventually(func() error {
				_, perr := aggPost(ctx, rootPath, rootBody)
				return perr
			}).WithContext(ctx).WithTimeout(2*time.Minute).WithPolling(pollInterval).
				Should(Succeed(), "POST %s", rootPath)

			By("Uploading volume bytes to each DataImport from the phase-4 backup cache")
			for _, leaf := range leaves {
				url, _, werr := waitDataImportReady(ctx, importNS, leaf.name, 15*time.Minute)
				Expect(werr).NotTo(HaveOccurred(), "DataImport %s Ready", leaf.name)
				srcFile := fmt.Sprintf("%s/%s.bin", backup.dataDir, leaf.pvcName)
				Expect(uploadBlockData(ctx, importNS, url, srcFile)).To(Succeed())
				Expect(waitDataImportCompleted(ctx, importNS, leaf.name, 15*time.Minute)).To(Succeed())
			}

			By("Waiting for the imported snapshot tree to reach Ready")
			content, err := waitSnapshotReady(ctx, importNS, bkImportRootName, suiteCfg.snapshotReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.snapshotReadyTO)).To(Succeed())
			nodes, err := walkSnapshotTree(ctx, importNS, bkImportRootName)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitChildrenReady(ctx, importNS, nodes, suiteCfg.snapshotReadyTO)).To(Succeed())

			By("Reading apply-ready manifests and applying them into the import namespace")
			restorePath := coreSnapshotSubPath(importNS, bkImportRootName, subManifestsRestore)
			body, err := aggGet(ctx, restorePath, map[string]string{"targetNamespace": importNS})
			Expect(err).NotTo(HaveOccurred(), "GET %s", restorePath)
			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			Expect(objs).NotTo(BeEmpty())
			ptrs := make([]*unstructured.Unstructured, 0, len(objs))
			for i := range objs {
				ptrs = append(ptrs, &objs[i])
			}
			Expect(applyObjects(ctx, ptrs, importNS)).To(Succeed())

			By("Asserting restored manifests match the live source objects")
			Expect(assertManifestsMatchLive(ctx, backup.srcNS, objs)).To(Succeed())

			By("Waiting for the restored demo disks to become Ready (fail fast on the disk-restore path)")
			// The demo disks materialize via the domain controller -> VolumeRestoreRequest, which binds the
			// backing PVC without needing a consumer (mirrors phase-3 VRR restore). The orphan PVC is restored
			// via a plain dataSourceRef on a WaitForFirstConsumer SC, so it only binds once the probe pod (its
			// first consumer) is scheduled below; we therefore do NOT gate on PVC Bound here.
			Expect(waitObjectCondition(ctx, demoDiskGVR, importNS, bkDiskAName, condReady, "True", 15*time.Minute)).
				To(Succeed(), "restored DemoVirtualDisk %s Ready", bkDiskAName)
			Expect(waitObjectCondition(ctx, demoDiskGVR, importNS, bkDiskBName, condReady, "True", 15*time.Minute)).
				To(Succeed(), "restored DemoVirtualDisk %s Ready", bkDiskBName)

			By("Verifying restored Block volume bytes via an in-cluster probe pod")
			pvcs := []string{bkPVCName, bkPVCDiskA, bkPVCDiskB}
			devicePaths := []string{"/dev/xvda", "/dev/xvdb", "/dev/xvdc"}
			_, err = suiteClientset.CoreV1().Pods(importNS).Create(ctx, restoreProbePodSpec(importNS, pvcs, devicePaths), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create restore probe pod")
			Expect(waitPodRunning(ctx, importNS, bkRestoreProbePod, 15*time.Minute)).To(Succeed())

			for i, pvc := range pvcs {
				got, rerr := readBlockChecksum(ctx, importNS, bkRestoreProbePod, bkRestoreProbeCont, devicePaths[i])
				Expect(rerr).NotTo(HaveOccurred(), "checksum restored PVC %s", pvc)
				want := backup.checksums[pvc]
				Expect(want).NotTo(BeEmpty(), "source checksum for PVC %s", pvc)
				Expect(got).To(Equal(want), "restored PVC %s bytes must match source checksum", pvc)
			}
		})
	})
}
