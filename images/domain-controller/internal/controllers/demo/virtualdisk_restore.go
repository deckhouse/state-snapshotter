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

package demo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/domain-controller/internal/controllers/snaphelpers"
)

type demoRestoreResolution struct {
	Ready   bool
	Failed  bool
	Reason  string
	Message string

	VSCName          string
	StorageClassName string
	VolumeMode       corev1.PersistentVolumeMode
	FsType           string
	AccessModes      []corev1.PersistentVolumeAccessMode
}

func demoDiskVRRName(diskUID types.UID) string {
	sum := sha256.Sum256([]byte("demo-vrr:" + string(diskUID)))
	return "demo-vrr-" + hex.EncodeToString(sum[:8])
}

func resolveDemoDiskRestore(
	ctx context.Context,
	reader client.Reader,
	disk *demov1alpha1.DemoVirtualDisk,
) (demoRestoreResolution, error) {
	var out demoRestoreResolution
	ds := disk.Spec.DataSource
	if ds == nil {
		return out, fmt.Errorf("dataSource is nil")
	}
	if ds.Kind != controllercommon.KindDemoVirtualDiskSnapshot {
		out.Failed = true
		out.Reason = demoReasonInvalidDataSource
		out.Message = fmt.Sprintf("unsupported dataSource kind %q (only %s is allowed)", ds.Kind, controllercommon.KindDemoVirtualDiskSnapshot)
		return out, nil
	}
	if ds.APIGroup != "" && ds.APIGroup != demov1alpha1.APIGroup {
		out.Failed = true
		out.Reason = demoReasonInvalidDataSource
		out.Message = fmt.Sprintf("unsupported dataSource apiGroup %q", ds.APIGroup)
		return out, nil
	}

	snap := &demov1alpha1.DemoVirtualDiskSnapshot{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: disk.Namespace, Name: ds.Name}, snap); err != nil {
		if apierrors.IsNotFound(err) {
			out.Reason = demoReasonSnapshotNotReady
			out.Message = fmt.Sprintf("%s %q not found", controllercommon.KindDemoVirtualDiskSnapshot, ds.Name)
			return out, nil
		}
		return out, err
	}
	if snap.Status.BoundSnapshotContentName == "" {
		out.Reason = demoReasonSnapshotNotReady
		out.Message = fmt.Sprintf("waiting for %s %q status.boundSnapshotContentName", controllercommon.KindDemoVirtualDiskSnapshot, ds.Name)
		return out, nil
	}

	content := &storagev1alpha1.SnapshotContent{}
	if err := reader.Get(ctx, types.NamespacedName{Name: snap.Status.BoundSnapshotContentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			out.Reason = demoReasonContentNotReady
			out.Message = fmt.Sprintf("SnapshotContent %q not found", snap.Status.BoundSnapshotContentName)
			return out, nil
		}
		return out, err
	}

	// Anti-spoofing handshake: the bound content must point its spec.snapshotRef back at this very
	// DemoVirtualDiskSnapshot. Without this, a user with status write on the snapshot could set
	// status.boundSnapshotContentName to a foreign content and restore someone else's data. We require a
	// full identity match (apiVersion+kind+namespace+name, and UID when the content carries one) rather
	// than a namespace-only check. Any missing/mismatched field fails closed.
	if msg := demoSnapshotContentRefMismatch(content.Spec.SnapshotRef, snap); msg != "" {
		out.Failed = true
		out.Reason = demoReasonRestoreDenied
		out.Message = msg
		return out, nil
	}

	dataRef := content.Status.Data
	if dataRef == nil || dataRef.Artifact.Name == "" {
		out.Reason = demoReasonContentNotReady
		out.Message = fmt.Sprintf("waiting for SnapshotContent %q status.data", content.Name)
		return out, nil
	}
	if dataRef.Source.Namespace == "" || dataRef.Source.Namespace != disk.Namespace {
		out.Failed = true
		out.Reason = demoReasonRestoreDenied
		out.Message = fmt.Sprintf("data source namespace %q does not match disk namespace %q", dataRef.Source.Namespace, disk.Namespace)
		return out, nil
	}
	if dataRef.Artifact.Kind != vscKind {
		out.Failed = true
		out.Reason = demoReasonInvalidRestoreSpec
		out.Message = fmt.Sprintf("unsupported data artifact kind %q (only %s is supported for demo restore)", dataRef.Artifact.Kind, vscKind)
		return out, nil
	}

	sc := dataRef.StorageClassName
	if sc == "" {
		sc = disk.Spec.StorageClassName
	}
	if sc == "" {
		out.Failed = true
		out.Reason = demoReasonMissingStorageClass
		out.Message = "storageClassName is required for restore (dataRef and disk spec are empty)"
		return out, nil
	}

	if dataRef.VolumeMode == "" {
		out.Failed = true
		out.Reason = demoReasonInvalidRestoreSpec
		out.Message = "dataRef.volumeMode is required for restore"
		return out, nil
	}
	volumeMode := corev1.PersistentVolumeMode(dataRef.VolumeMode)
	if volumeMode != corev1.PersistentVolumeBlock && volumeMode != corev1.PersistentVolumeFilesystem {
		out.Failed = true
		out.Reason = demoReasonInvalidRestoreSpec
		out.Message = fmt.Sprintf("invalid dataRef.volumeMode %q", dataRef.VolumeMode)
		return out, nil
	}

	accessModes := make([]corev1.PersistentVolumeAccessMode, 0, len(dataRef.AccessModes))
	for _, mode := range dataRef.AccessModes {
		accessModes = append(accessModes, corev1.PersistentVolumeAccessMode(mode))
	}
	if len(accessModes) == 0 {
		accessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}

	out.Ready = true
	out.VSCName = dataRef.Artifact.Name
	out.StorageClassName = sc
	out.VolumeMode = volumeMode
	out.FsType = dataRef.FsType
	out.AccessModes = accessModes
	out.Reason = storagev1alpha1.ReasonCompleted
	out.Message = "restore chain resolved"
	return out, nil
}

