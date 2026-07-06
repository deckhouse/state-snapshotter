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

// Package children reconciles a snapshot's desired set of child snapshot objects: create-or-adopt each
// desired child and derive its durable SnapshotChildRef. SDK v1 is a publication layer, not a lifecycle
// owner: it never deletes children. A child no longer desired simply drops out of the published
// childrenSnapshotRefs and becomes detached from the snapshot graph; reclaiming the leftover object is left
// to ownerRef garbage collection (the parent owns each child) or a future cleanup component. This keeps the
// contract delete-free — no List, no orphan diff, no unstructured delete, no risk of removing a foreign
// object on the strength of a stale status (see design Р23/Р29).
package children

import (
	"context"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/ownerref"
)

// Reconcile makes the cluster match desired (the domain-built child snapshot objects), adopting each child
// under owner, and returns the resulting, sorted SnapshotChildRefs to publish into status. It performs
// create/adopt + ref derivation only and never deletes children (SDK v1 is delete-free; see package doc).
// A nil/empty desired set therefore yields empty refs without touching any previously created object.
//
// It fails closed on a child already owned by a different parent (no theft), leaving that child untouched.
func Reconcile(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	owner metav1.OwnerReference,
	desired []client.Object,
) ([]storagev1alpha1.SnapshotChildRef, error) {
	newRefs := make([]storagev1alpha1.SnapshotChildRef, 0, len(desired))
	for _, d := range desired {
		gvk, err := apiutil.GVKForObject(d, scheme)
		if err != nil {
			return nil, err
		}
		if err := ensureChild(ctx, c, d, owner); err != nil {
			return nil, err
		}
		newRefs = append(newRefs, storagev1alpha1.SnapshotChildRef{
			APIVersion: gvk.GroupVersion().String(),
			Kind:       gvk.Kind,
			Name:       d.GetName(),
		})
	}

	SortRefs(newRefs)
	return newRefs, nil
}

// ensureChild creates-or-adopts the child described by desired without ever mutating desired itself: the
// desired object is a caller-owned template (ChildSpec.Object), so owner-reference stamping and the create
// happen on a deep copy.
func ensureChild(ctx context.Context, c client.Client, desired client.Object, owner metav1.OwnerReference) error {
	key := client.ObjectKeyFromObject(desired)
	existing := desired.DeepCopyObject().(client.Object)
	err := c.Get(ctx, key, existing)
	if apierrors.IsNotFound(err) {
		child := desired.DeepCopyObject().(client.Object)
		if oErr := ownerref.Ensure(child, owner); oErr != nil {
			return oErr
		}
		return c.Create(ctx, child)
	}
	if err != nil {
		return err
	}
	base := existing.DeepCopyObject().(client.Object)
	if oErr := ownerref.Ensure(existing, owner); oErr != nil {
		return oErr
	}
	if ownerRefsEqual(base.GetOwnerReferences(), existing.GetOwnerReferences()) {
		return nil
	}
	return c.Patch(ctx, existing, client.MergeFrom(base))
}

// SortRefs orders child refs deterministically by (apiVersion, kind, name).
func SortRefs(refs []storagev1alpha1.SnapshotChildRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].APIVersion != refs[j].APIVersion {
			return refs[i].APIVersion < refs[j].APIVersion
		}
		if refs[i].Kind != refs[j].Kind {
			return refs[i].Kind < refs[j].Kind
		}
		return refs[i].Name < refs[j].Name
	})
}

// UnionRefs returns the set union of two child-ref slices, de-duplicated by full ref identity
// (apiVersion+kind+name) and sorted deterministically. It is the additive publication primitive (wave5):
// a planning pass unions its freshly derived refs INTO the already-published set rather than replacing it,
// so refs contributed by a co-writer of the same field — the namespace root's orphan VolumeSnapshot wave,
// which a given planning pass does not itself enumerate (see wave5 design §6.2) — are preserved. This keeps
// SDK v1 delete-free: refs only accumulate; a child no longer desired is simply not re-added by its
// emitter, nothing is removed here.
func UnionRefs(existing, added []storagev1alpha1.SnapshotChildRef) []storagev1alpha1.SnapshotChildRef {
	out := make([]storagev1alpha1.SnapshotChildRef, 0, len(existing)+len(added))
	seen := make(map[storagev1alpha1.SnapshotChildRef]struct{}, len(existing)+len(added))
	for _, r := range existing {
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	for _, r := range added {
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	SortRefs(out)
	return out
}

// RefsEqualIgnoreOrder reports set equality of two child-ref slices.
func RefsEqualIgnoreOrder(a, b []storagev1alpha1.SnapshotChildRef) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]storagev1alpha1.SnapshotChildRef(nil), a...)
	bb := append([]storagev1alpha1.SnapshotChildRef(nil), b...)
	SortRefs(aa)
	SortRefs(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

func ownerRefsEqual(left, right []metav1.OwnerReference) bool {
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
		lc := left[i].Controller != nil && *left[i].Controller
		rc := right[i].Controller != nil && *right[i].Controller
		if lc != rc {
			return false
		}
	}
	return true
}
