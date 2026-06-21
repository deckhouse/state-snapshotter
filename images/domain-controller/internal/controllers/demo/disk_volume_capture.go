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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/domain-controller/internal/controllers/common"
	vcctrl "github.com/deckhouse/state-snapshotter/images/domain-controller/internal/controllers/volumecapture"
	vcpkg "github.com/deckhouse/state-snapshotter/images/domain-controller/pkg/volumecapture"
)

// ensureDemoVirtualDiskDataLeg owns the DOMAIN side of the disk PVC data leg only. When the disk declares
// spec.persistentVolumeClaimName it ensures a VolumeCaptureRequest for that PVC (owned by the disk
// snapshot, named by the disk snapshot UID per D3) and returns the VCR name to publish into
// demo.status.volumeCaptureRequestName. It does NOT read/enrich/publish SnapshotContent or delete the VCR:
// the common controller (GenericSnapshotBinderController) reads the VCR result, performs the
// VolumeSnapshotContent ownership handoff, publishes dataRefs, then deletes the VCR and sets
// status.dataCaptured. A disk without spec.persistentVolumeClaimName is manifest-only (empty vcrName).
//
// A non-empty terminalReason signals an actionable, surfaced failure (PVC missing) instead of an endless
// raw requeue; volume-capture failures are surfaced by the common controller from the VCR result.
func (r *DemoVirtualDiskSnapshotReconciler) ensureDemoVirtualDiskDataLeg(
	ctx context.Context,
	s *demov1alpha1.DemoVirtualDiskSnapshot,
	source *demov1alpha1.DemoVirtualDisk,
) (vcrName string, terminalReason string, terminalMessage string, err error) {
	pvcName := source.Spec.PersistentVolumeClaimName
	if pvcName == "" {
		return "", "", "", nil
	}

	reader := demoReconcilerReader(r.APIReader, r.Client)
	pvc := &corev1.PersistentVolumeClaim{}
	if getErr := reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: pvcName}, pvc); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return "", storagev1alpha1.ReasonArtifactMissing, fmt.Sprintf("PersistentVolumeClaim %q not found for disk data leg", pvcName), nil
		}
		return "", "", "", getErr
	}

	targets := []vcpkg.Target{{
		UID:        string(pvc.UID),
		APIVersion: corev1.SchemeGroupVersion.String(),
		Kind:       "PersistentVolumeClaim",
		Name:       pvc.Name,
		Namespace:  pvc.Namespace,
	}}

	vcrName = vcpkg.SnapshotOwnedVCRName(s.UID)
	vcrKey := types.NamespacedName{Namespace: s.Namespace, Name: vcrName}
	ownerRef := demoSnapshotOwnerReference(demov1alpha1.SchemeGroupVersion.String(), controllercommon.KindDemoVirtualDiskSnapshot, s.Name, s.UID)
	if _, ensureErr := r.ensureDemoDiskVolumeCaptureRequest(ctx, vcrKey, ownerRef, targets); ensureErr != nil {
		return "", "", "", ensureErr
	}
	return vcrName, "", "", nil
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