// demoSnapshotContentRefMismatch returns a non-empty, human-readable reason when the content's
// spec.snapshotRef does not identify snap, and "" when the handshake passes. It enforces a full identity
// match (apiVersion+kind+namespace+name) so a content bound to a different subject (another snapshot, a
// core Snapshot, or a CSI VolumeSnapshot) is rejected. The UID is verified only when the ref carries one:
// domain-leaf imports always set it, while the orphan VolumeSnapshot path legitimately leaves it empty —
// but that path never backs a DemoVirtualDiskSnapshot, so an empty UID here still requires the kind to be
// DemoVirtualDiskSnapshot.
func demoSnapshotContentRefMismatch(ref *storagev1alpha1.SnapshotSubjectRef, snap *demov1alpha1.DemoVirtualDiskSnapshot) string {
	if ref == nil {
		return "SnapshotContent.spec.snapshotRef is absent (fail closed)"
	}
	if want := demov1alpha1.SchemeGroupVersion.String(); ref.APIVersion != want {
		return fmt.Sprintf("snapshotRef apiVersion %q does not match %q (fail closed)", ref.APIVersion, want)
	}
	if ref.Kind != controllercommon.KindDemoVirtualDiskSnapshot {
		return fmt.Sprintf("snapshotRef kind %q does not match %q (fail closed)", ref.Kind, controllercommon.KindDemoVirtualDiskSnapshot)
	}
	if ref.Namespace != snap.Namespace {
		return fmt.Sprintf("snapshotRef namespace %q does not match snapshot namespace %q (fail closed)", ref.Namespace, snap.Namespace)
	}
	if ref.Name != snap.Name {
		return fmt.Sprintf("snapshotRef name %q does not match snapshot name %q (fail closed)", ref.Name, snap.Name)
	}
	if ref.UID != "" && ref.UID != snap.UID {
		return fmt.Sprintf("snapshotRef uid %q does not match snapshot uid %q (fail closed)", ref.UID, snap.UID)
	}
	return ""
}

func buildDemoDiskVRR(disk *demov1alpha1.DemoVirtualDisk, resolution demoRestoreResolution) *unstructured.Unstructured {
	name := demoDiskVRRName(disk.UID)
	// pvcTemplate is the spec of the PVC the restore creates and binds; it absorbs the former root
	// storageClassName/volumeMode/accessModes. The PVC name lives in pvcTemplate.metadata.name; the
	// namespace is implicit = the VRR namespace (restore is never cross-namespace), set below via
	// metadata.namespace = disk.Namespace. Size is intentionally omitted — the foundation restore
	// executor derives it from the source snapshot's restoreSize.
	pvcSpec := map[string]interface{}{
		"storageClassName": resolution.StorageClassName,
		"volumeMode":       string(resolution.VolumeMode),
	}
	if len(resolution.AccessModes) > 0 {
		modes := make([]interface{}, 0, len(resolution.AccessModes))
		for _, mode := range resolution.AccessModes {
			modes = append(modes, string(mode))
		}
		pvcSpec["accessModes"] = modes
	}
	spec := map[string]interface{}{
		"sourceRef": map[string]interface{}{
			"kind": vscKind,
			"name": resolution.VSCName,
		},
		"pvcTemplate": map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": disk.Spec.PersistentVolumeClaimName,
			},
			"spec": pvcSpec,
		},
	}
	// fsType is a restore execution parameter read by the external-provisioner, not a PVC field, so it
	// stays at spec root (optional, ignored for Block volumes).
	if resolution.FsType != "" {
		spec["fsType"] = resolution.FsType
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": vrrAPIVersion,
		"kind":       vrrKind,
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": disk.Namespace,
			"labels": map[string]interface{}{
				demoLabelManagedBy: demoManagedByValue,
				demoLabelDiskName:  disk.Name,
			},
		},
		"spec": spec,
	}}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "storage-foundation.deckhouse.io",
		Version: "v1alpha1",
		Kind:    vrrKind,
	})
	return obj
}

