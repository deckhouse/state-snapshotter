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

package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

const defaultDemoSnapshotRequeueAfter = 500 * time.Millisecond

func demoSnapshotManifestCaptureRequestName(kind, namespace, name string) string {
	sum := sha256.Sum256([]byte(kind + ":" + namespace + "/" + name))
	return "demo-mcr-" + hex.EncodeToString(sum[:10])
}

func ensureDemoSnapshotManifestCaptureRequest(
	ctx context.Context,
	c client.Client,
	namespace string,
	name string,
	kind string,
	targetAPIVersion string,
	targetKind string,
	targetName string,
	ownerRef metav1.OwnerReference,
) (*ssv1alpha1.ManifestCaptureRequest, error) {
	mcrName := demoSnapshotManifestCaptureRequestName(kind, namespace, name)
	key := types.NamespacedName{Namespace: namespace, Name: mcrName}
	existing := &ssv1alpha1.ManifestCaptureRequest{}
	desiredTargets := []ssv1alpha1.ManifestTarget{{
		APIVersion: targetAPIVersion,
		Kind:       targetKind,
		Name:       targetName,
	}}
	err := c.Get(ctx, key, existing)
	if err == nil {
		if !manifestTargetsEqual(existing.Spec.Targets, desiredTargets) {
			base := existing.DeepCopy()
			existing.Spec.Targets = desiredTargets
			if err := c.Patch(ctx, existing, client.MergeFrom(base)); err != nil {
				return nil, err
			}
		}
		return existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	mcr := &ssv1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:            mcrName,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: ssv1alpha1.ManifestCaptureRequestSpec{
			Targets: desiredTargets,
		},
	}
	if err := c.Create(ctx, mcr); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ensureDemoSnapshotManifestCaptureRequest(ctx, c, namespace, name, kind, targetAPIVersion, targetKind, targetName, ownerRef)
		}
		return nil, err
	}
	return mcr, nil
}

func cleanupDemoSnapshotManifestCaptureRequest(ctx context.Context, c client.Client, mcr *ssv1alpha1.ManifestCaptureRequest) error {
	if mcr == nil {
		return nil
	}
	err := c.Delete(ctx, mcr)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func demoSnapshotManifestCaptureRequestReadyForCleanup(ctx context.Context, c client.Reader, key types.NamespacedName, contentName string) (bool, error) {
	return manifestCaptureRequestSafeToDelete(ctx, c, key, contentName)
}

func demoManifestCheckpointReady(
	ctx context.Context,
	c client.Client,
	mcr *ssv1alpha1.ManifestCaptureRequest,
) (mcpName string, ready bool, terminalFailed bool, message string, err error) {
	if mcr.Status.CheckpointName == "" {
		cond := meta.FindStatusCondition(mcr.Status.Conditions, ssv1alpha1.ManifestCaptureRequestConditionTypeReady)
		if cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == ssv1alpha1.ManifestCaptureRequestConditionReasonFailed {
			return "", false, true, cond.Message, nil
		}
		return "", false, false, fmt.Sprintf("waiting for ManifestCaptureRequest %s/%s", mcr.Namespace, mcr.Name), nil
	}

	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := c.Get(ctx, client.ObjectKey{Name: mcr.Status.CheckpointName}, mcp); err != nil {
		if apierrors.IsNotFound(err) {
			return mcr.Status.CheckpointName, false, false, fmt.Sprintf("waiting for ManifestCheckpoint %q", mcr.Status.CheckpointName), nil
		}
		return "", false, false, "", err
	}
	cond := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
	if cond == nil {
		return mcp.Name, false, false, fmt.Sprintf("waiting for ManifestCheckpoint %q Ready condition", mcp.Name), nil
	}
	if cond.Status == metav1.ConditionTrue {
		return mcp.Name, true, false, cond.Message, nil
	}
	if cond.Reason == ssv1alpha1.ManifestCheckpointConditionReasonFailed {
		return mcp.Name, false, true, cond.Message, nil
	}
	return mcp.Name, false, false, cond.Message, nil
}

func commonSnapshotContentReadyForSnapshot(ctx context.Context, c client.Reader, contentName string) (bool, string, string, error) {
	content := &storagev1alpha1.SnapshotContent{}
	if err := c.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			return false, snapshot.ReasonContentMissing, fmt.Sprintf("SnapshotContent %q not found", contentName), nil
		}
		return false, "", "", err
	}
	ready := meta.FindStatusCondition(content.Status.Conditions, snapshot.ConditionReady)
	if ready == nil {
		return false, snapshot.ReasonManifestCapturePending, fmt.Sprintf("SnapshotContent %q is not Ready yet", contentName), nil
	}
	if ready.Status == metav1.ConditionTrue {
		return true, ready.Reason, ready.Message, nil
	}
	return false, ready.Reason, ready.Message, nil
}

func demoSnapshotOwnerReference(apiVersion, kind, name string, uid types.UID) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		UID:        uid,
		Controller: &controller,
	}
}

func demoSnapshotOwnerRefMatches(ref, desired metav1.OwnerReference) bool {
	if ref.APIVersion != desired.APIVersion || ref.Kind != desired.Kind || ref.Name != desired.Name {
		return false
	}
	return desired.UID == "" || ref.UID == "" || ref.UID == desired.UID
}

func ensureDemoSnapshotOwnerRef(obj client.Object, desired metav1.OwnerReference) error {
	refs := make([]metav1.OwnerReference, 0, len(obj.GetOwnerReferences())+1)
	desiredSet := false
	for _, ref := range obj.GetOwnerReferences() {
		if demoSnapshotOwnerRefMatches(ref, desired) {
			if !desiredSet {
				refs = append(refs, desired)
				desiredSet = true
			}
			continue
		}
		if isSnapshotParentOwnerRef(ref) {
			return fmt.Errorf("child snapshot %s/%s is already owned by %s/%s", obj.GetNamespace(), obj.GetName(), ref.Kind, ref.Name)
		}
		if ref.Controller != nil && *ref.Controller {
			return fmt.Errorf("child snapshot %s/%s already has controller ownerRef %s/%s", obj.GetNamespace(), obj.GetName(), ref.Kind, ref.Name)
		}
		refs = append(refs, ref)
	}
	if !desiredSet {
		refs = append(refs, desired)
	}
	if !ownerReferencesEqual(obj.GetOwnerReferences(), refs) {
		obj.SetOwnerReferences(refs)
	}
	return nil
}

func isSnapshotParentOwnerRef(ref metav1.OwnerReference) bool {
	if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == KindNamespaceSnapshot {
		return true
	}
	if ref.APIVersion == demov1alpha1.SchemeGroupVersion.String() && ref.Kind == KindDemoVirtualMachineSnapshot {
		return true
	}
	return false
}

func ownerReferencesEqual(left, right []metav1.OwnerReference) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].APIVersion != right[i].APIVersion ||
			left[i].Kind != right[i].Kind ||
			left[i].Name != right[i].Name ||
			left[i].UID != right[i].UID {
			return false
		}
		leftController := left[i].Controller != nil && *left[i].Controller
		rightController := right[i].Controller != nil && *right[i].Controller
		if leftController != rightController {
			return false
		}
	}
	return true
}
