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

package volumecapture

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// VolumeCaptureRequestSafeToDeleteWithHandoff verifies published dataRefs and VSC ownerRefs.
func VolumeCaptureRequestSafeToDeleteWithHandoff(
	ctx context.Context,
	reader client.Reader,
	key client.ObjectKey,
	contentName string,
) (bool, error) {
	vcr := &unstructured.Unstructured{}
	vcr.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	if err := reader.Get(ctx, key, vcr); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if contentName == "" {
		return false, nil
	}
	content := &storagev1alpha1.SnapshotContent{}
	if err := reader.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	vcrRefs, err := ParseVolumeCaptureDataRefs(vcr)
	if err != nil {
		return false, err
	}
	if !isPublishedDataRefsComplete(vcrRefs, content.Status.DataRefs) {
		return false, nil
	}
	for _, b := range content.Status.DataRefs {
		if b.Artifact.Kind != "VolumeSnapshotContent" || b.Artifact.Name == "" {
			continue
		}
		ok, err := VolumeSnapshotContentOwnedByContent(ctx, reader, b.Artifact.Name, content)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

func isPublishedDataRefsComplete(vcrRefs []vcpkg.DataBinding, published []storagev1alpha1.SnapshotDataBinding) bool {
	if len(vcrRefs) == 0 || len(published) < len(vcrRefs) {
		return false
	}
	byUID := make(map[string]storagev1alpha1.SnapshotDataBinding, len(published))
	for _, b := range published {
		byUID[b.TargetUID] = b
	}
	for _, want := range vcrRefs {
		got, ok := byUID[want.TargetUID]
		if !ok {
			return false
		}
		if got.Artifact.APIVersion != want.Artifact.APIVersion ||
			got.Artifact.Kind != want.Artifact.Kind ||
			got.Artifact.Name != want.Artifact.Name {
			return false
		}
	}
	return true
}

// VolumeSnapshotContentOwnedByContent reports whether the VSC is owned by the SnapshotContent.
func VolumeSnapshotContentOwnedByContent(
	ctx context.Context,
	reader client.Reader,
	vscName string,
	content *storagev1alpha1.SnapshotContent,
) (bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
	if err := reader.Get(ctx, client.ObjectKey{Name: vscName}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if content.UID == "" {
		return false, nil
	}
	for _, ref := range obj.GetOwnerReferences() {
		if ref.APIVersion != storagev1alpha1.SchemeGroupVersion.String() || ref.Kind != "SnapshotContent" || ref.Name != content.Name {
			continue
		}
		if ref.UID == "" {
			return false, nil
		}
		return ref.UID == content.UID, nil
	}
	return false, nil
}

// VolumeCaptureRequestReady reports Ready=True with Completed reason.
func VolumeCaptureRequestReady(vcr *unstructured.Unstructured) bool {
	status, reason, _, ok := parseReadyCondition(vcr)
	return ok && status == string(metav1.ConditionTrue) && reason == vcpkg.ConditionReasonCompleted
}

// VolumeCaptureRequestFailed reports terminal failure (Ready=False, not TargetsPending).
func VolumeCaptureRequestFailed(vcr *unstructured.Unstructured) (bool, string, string) {
	status, reason, message, ok := parseReadyCondition(vcr)
	if !ok {
		return false, "", ""
	}
	if status == string(metav1.ConditionFalse) && reason != vcpkg.ConditionReasonTargetsPending {
		return true, reason, message
	}
	return false, "", ""
}