func ensureDemoDiskVRR(ctx context.Context, c client.Client, disk *demov1alpha1.DemoVirtualDisk, resolution demoRestoreResolution) error {
	desired := buildDemoDiskVRR(disk, resolution)
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(desired.GroupVersionKind())
	err := c.Get(ctx, types.NamespacedName{Namespace: disk.Namespace, Name: desired.GetName()}, existing)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		if createErr := c.Create(ctx, desired); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return createErr
		}
		return nil
	}
	return nil
}

func adoptDemoDiskPVC(ctx context.Context, c client.Client, disk *demov1alpha1.DemoVirtualDisk, pvc *corev1.PersistentVolumeClaim) error {
	base := pvc.DeepCopy()
	changed := false

	if pvc.Labels == nil {
		pvc.Labels = map[string]string{}
	}
	if pvc.Labels[demoLabelManagedBy] != demoManagedByValue || pvc.Labels[demoLabelDiskName] != disk.Name {
		pvc.Labels[demoLabelManagedBy] = demoManagedByValue
		pvc.Labels[demoLabelDiskName] = disk.Name
		changed = true
	}

	ownerRefs := make([]metav1.OwnerReference, 0, len(pvc.OwnerReferences)+1)
	diskRef := *metav1.NewControllerRef(disk, demov1alpha1.SchemeGroupVersion.WithKind(controllercommon.KindDemoVirtualDisk))
	hasDiskController := false
	for i := range pvc.OwnerReferences {
		ref := pvc.OwnerReferences[i]
		if ref.Kind == objectKeeperKind && ref.APIVersion == objectKeeperAPIVersion {
			if ref.Controller != nil && *ref.Controller {
				ref.Controller = nil
				changed = true
			}
			ownerRefs = append(ownerRefs, ref)
			continue
		}
		if ref.UID == disk.UID && ref.Controller != nil && *ref.Controller {
			hasDiskController = true
		}
		ownerRefs = append(ownerRefs, ref)
	}
	if !hasDiskController {
		ownerRefs = append(ownerRefs, diskRef)
		changed = true
	}
	if changed || len(ownerRefs) != len(pvc.OwnerReferences) {
		pvc.OwnerReferences = ownerRefs
	}
	if !changed {
		return nil
	}
	return c.Patch(ctx, pvc, client.MergeFrom(base))
}

func deleteDemoDiskVRR(ctx context.Context, c client.Client, disk *demov1alpha1.DemoVirtualDisk) error {
	vrr := &unstructured.Unstructured{}
	vrr.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "storage-foundation.deckhouse.io",
		Version: "v1alpha1",
		Kind:    vrrKind,
	})
	name := demoDiskVRRName(disk.UID)
	err := c.Get(ctx, types.NamespacedName{Namespace: disk.Namespace, Name: name}, vrr)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return client.IgnoreNotFound(c.Delete(ctx, vrr))
}

func pvcCapacityCopy(pvc *corev1.PersistentVolumeClaim) map[string]resource.Quantity {
	if len(pvc.Status.Capacity) == 0 {
		return nil
	}
	out := make(map[string]resource.Quantity, len(pvc.Status.Capacity))
	for k, v := range pvc.Status.Capacity {
		q := v
		out[string(k)] = q
	}
	return out
}

func pvcIsBound(pvc *corev1.PersistentVolumeClaim) bool {
	return pvc != nil && pvc.Status.Phase == corev1.ClaimBound && len(pvc.Status.Capacity) > 0
}

func diskHasControllerOwner(pvc *corev1.PersistentVolumeClaim, disk *demov1alpha1.DemoVirtualDisk) bool {
	for _, ref := range pvc.OwnerReferences {
		if ref.UID == disk.UID && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

func keeperStillControlsPVC(pvc *corev1.PersistentVolumeClaim) bool {
	for _, ref := range pvc.OwnerReferences {
		if ref.Kind == objectKeeperKind && ref.APIVersion == objectKeeperAPIVersion && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

func normalizeDemoVolumeMode(raw string) (corev1.PersistentVolumeMode, error) {
	switch strings.TrimSpace(raw) {
	case "", string(corev1.PersistentVolumeFilesystem):
		return corev1.PersistentVolumeFilesystem, nil
	case string(corev1.PersistentVolumeBlock):
		return corev1.PersistentVolumeBlock, nil
	default:
		return "", fmt.Errorf("invalid volumeMode %q", raw)
	}
}
