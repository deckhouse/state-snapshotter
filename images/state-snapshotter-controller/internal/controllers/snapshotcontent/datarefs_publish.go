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

package snapshotcontent

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// PublishSnapshotContentDataRefs copies durable data bindings onto logical SnapshotContent (N5 PR-4).
func PublishSnapshotContentDataRefs(ctx context.Context, c client.Client, contentName string, refs []storagev1alpha1.SnapshotDataBinding) error {
	if contentName == "" || len(refs) == 0 {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		content := &storagev1alpha1.SnapshotContent{}
		if err := c.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
			return err
		}
		if snapshotDataRefsEqual(content.Status.DataRefs, refs) {
			return nil
		}
		base := content.DeepCopy()
		content.Status.DataRefs = append([]storagev1alpha1.SnapshotDataBinding(nil), refs...)
		return c.Status().Patch(ctx, content, client.MergeFrom(base))
	})
}

// EnsureVolumeSnapshotContentsOwnedByContent patches VSC ownerReferences to the bound SnapshotContent.
func EnsureVolumeSnapshotContentsOwnedByContent(
	ctx context.Context,
	c client.Client,
	content *storagev1alpha1.SnapshotContent,
	refs []storagev1alpha1.SnapshotDataBinding,
) error {
	if content == nil {
		return nil
	}
	for _, binding := range refs {
		if binding.Artifact.Kind != "VolumeSnapshotContent" || binding.Artifact.Name == "" {
			continue
		}
		if err := ensureVolumeSnapshotContentOwnedByContent(ctx, c, binding.Artifact.Name, content); err != nil {
			return err
		}
	}
	return nil
}

func ensureVolumeSnapshotContentOwnedByContent(
	ctx context.Context,
	c client.Client,
	vscName string,
	content *storagev1alpha1.SnapshotContent,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
		if err := c.Get(ctx, client.ObjectKey{Name: vscName}, obj); err != nil {
			return err
		}
		ownerRef := metav1.OwnerReference{
			APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
			Kind:       "SnapshotContent",
			Name:       content.Name,
			UID:        content.UID,
			Controller: func() *bool { b := true; return &b }(),
		}
		refs, changed, err := snapshotContentControllerOwnerRefsForHandoff(obj.GetOwnerReferences(), ownerRef)
		if err != nil {
			return fmt.Errorf("VolumeSnapshotContent %s: %w", vscName, err)
		}
		if !changed {
			return nil
		}
		base := obj.DeepCopy()
		obj.SetOwnerReferences(refs)
		return c.Patch(ctx, obj, client.MergeFrom(base))
	})
}

func snapshotDataRefsEqual(a, b []storagev1alpha1.SnapshotDataBinding) bool {
	if len(a) != len(b) {
		return false
	}
	byUID := make(map[string]storagev1alpha1.SnapshotDataBinding, len(a))
	for _, x := range a {
		byUID[x.TargetUID] = x
	}
	for _, y := range b {
		x, ok := byUID[y.TargetUID]
		if !ok {
			return false
		}
		if x.Target.APIVersion != y.Target.APIVersion || x.Target.Kind != y.Target.Kind ||
			x.Target.Name != y.Target.Name || x.Target.Namespace != y.Target.Namespace ||
			string(x.Target.UID) != string(y.Target.UID) {
			return false
		}
		if x.Artifact != y.Artifact {
			return false
		}
	}
	return true
}
