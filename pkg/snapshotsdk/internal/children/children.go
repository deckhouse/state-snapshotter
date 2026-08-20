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

// Package children reconciles a snapshot's desired set of child snapshot objects: create each desired child
// (or service one this parent already controls) and derive its durable SnapshotChildRef. It never takes over
// an object it did not create — see ensureChild. SDK v1 is a publication layer, not a lifecycle
// owner: it never deletes children, and publication is additive (the caller UNIONs these refs into status,
// never removing one). A child no longer enumerated by its emitter therefore KEEPS its published
// childrenSnapshotRefs entry; only the leftover OBJECT is reclaimed — by ownerRef garbage collection (the
// parent owns each child, so it is collected when the parent is deleted) or a future cleanup component.
// This keeps the contract delete-free — no List, no orphan diff, no unstructured delete, no risk of
// removing a foreign object on the strength of a stale status (see design R23/R29).
package children

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/ownerref"
)

// Reconcile creates each desired child snapshot object (the domain-built templates) under owner — or
// services one owner already controls — and returns the resulting, sorted SnapshotChildRefs for the caller
// to UNION into status. It performs that plus ref derivation only, and never deletes children (SDK v1 is
// delete-free; see package doc). A nil/empty desired set therefore yields empty refs without touching any
// previously created object.
//
// It fails closed, leaving the object untouched, on anything at a child's name that this same parent does
// not already control: another parent's child, a foreign-controlled object, or an unowned one. A child
// enters this tree by being created here and in no other way, so an existing object is never taken over on
// the strength of its name, its spec or a label.
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

// ensureChild creates the child described by desired, or services one this same parent already controls,
// without ever mutating desired itself: the desired object is a caller-owned template (ChildSpec.Object), so
// owner-reference stamping and the create happen on a deep copy.
//
// It never takes over an object that exists but is not already controlled by this parent. Writing the owner
// reference is what pulls an object into this tree's garbage-collection graph, so it may only be written to
// an object this write-path is creating right now. An object that merely occupies the name — however well it
// matches the template — belongs to whoever made it, and there is nothing here that could tell the two
// apart: a name and a label are both writable by anyone who can create the object.
func ensureChild(ctx context.Context, c client.Client, desired client.Object, owner metav1.OwnerReference) error {
	key := client.ObjectKeyFromObject(desired)
	existing := desired.DeepCopyObject().(client.Object)
	err := c.Get(ctx, key, existing)
	if apierrors.IsNotFound(err) {
		child := desired.DeepCopyObject().(client.Object)
		if oErr := ownerref.Ensure(child, owner); oErr != nil {
			return oErr
		}
		// Stamp origin and the authoritative delete-protection state into the CREATE payload, alongside the
		// owner reference: a child node is introduced into the tree here, and it must never exist without
		// being recognisable as ours or while still deletable. This is also the only moment at which origin
		// can be stated truthfully — an observer of an existing object cannot know who created it — and a
		// post-create patch would leave a race.
		storagev1alpha1.StampProvenance(child)
		storagev1alpha1.StampDeleteProtected(child)
		return c.Create(ctx, child)
	}
	if err != nil {
		return err
	}
	// Eligibility is decided BEFORE any mutation, and the only thing that establishes it is a controller
	// owner reference to exactly this parent — the shape an earlier pass of this write-path leaves behind, and
	// the only membership signal available here. Refusing leaves the object exactly as found — no owner
	// reference, no marker, no status — which is the whole point: the alternative is to own an object we then
	// refuse to manage, GC-linked to a tree it was never part of.
	if !ownerref.ControlledBy(existing, owner) {
		return fmt.Errorf("child %s/%s exists but is not controlled by %s %s (uid %s); this write-path never took it in",
			existing.GetNamespace(), existing.GetName(), owner.Kind, owner.Name, owner.UID)
	}
	if storagev1alpha1.HasProvenance(existing) && storagev1alpha1.IsDeleteProtected(existing) {
		return nil
	}
	// Repair, not a second way in: the owner reference above establishes that this object was already taken
	// into this parent's ownership shape on an earlier pass, so a missing marker is a build that predates the
	// marker or a label someone stripped — provenance, unlike the protection state, is not immutable at
	// admission. Restoring it keeps the write-path's own invariant; it opens no path for an object that was
	// not already in. Leaving it missing would strand our own child outside the contract forever, since every
	// consumer decides by origin. A caller cannot tell which branch produced the child it got back, so both
	// leave it in the same state.
	base := existing.DeepCopyObject().(client.Object)
	storagev1alpha1.StampProvenance(existing)
	storagev1alpha1.StampDeleteProtected(existing)
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
// (apiVersion+kind+name) and sorted deterministically. It is the additive publication primitive:
// a planning pass unions its freshly derived refs INTO the already-published set rather than replacing it,
// so refs contributed by a co-writer of the same field — the namespace root's orphan VolumeSnapshot wave,
// which a given planning pass does not itself enumerate — are preserved. This keeps
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
