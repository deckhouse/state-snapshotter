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

package genericbinder

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// The deletionPolicy of a capture tree is chosen once, by the creator of its root, and every content of
// the tree carries the same value: the root content takes it from the root Snapshot's spec, and each child
// content copies it from the content of its parent at creation. Copying from the parent rather than
// looking the root up keeps a child's creation to the one read it already makes (the parent content is
// what owns the child), and a content spec is immutable, so the value can never diverge afterwards.
//
// The policy is read in one place only, the root Snapshot's deletion: Delete deletes the root content
// there instead of leaving it in the recycle bin under the root ObjectKeeper's TTL. What happens below the
// root does not depend on the policy at all. The garbage collector deletes each child after its parent
// through the SnapshotContent ownerReference, and every content being torn down reclaims its own data
// artifacts (snapshotcontent.reclaimDataArtifactsFromContentObj) — the same path the TTL takes for a
// Retain tree, only immediately. A content owned by its artifact (isArtifactOwnedRoot) is never part of a
// tree and keeps Retain here; its artifact's own policy is what decides.

// rootContentDeletionPolicy returns the deletionPolicy for the content of a root snapshot of a capture
// tree. Only the core Snapshot has a spec.deletionPolicy; every other root kind keeps Retain.
func rootContentDeletionPolicy(obj *unstructured.Unstructured) (string, error) {
	if obj.GroupVersionKind() != storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot") {
		return storagev1alpha1.SnapshotContentDeletionPolicyRetain, nil
	}
	snap := &storagev1alpha1.Snapshot{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, snap); err != nil {
		return "", fmt.Errorf("read spec.deletionPolicy of Snapshot %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
	}
	return snap.ContentDeletionPolicy(), nil
}

// childContentDeletionPolicy returns the deletionPolicy for the content of a child snapshot: the one its
// parent's content carries. A parent content without the field (created before the field existed) reads
// as Retain, which is what every capture content was created with then.
func childContentDeletionPolicy(parent *storagev1alpha1.SnapshotContent) string {
	if parent != nil && parent.Spec.DeletionPolicy == storagev1alpha1.SnapshotContentDeletionPolicyDelete {
		return storagev1alpha1.SnapshotContentDeletionPolicyDelete
	}
	return storagev1alpha1.SnapshotContentDeletionPolicyRetain
}
