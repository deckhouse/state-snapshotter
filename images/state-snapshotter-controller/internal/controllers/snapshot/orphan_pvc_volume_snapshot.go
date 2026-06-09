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

package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	volumecapturectrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

const (
	csiSnapshotAPIGroup = "snapshot.storage.k8s.io"
)

var (
	csiVolumeSnapshotGVK = schema.GroupVersionKind{
		Group:   csiSnapshotAPIGroup,
		Version: "v1",
		Kind:    snapshotpkg.KindVolumeSnapshot,
	}
	csiVolumeSnapshotContentGVK = schema.GroupVersionKind{
		Group:   csiSnapshotAPIGroup,
		Version: "v1",
		Kind:    "VolumeSnapshotContent",
	}
)

// ensureOrphanPVCVolumeSnapshots creates standard CSI VolumeSnapshots for root residual PVC targets.
// Domain/non-root controllers keep the VCR path; this helper is the namespace-root carve-out only.
func (r *SnapshotReconciler) ensureOrphanPVCVolumeSnapshots(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	targets []vcpkg.Target,
) error {
	if len(targets) == 0 {
		return nil
	}
	refs := make([]storagev1alpha1.SnapshotChildRef, 0, len(targets))
	for _, target := range targets {
		name := orphanPVCVolumeSnapshotName(nsSnap.UID, target)
		if err := r.ensureOrphanPVCVolumeSnapshot(ctx, nsSnap, target, name); err != nil {
			return err
		}
		refs = append(refs, storagev1alpha1.SnapshotChildRef{
			APIVersion: snapshotpkg.CSISnapshotAPIVersion,
			Kind:       snapshotpkg.KindVolumeSnapshot,
			Name:       name,
		})
	}
	return r.patchSnapshotOrphanPVCVolumeSnapshotChildRefs(ctx, nsSnap, refs)
}

func (r *SnapshotReconciler) ensureOrphanPVCVolumeSnapshot(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	target vcpkg.Target,
	name string,
) error {
	key := types.NamespacedName{Namespace: nsSnap.Namespace, Name: name}
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(csiVolumeSnapshotGVK)
	err := r.Client.Get(ctx, key, existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get VolumeSnapshot %s: %w", key, err)
	}
	if apierrors.IsNotFound(err) {
		obj := orphanPVCVolumeSnapshotObject(nsSnap, target, name)
		if err := r.Client.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create VolumeSnapshot %s: %w", key, err)
		}
		return nil
	}
	if volumeSnapshotConflictingSnapshotOwner(existing.GetOwnerReferences(), nsSnap) {
		return fmt.Errorf("VolumeSnapshot %s owned by another Snapshot", key)
	}
	if !volumeSnapshotHasOwnerRefToSnapshot(existing.GetOwnerReferences(), nsSnap) {
		base := existing.DeepCopy()
		existing.SetOwnerReferences(append(existing.GetOwnerReferences(), volumeSnapshotOwnerReferenceForSnapshot(nsSnap)))
		if err := r.Client.Patch(ctx, existing, client.MergeFrom(base)); err != nil {
			return err
		}
	}
	pvcName, _, err := unstructured.NestedString(existing.Object, "spec", "source", "persistentVolumeClaimName")
	if err != nil {
		return fmt.Errorf("read VolumeSnapshot %s source PVC: %w", key, err)
	}
	if pvcName != target.Name {
		return fmt.Errorf("VolumeSnapshot %s spec.source.persistentVolumeClaimName=%q, want %q", key, pvcName, target.Name)
	}
	return nil
}

func orphanPVCVolumeSnapshotObject(nsSnap *storagev1alpha1.Snapshot, target vcpkg.Target, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": snapshotpkg.CSISnapshotAPIVersion,
		"kind":       snapshotpkg.KindVolumeSnapshot,
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": nsSnap.Namespace,
			"labels": map[string]interface{}{
				labelSnapshotUID: string(nsSnap.UID),
			},
			"ownerReferences": []interface{}{
				ownerRefToMap(volumeSnapshotOwnerReferenceForSnapshot(nsSnap)),
			},
		},
		"spec": map[string]interface{}{
			"source": map[string]interface{}{
				"persistentVolumeClaimName": target.Name,
			},
		},
	}}
	obj.SetGroupVersionKind(csiVolumeSnapshotGVK)
	return obj
}

func orphanPVCVolumeSnapshotName(snapshotUID types.UID, target vcpkg.Target) string {
	sum := sha256.Sum256([]byte(string(snapshotUID) + "|" + target.UID + "|" + target.Namespace + "/" + target.Name))
	return "nss-vs-" + hex.EncodeToString(sum[:])[:20]
}

func ownerRefToMap(ref metav1.OwnerReference) map[string]interface{} {
	out := map[string]interface{}{
		"apiVersion": ref.APIVersion,
		"kind":       ref.Kind,
		"name":       ref.Name,
		"uid":        string(ref.UID),
	}
	if ref.Controller != nil {
		out["controller"] = *ref.Controller
	}
	return out
}

func volumeSnapshotOwnerReferenceForSnapshot(ns *storagev1alpha1.Snapshot) metav1.OwnerReference {
	b := true
	return metav1.OwnerReference{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "Snapshot",
		Name:       ns.Name,
		UID:        ns.UID,
		Controller: &b,
	}
}

func volumeSnapshotHasOwnerRefToSnapshot(refs []metav1.OwnerReference, ns *storagev1alpha1.Snapshot) bool {
	for i := range refs {
		ref := refs[i]
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "Snapshot" &&
			ref.Name == ns.Name && ref.UID == ns.UID {
			return true
		}
	}
	return false
}

