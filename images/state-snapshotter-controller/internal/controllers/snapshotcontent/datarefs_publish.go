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

// volumeSnapshotContentRetainPolicy keeps the bound VSC durable after the per-run VolumeSnapshot /
// VolumeCaptureRequest is deleted (durable-artifact contract, ADR 2026-06-09 / spec §3.9.6, §3.9.11).
const volumeSnapshotContentRetainPolicy = "Retain"

// EnsureVolumeSnapshotContentsOwnedByContent performs the durable-artifact handoff for each bound VSC:
// it re-parents the VSC ownerReference to the bound SnapshotContent AND forces spec.deletionPolicy to
// Retain so the artifact survives deletion of the per-run VolumeSnapshot/VCR. This mirrors the orphan
// PVC path (ensureVolumeSnapshotContentRetain): without the Retain step a class-default Delete policy
// would let the underlying snapshot be garbage-collected, breaking durability (the domain VCR path
// previously left the VSC at deletionPolicy=Delete).
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
		// A VSC that is being deleted MUST NOT be patched (spec §3.9.10): touching ownerRef or
		// deletionPolicy on an object with a deletionTimestamp is pointless (it is going away) and could
		// race finalizer removal. Data readiness already treats a deleting VSC as ArtifactMissing. The
		// self-heal caller pre-checks this too; the guard here makes the publish-path handoff equally safe.
		if !obj.GetDeletionTimestamp().IsZero() {
			return nil
		}
		ownerRef := metav1.OwnerReference{
			APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
			Kind:       "SnapshotContent",
			Name:       content.Name,
			UID:        content.UID,
			Controller: func() *bool { b := true; return &b }(),
		}
		refs, ownerChanged, err := snapshotContentControllerOwnerRefsForHandoff(obj.GetOwnerReferences(), ownerRef)
		if err != nil {
			return fmt.Errorf("VolumeSnapshotContent %s: %w", vscName, err)
		}
		policy, _, perr := unstructured.NestedString(obj.Object, "spec", "deletionPolicy")
		if perr != nil {
			return fmt.Errorf("read VolumeSnapshotContent %s deletionPolicy: %w", vscName, perr)
		}
		policyChanged := policy != volumeSnapshotContentRetainPolicy
		if !ownerChanged && !policyChanged {
			return nil
		}
		base := obj.DeepCopy()
		if ownerChanged {
			obj.SetOwnerReferences(refs)
		}
		if policyChanged {
			if serr := unstructured.SetNestedField(obj.Object, volumeSnapshotContentRetainPolicy, "spec", "deletionPolicy"); serr != nil {
				return fmt.Errorf("set VolumeSnapshotContent %s deletionPolicy=Retain: %w", vscName, serr)
			}
		}
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
