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
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	volumecapturectrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	volumecaptureuc "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/volumecapture"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// ensureVolumeCaptureLeg creates root residual data artifacts.
// For orphan/uncovered PVCs the namespace root uses standard CSI VolumeSnapshots (ADR 2026-06-09);
// domain/non-root VCR publishing helpers below remain for domain-owned capture paths.
func (r *SnapshotReconciler) ensureVolumeCaptureLeg(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
) error {
	// Root residual/orphan PVC volume capture is the final wave: it runs only after every declared domain
	// child snapshot is Ready, so a PVC that a domain child covers is never momentarily treated as orphan
	// (which previously created an nss-vs-* VolumeSnapshot + child volume node that then got pruned, leaving
	// a dangling, never-archiving node). The gate is checked before the (costly) namespace PVC list so a
	// long wait does not re-list every poll. The root manifest branch is independent and keeps running.
	if volumecaptureuc.IsResidualRootPVCCaptureScope(nsSnap, content) {
		ready, pending, err := r.allDeclaredDomainChildSnapshotsReady(ctx, nsSnap.Namespace, nsSnap.Status.ChildrenSnapshotRefs)
		if err != nil {
			return err
		}
		if !ready {
			log.FromContext(ctx).V(1).Info("deferring orphan PVC volume capture until domain children are Ready", "pending", summarizePendingChildren(pending))
			return nil
		}
		targets, err := volumecaptureuc.ListOwnedPVCTargetsForLogicalContent(ctx, r.Client, nsSnap, content)
		if err != nil {
			_, ferr := r.failCapture(ctx, nsSnap, content, "VolumeCaptureTargetsFailed", fmt.Sprintf("list owned PVC targets: %v", err))
			return ferr
		}
		return r.ensureOrphanPVCVolumeSnapshots(ctx, nsSnap, content, targets)
	}

	targets, err := volumecaptureuc.ListOwnedPVCTargetsForLogicalContent(ctx, r.Client, nsSnap, content)
	if err != nil {
		_, ferr := r.failCapture(ctx, nsSnap, content, "VolumeCaptureTargetsFailed", fmt.Sprintf("list owned PVC targets: %v", err))
		return ferr
	}
	if len(targets) == 0 {
		return nil
	}

	vcrKey := types.NamespacedName{Namespace: nsSnap.Namespace, Name: vcpkg.SnapshotContentVCRName(content.UID)}

	if done, _, err := r.reconcileVolumeCaptureSteadyState(ctx, nsSnap, content, vcrKey, targets); done {
		return err
	}

	if _, _, err := r.ensureVolumeCaptureRequest(ctx, nsSnap, content, targets); err != nil {
		return err
	}
	// The root VCR name is a core-internal execution handle (deterministic SnapshotContentVCRName +
	// ownerRef) and is not published in the public status.
	return nil
}

