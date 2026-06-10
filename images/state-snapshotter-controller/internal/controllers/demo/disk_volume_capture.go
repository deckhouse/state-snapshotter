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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	vcctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// reconcileDemoVirtualDiskDataLeg owns the disk PVC data leg. When the disk declares
// spec.persistentVolumeClaimName it creates a VolumeCaptureRequest for that PVC (owned by the disk
// snapshot, mirroring the domain VCR path), hands off the bound VolumeSnapshotContent into the disk
// SnapshotContent.status.dataRefs[], and deletes the VCR after a durable handoff. The PVC then becomes
// a subtree-covered volume (via dataRefs / pending VCR) so the namespace root MUST NOT treat it as an
// orphan PVC (no root VolumeSnapshot). A disk without spec.persistentVolumeClaimName is manifest-only.
//
// Returns dataComplete=true when there is no data leg or the bound VSC has been published into the
// disk content dataRefs[]. A non-empty terminalReason signals an actionable, surfaced failure
// (Ready=False) instead of an endless raw requeue.
func (r *DemoVirtualDiskSnapshotReconciler) reconcileDemoVirtualDiskDataLeg(
	ctx context.Context,
	s *demov1alpha1.DemoVirtualDiskSnapshot,
	source *demov1alpha1.DemoVirtualDisk,
	contentName string,
) (dataComplete bool, terminalReason string, terminalMessage string, err error) {
	pvcName := source.Spec.PersistentVolumeClaimName
	if pvcName == "" {
		return true, "", "", nil
	}

	reader := demoReconcilerReader(r.APIReader, r.Client)
	pvc := &corev1.PersistentVolumeClaim{}
	if getErr := reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: pvcName}, pvc); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return false, snapshot.ReasonArtifactMissing, fmt.Sprintf("PersistentVolumeClaim %q not found for disk data leg", pvcName), nil
		}
		return false, "", "", getErr
	}

	targets := []vcpkg.Target{{
		UID:        string(pvc.UID),
		APIVersion: corev1.SchemeGroupVersion.String(),
		Kind:       "PersistentVolumeClaim",
		Name:       pvc.Name,
		Namespace:  pvc.Namespace,
	}}

	content := &storagev1alpha1.SnapshotContent{}
	if getErr := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, content); getErr != nil {
		return false, "", "", getErr
	}

	vcrKey := types.NamespacedName{Namespace: s.Namespace, Name: vcpkg.SnapshotContentVCRName(content.UID)}

	// Steady state: dataRefs already cover the PVC. Drop the VCR once the handoff is durable.
	if vcctrl.ContentDataRefsCoverExpectedTargets(content.Status.DataRefs, targets) {
		safe, safeErr := vcctrl.VolumeCaptureRequestSafeToDeleteWithHandoff(ctx, r.Client, vcrKey, content.Name)
		if safeErr != nil {
			return false, "", "", safeErr
		}
		if safe {
			if delErr := r.deleteDemoDiskVolumeCaptureRequest(ctx, vcrKey); delErr != nil {
				return false, "", "", delErr
			}
		}
		return true, "", "", nil
	}

	ownerRef := demoSnapshotOwnerReference(demov1alpha1.SchemeGroupVersion.String(), controllercommon.KindDemoVirtualDiskSnapshot, s.Name, s.UID)
	vcr, ensureErr := r.ensureDemoDiskVolumeCaptureRequest(ctx, vcrKey, ownerRef, targets)
	if ensureErr != nil {
		return false, "", "", ensureErr
	}

	if failed, reason, msg := vcctrl.VolumeCaptureRequestFailed(vcr); failed {
		detail := msg
		if reason != "" {
			detail = fmt.Sprintf("%s: %s", reason, msg)
		}
		return false, snapshot.ReasonVolumeCaptureFailed, fmt.Sprintf("disk PVC %q volume capture failed: %s", pvcName, detail), nil
	}
	if !vcctrl.VolumeCaptureRequestReady(vcr) {
		// Pending: rely on the caller's requeue/content watch. Coverage already holds via the pending VCR.
		return false, "", "", nil
	}

	vcrRefs, parseErr := vcctrl.ParseVolumeCaptureDataRefs(vcr)
	if parseErr != nil {
		return false, "", "", parseErr
	}
	if validateErr := vcctrl.ValidateDataRefsForPublish(targets, vcrRefs); validateErr != nil {
		// Ready VCR with not-yet-consistent dataRefs: retry without surfacing a terminal condition.
		return false, "", "", nil
	}

	bindings := vcctrl.SnapshotDataBindingsFromVCRStatus(vcrRefs)
	if getErr := r.Client.Get(ctx, client.ObjectKey{Name: content.Name}, content); getErr != nil {
		return false, "", "", getErr
	}
	if handoffErr := snapshotcontent.EnsureVolumeSnapshotContentsOwnedByContent(ctx, r.Client, content, bindings); handoffErr != nil {
		// Retryable handoff; coverage still holds via the pending VCR.
		return false, "", "", nil
	}
	if pubErr := snapshotcontent.PublishSnapshotContentDataRefs(ctx, r.Client, content.Name, bindings); pubErr != nil {
		return false, "", "", pubErr
	}
	return true, "", "", nil
}

func (r *DemoVirtualDiskSnapshotReconciler) ensureDemoDiskVolumeCaptureRequest(
	ctx context.Context,
	key types.NamespacedName,
	ownerRef metav1.OwnerReference,
	targets []vcpkg.Target,
) (*unstructured.Unstructured, error) {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	err := r.Client.Get(ctx, key, existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get VolumeCaptureRequest %s: %w", key, err)
	}
	if apierrors.IsNotFound(err) {
		vcr := vcctrl.NewVolumeCaptureRequestObject(key.Namespace, key.Name, ownerRef, targets)
		if createErr := r.Client.Create(ctx, vcr); createErr != nil {
			if apierrors.IsAlreadyExists(createErr) {
				return r.ensureDemoDiskVolumeCaptureRequest(ctx, key, ownerRef, targets)
			}
			return nil, fmt.Errorf("create VolumeCaptureRequest %s: %w", key, createErr)
		}
		return vcr, nil
	}

	if !demoDiskVCRHasOwnerRef(existing.GetOwnerReferences(), ownerRef) {
		base := existing.DeepCopy()
		existing.SetOwnerReferences(append(existing.GetOwnerReferences(), ownerRef))
		if patchErr := r.Client.Patch(ctx, existing, client.MergeFrom(base)); patchErr != nil {
			return nil, patchErr
		}
	}
	existingTargets, parseErr := vcctrl.ParseVolumeCaptureTargets(existing)
	if parseErr != nil {
		return nil, parseErr
	}
	if !vcctrl.VolumeCaptureTargetsEqual(existingTargets, targets) {
		return nil, fmt.Errorf("VolumeCaptureRequest %s spec.targets differ from disk PVC target", key)
	}
	return existing, nil
}

func demoDiskVCRHasOwnerRef(refs []metav1.OwnerReference, desired metav1.OwnerReference) bool {
	for i := range refs {
		if demoSnapshotOwnerRefMatches(refs[i], desired) {
			return true
		}
	}
	return false
}

func (r *DemoVirtualDiskSnapshotReconciler) deleteDemoDiskVolumeCaptureRequest(ctx context.Context, key types.NamespacedName) error {
	vcr := &unstructured.Unstructured{}
	vcr.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	if err := r.Client.Get(ctx, key, vcr); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get VolumeCaptureRequest %s: %w", key, err)
	}
	if err := r.Client.Delete(ctx, vcr); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete VolumeCaptureRequest %s: %w", key, err)
	}
	return nil
}
