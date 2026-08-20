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
	"context"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// isArtifactOwnedRoot reports whether this snapshot's content must be owned by the snapshot's own data
// artifact rather than by a root ObjectKeeper — the second, and only other, shape a root content can take.
//
// Three conditions, each carrying its own half of the meaning:
//
//   - a CSI VolumeSnapshot, because it is the one registered kind whose data artifact exists on its own,
//     created by the external-snapshotter rather than by any capture request of this module;
//   - a root, because a node inside a tree is owned by its parent content and nothing about that changes;
//   - without provenance, because that is what says the artifact belongs to somebody else. A snapshot this
//     module created has an artifact it made for itself and keeps the ordinary tree shape.
//
// Provenance is a label and therefore forgeable, which is the right trade here: forging it onto a snapshot
// makes this module treat that snapshot's data as its own, and the object doing the forging would have to be
// one the forger can already create and delete. What matters is that the honest default is the safe one —
// nothing this module did not create is claimed by accident.
func isArtifactOwnedRoot(obj *unstructured.Unstructured) bool {
	return obj.GetObjectKind().GroupVersionKind().Kind == snapshot.KindVolumeSnapshot &&
		snapshot.IsRootSnapshot(obj) &&
		!storagev1alpha1.HasProvenance(obj)
}

// resolveBoundArtifactOwnerRef builds the controller ownerReference to the CSI VolumeSnapshotContent this
// snapshot is bound to. It reports pending while the binding does not exist yet: CSI publishes
// status.boundVolumeSnapshotContentName once the snapshot is provisioned, and until then the owner of the
// wrapper content is simply not known.
//
// Both objects are cluster-scoped, so the reference is legal in the direction it is built here. It must
// never be built in the other direction as well: mutual owner references form a cycle that garbage
// collection does not resolve, which is why the durable-artifact handoff leaves this artifact alone
// (snapshotcontent.EnsureVolumeSnapshotContentsOwnedByContent).
func (r *GenericSnapshotBinderController) resolveBoundArtifactOwnerRef(
	ctx context.Context,
	obj *unstructured.Unstructured,
) (*metav1.OwnerReference, bool, error) {
	vscName, _, err := unstructured.NestedString(obj.Object, "status", "boundVolumeSnapshotContentName")
	if err != nil {
		return nil, false, err
	}
	if vscName == "" {
		return nil, true, nil
	}

	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   snapshot.CSISnapshotGroup,
		Version: snapshot.CSISnapshotVersion,
		Kind:    snapshot.KindVolumeSnapshotContent,
	})
	if err := r.Get(ctx, client.ObjectKey{Name: vscName}, vsc); err != nil {
		if errors.IsNotFound(err) {
			// The name is published but the object is not observable yet (or was removed under us). Either
			// way there is nothing to own the content, so keep waiting rather than inventing an owner.
			return nil, true, nil
		}
		return nil, false, err
	}

	controller := true
	return &metav1.OwnerReference{
		APIVersion: snapshot.CSISnapshotAPIVersion,
		Kind:       snapshot.KindVolumeSnapshotContent,
		Name:       vsc.GetName(),
		UID:        vsc.GetUID(),
		Controller: &controller,
	}, false, nil
}