// reconcileVolumeCapturePublish validates VCR output, hands off VSC ownerRefs, publishes dataRefs, and deletes the VCR.
// When allowRequeue is false (early in reconcileCaptureN2a), pending/incomplete volume work does not requeue the Snapshot
// so the manifest leg can proceed; when true (end of capture), incomplete publish may requeue.
func (r *SnapshotReconciler) reconcileVolumeCapturePublish(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
	allowRequeue bool,
) (ctrl.Result, error) {
	// Residual/orphan root scope is gated on all declared domain children being Ready (final-wave
	// sequencing, see ensureVolumeCaptureLeg). While the gate is closed, skip orphan publish and requeue on
	// the child-graph poll cadence so it re-checks; the root manifest branch is unaffected. The RequeueAfter
	// propagates through every n2aReturnAfterVolumePublish exit (incl. steady state). The gate is checked
	// before the namespace PVC list so a long wait does not re-list every poll.
	if volumecaptureuc.IsResidualRootPVCCaptureScope(nsSnap, content) {
		ready, _, err := r.allDeclaredDomainChildSnapshotsReady(ctx, nsSnap.Namespace, nsSnap.Status.ChildrenSnapshotRefs)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			return ctrl.Result{RequeueAfter: snapshotChildGraphPollInterval}, nil
		}
		targets, err := volumecaptureuc.ListOwnedPVCTargetsForLogicalContent(ctx, r.Client, nsSnap, content)
		if err != nil {
			return r.failCapture(ctx, nsSnap, content, "VolumeCaptureTargetsFailed", fmt.Sprintf("list owned PVC targets: %v", err))
		}
		return r.reconcileOrphanPVCVolumeSnapshotPublish(ctx, nsSnap, content, targets, allowRequeue)
	}

	targets, err := volumecaptureuc.ListOwnedPVCTargetsForLogicalContent(ctx, r.Client, nsSnap, content)
	if err != nil {
		return r.failCapture(ctx, nsSnap, content, "VolumeCaptureTargetsFailed", fmt.Sprintf("list owned PVC targets: %v", err))
	}
	if len(targets) == 0 {
		return ctrl.Result{}, nil
	}

	vcrKey := types.NamespacedName{Namespace: nsSnap.Namespace, Name: vcpkg.SnapshotContentVCRName(content.UID)}

	if done, res, err := r.reconcileVolumeCaptureSteadyState(ctx, nsSnap, content, vcrKey, targets); done {
		return res, err
	}

	vcr := &unstructured.Unstructured{}
	vcr.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	if err := r.Client.Get(ctx, vcrKey, vcr); err != nil {
		if apierrors.IsNotFound(err) {
			return requeueVolumeCaptureIf(allowRequeue, "VolumeCaptureRequest missing")
		}
		return ctrl.Result{}, err
	}

	if failed, reason, msg := volumecapturectrl.VolumeCaptureRequestFailed(vcr); failed {
		// Retain VCR and status.volumeCaptureRequestName for operator debugging (see design docs).
		return r.failCapture(ctx, nsSnap, content, snapshotpkg.ReasonVolumeCaptureFailed, fmt.Sprintf("%s: %s", reason, msg))
	}

	if !volumecapturectrl.VolumeCaptureRequestReady(vcr) {
		return requeueVolumeCaptureIf(allowRequeue, "waiting for VolumeCaptureRequest Ready=True/Completed")
	}

	vcrRefs, err := volumecapturectrl.ParseVolumeCaptureDataRefs(vcr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := volumecapturectrl.ValidateDataRefsForPublish(targets, vcrRefs); err != nil {
		return requeueVolumeCaptureIf(allowRequeue, fmt.Sprintf("invalid VolumeCaptureRequest dataRefs: %v", err))
	}

	bindings := volumecapturectrl.SnapshotDataBindingsFromVCRStatus(vcrRefs)
	bindings, err = snapshotcontent.EnrichDataBindingsWithVolumeMetadata(ctx, r.Client, r.directReader(), bindings)
	if err != nil {
		return requeueVolumeCaptureIf(allowRequeue, fmt.Sprintf("enrich volume metadata: %v", err))
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

	safe, err := volumecapturectrl.VolumeCaptureRequestSafeToDeleteWithHandoff(ctx, r.Client, vcrKey, content.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !safe {
		return requeueVolumeCaptureIf(allowRequeue, "waiting for VolumeCaptureRequest safe delete after handoff")
	}
	if err := r.deleteSnapshotVolumeCaptureRequest(ctx, vcrKey); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func requeueVolumeCaptureIf(allow bool, _ string) (ctrl.Result, error) {
	if !allow {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
}

// n2aReturnAfterVolumePublish runs volume publish (with requeue allowed) before returning a manifest-leg result.
func (r *SnapshotReconciler) n2aReturnAfterVolumePublish(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
	res ctrl.Result,
	err error,
) (ctrl.Result, error) {
	if err != nil {
		return res, err
	}
	pubRes, pubErr := r.reconcileVolumeCapturePublish(ctx, nsSnap, content, true)
	if pubErr != nil {
		return ctrl.Result{}, pubErr
	}
	if pubRes.Requeue || pubRes.RequeueAfter > 0 {
		return pubRes, nil
	}
	return res, nil
}

func (r *SnapshotReconciler) reconcileVolumeCaptureSteadyState(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
	vcrKey types.NamespacedName,
	targets []vcpkg.Target,
) (done bool, res ctrl.Result, err error) {
	if !volumecapturectrl.ContentDataRefsCoverExpectedTargets(content.DataRefList(), targets) {
		return false, ctrl.Result{}, nil
	}

	vcr := &unstructured.Unstructured{}
	vcr.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	if err := r.Client.Get(ctx, vcrKey, vcr); err != nil {
		if !apierrors.IsNotFound(err) {
			return true, ctrl.Result{}, err
		}
		// VCR already gone: nothing to clean up (the name is a deterministic core-internal handle, not
		// tracked in status).
		return true, ctrl.Result{}, nil
	}

	safe, err := volumecapturectrl.VolumeCaptureRequestSafeToDeleteWithHandoff(ctx, r.Client, vcrKey, content.Name)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	if !safe {
		return false, ctrl.Result{}, nil
	}
	if err := r.deleteSnapshotVolumeCaptureRequest(ctx, vcrKey); err != nil {
		return true, ctrl.Result{}, err
	}
	return true, ctrl.Result{}, nil
}

func (r *SnapshotReconciler) ensureVolumeCaptureRequest(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
	targets []vcpkg.Target,
) (*unstructured.Unstructured, ctrl.Result, error) {
	name := vcpkg.SnapshotContentVCRName(content.UID)
	key := types.NamespacedName{Namespace: nsSnap.Namespace, Name: name}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	err := r.Client.Get(ctx, key, existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, ctrl.Result{}, fmt.Errorf("get VolumeCaptureRequest %s: %w", key, err)
	}

	ownerRef := volumeCaptureRequestOwnerReferenceForSnapshot(nsSnap)

	if apierrors.IsNotFound(err) {
		vcr := volumecapturectrl.NewVolumeCaptureRequestObject(nsSnap.Namespace, name, ownerRef, targets)
		if err := r.Client.Create(ctx, vcr); err != nil {
			return nil, ctrl.Result{}, fmt.Errorf("create VolumeCaptureRequest %s: %w", key, err)
		}
		return vcr, ctrl.Result{}, nil
	}

	if volumeCaptureRequestConflictingSnapshotOwner(existing.GetOwnerReferences(), nsSnap) {
		return nil, ctrl.Result{}, fmt.Errorf("VolumeCaptureRequest %s owned by another Snapshot", key)
	}
	if !volumeCaptureRequestHasOwnerRefToSnapshot(existing.GetOwnerReferences(), nsSnap) {
		base := existing.DeepCopy()
		refs := append(existing.GetOwnerReferences(), ownerRef)
		existing.SetOwnerReferences(refs)
		if err := r.Client.Patch(ctx, existing, client.MergeFrom(base)); err != nil {
			return nil, ctrl.Result{}, err
		}
	}
	existingTargets, err := volumecapturectrl.ParseVolumeCaptureTargets(existing)
	if err != nil {
		return nil, ctrl.Result{}, err
	}
	if !volumecapturectrl.VolumeCaptureTargetsEqual(existingTargets, targets) {
		return nil, ctrl.Result{}, fmt.Errorf("VolumeCaptureRequest %s spec.target differs from resolved owned PVC target", key)
	}
	return existing, ctrl.Result{}, nil
}

func volumeCaptureRequestOwnerReferenceForSnapshot(ns *storagev1alpha1.Snapshot) metav1.OwnerReference {
	b := true
	return metav1.OwnerReference{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "Snapshot",
		Name:       ns.Name,
		UID:        ns.UID,
		Controller: &b,
	}
}

func volumeCaptureRequestHasOwnerRefToSnapshot(refs []metav1.OwnerReference, ns *storagev1alpha1.Snapshot) bool {
	for i := range refs {
		ref := refs[i]
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "Snapshot" &&
			ref.Name == ns.Name && ref.UID == ns.UID {
			return true
		}
	}
	return false
}

func volumeCaptureRequestConflictingSnapshotOwner(refs []metav1.OwnerReference, ns *storagev1alpha1.Snapshot) bool {
	for i := range refs {
		ref := refs[i]
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "Snapshot" &&
			(ref.Name != ns.Name || ref.UID != ns.UID) {
			return true
		}
	}
	return false
}

func (r *SnapshotReconciler) deleteSnapshotVolumeCaptureRequest(ctx context.Context, key types.NamespacedName) error {
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