func volumeSnapshotConflictingSnapshotOwner(refs []metav1.OwnerReference, ns *storagev1alpha1.Snapshot) bool {
	for i := range refs {
		ref := refs[i]
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "Snapshot" &&
			(ref.Name != ns.Name || ref.UID != ns.UID) {
			return true
		}
	}
	return false
}

func (r *SnapshotReconciler) patchSnapshotOrphanPVCVolumeSnapshotChildRefs(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	refs []storagev1alpha1.SnapshotChildRef,
) error {
	key := types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, key, cur); err != nil {
			return err
		}
		merged := mergeSnapshotChildRefs(cur.Status.ChildrenSnapshotRefs, refs)
		if snapshotChildRefsEqualIgnoreOrder(cur.Status.ChildrenSnapshotRefs, merged) {
			return nil
		}
		base := cur.DeepCopy()
		cur.Status.ChildrenSnapshotRefs = merged
		cur.Status.ObservedGeneration = cur.Generation
		return r.Client.Status().Patch(ctx, cur, client.MergeFrom(base))
	})
}

func (r *SnapshotReconciler) reconcileOrphanPVCVolumeSnapshotPublish(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
	targets []vcpkg.Target,
	allowRequeue bool,
) (ctrl.Result, error) {
	if len(targets) == 0 {
		return ctrl.Result{}, nil
	}
	if snapshotcontentDataRefsCoverTargets(content.Status.DataRefs, targets) {
		_ = r.deleteSnapshotVolumeCaptureRequest(ctx, types.NamespacedName{Namespace: nsSnap.Namespace, Name: vcpkg.SnapshotContentVCRName(content.UID)})
		if nsSnap.Status.VolumeCaptureRequestName != "" {
			if err := r.patchSnapshotVolumeCaptureRequestName(ctx, nsSnap, ""); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	bindings := make([]storagev1alpha1.SnapshotDataBinding, 0, len(targets))
	for _, target := range targets {
		binding, ready, err := r.orphanPVCVolumeSnapshotBinding(ctx, nsSnap, target)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			return requeueVolumeCaptureIf(allowRequeue, "waiting for orphan PVC VolumeSnapshot ready")
		}
		bindings = append(bindings, *binding)
	}

	if err := r.Client.Get(ctx, client.ObjectKey{Name: content.Name}, content); err != nil {
		return ctrl.Result{}, err
	}
	if err := snapshotcontent.EnsureVolumeSnapshotContentsOwnedByContent(ctx, r.Client, content, bindings); err != nil {
		return requeueVolumeCaptureIf(allowRequeue, fmt.Sprintf("VolumeSnapshotContent handoff: %v", err))
	}
	if err := snapshotcontent.PublishSnapshotContentDataRefs(ctx, r.Client, content.Name, bindings); err != nil {
		return ctrl.Result{}, err
	}
	_ = r.deleteSnapshotVolumeCaptureRequest(ctx, types.NamespacedName{Namespace: nsSnap.Namespace, Name: vcpkg.SnapshotContentVCRName(content.UID)})
	if nsSnap.Status.VolumeCaptureRequestName != "" {
		if err := r.patchSnapshotVolumeCaptureRequestName(ctx, nsSnap, ""); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *SnapshotReconciler) orphanPVCVolumeSnapshotBinding(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	target vcpkg.Target,
) (*storagev1alpha1.SnapshotDataBinding, bool, error) {
	vsName := orphanPVCVolumeSnapshotName(nsSnap.UID, target)
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: nsSnap.Namespace, Name: vsName}, vs); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	boundName, _, err := unstructured.NestedString(vs.Object, "status", "boundVolumeSnapshotContentName")
	if err != nil {
		return nil, false, fmt.Errorf("read VolumeSnapshot %s boundVolumeSnapshotContentName: %w", vsName, err)
	}
	readyToUse, _, err := unstructured.NestedBool(vs.Object, "status", "readyToUse")
	if err != nil {
		return nil, false, fmt.Errorf("read VolumeSnapshot %s readyToUse: %w", vsName, err)
	}
	if boundName == "" || !readyToUse {
		return nil, false, nil
	}

	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(csiVolumeSnapshotContentGVK)
	if err := r.Client.Get(ctx, client.ObjectKey{Name: boundName}, vsc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	policy, _, err := unstructured.NestedString(vsc.Object, "spec", "deletionPolicy")
	if err != nil {
		return nil, false, fmt.Errorf("read VolumeSnapshotContent %s deletionPolicy: %w", boundName, err)
	}
	if policy != "Retain" {
		return nil, false, fmt.Errorf("VolumeSnapshotContent %s deletionPolicy=%q, want Retain", boundName, policy)
	}
	return &storagev1alpha1.SnapshotDataBinding{
		TargetUID: target.UID,
		Target: storagev1alpha1.SnapshotSubjectRef{
			UID:        types.UID(target.UID),
			APIVersion: target.APIVersion,
			Kind:       target.Kind,
			Namespace:  target.Namespace,
			Name:       target.Name,
		},
		Artifact: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: snapshotpkg.CSISnapshotAPIVersion,
			Kind:       "VolumeSnapshotContent",
			Name:       boundName,
		},
		// TODO: later add snapshotRef to dataRefs:
		// snapshotRef:
		//   apiVersion: snapshot.storage.k8s.io/v1
		//   kind: VolumeSnapshot
		//   namespace: <ns>
		//   name: <vs-name>
	}, true, nil
}

func snapshotcontentDataRefsCoverTargets(refs []storagev1alpha1.SnapshotDataBinding, targets []vcpkg.Target) bool {
	return len(targets) > 0 && volumecapturectrl.ContentDataRefsCoverExpectedTargets(refs, targets)
}
