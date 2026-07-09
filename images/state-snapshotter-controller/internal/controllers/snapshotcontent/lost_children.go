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

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// detectLostDeclaredChildren inspects the owner's declared children for a child snapshot that has
// vanished and returns a Ready fold for the owner mirror: a terminal ChildSnapshotLost, a non-terminal
// ChildSnapshotDeleted, or ("", "", nil) when the declared set is intact. Detection lives in MAIN (the
// SnapshotContentController holds both the content tree and the owner in one pass) and is applied as an
// override on the owner Ready mirror — the CONTENT Ready stays intact, because the durable
// SnapshotContent tree is not what degrades; only the namespaced user surface does (d8 download reads
// namespaced CRs only). All reads are UNCACHED (r.APIReader).
//
// It runs only past capture barrier 1 (phase Planned/Finished — ownerDomainCaptureAtLeastPlanned), where
// every declared child CR was already created, so a child now absent (uncached) was DELETED, not merely
// not-created-yet: no false positive from an in-flight plan. A Failed owner is excluded by that gate (its
// own phase=Failed reason bubbles instead). Whole-tree deletion is excluded by the owner-alive gate: the
// caller only folds onto a live, bound owner, and deleting the owner removes the mirror target.
//
// Two mutually-exclusive modes keyed on whether the child CONTENT edges are frozen:
//
//   - frozen (contentObj.status.childrenSnapshotContentRefs non-empty): every declared child linked its
//     content, so inspect the child SnapshotContents directly. A missing child content is Lost (terminal:
//     content names are UID-derived, a recreated child cannot relink into the immutable frozen edge set).
//     A surviving child content whose owning child CR is currently ABSENT (checked live via the content's
//     own spec.snapshotRef, NOT the monotonic status.parentDeleted latch, so a manual restore of the child
//     CR heals it) is ChildSnapshotDeleted when that content is Ready (capture complete, its data survives
//     in the recycle bin) and ChildSnapshotLost when it is not (an incomplete capture cannot be resumed).
//
//   - not frozen yet (edges empty, still linking post-Planned): inspect the declared child CRs on the
//     owner's status.childrenSnapshotRefs. A declared child CR absent (uncached) is Lost — it was created
//     at barrier 1 and has since been deleted; a recreated CR cannot relink into the frozen edge set the
//     tree will build (UID-derived content names), and the namespace domain no longer re-plans membership
//     after Planned (ns-replan-skip), so nothing self-heals it.
//
// Terminal (Lost) always wins over non-terminal (Deleted): the scan returns Lost immediately and only
// reports Deleted if no Lost child was found.
func (r *SnapshotContentController) detectLostDeclaredChildren(
	ctx context.Context,
	owner *unstructured.Unstructured,
	contentObj *unstructured.Unstructured,
) (reason, message string, err error) {
	if owner.GetDeletionTimestamp() != nil {
		// Whole-tree teardown: the owner itself is going away, there is no meaningful mirror target.
		return "", "", nil
	}
	if !ownerDomainCaptureAtLeastPlanned(owner) {
		// Before barrier 1 the child CRs may legitimately not exist yet (a missing child is not "lost").
		return "", "", nil
	}

	contentEdges, _, edgesErr := unstructured.NestedSlice(contentObj.Object, "status", "childrenSnapshotContentRefs")
	if edgesErr != nil {
		return "", "", edgesErr
	}
	if len(contentEdges) > 0 {
		return r.detectLostFromFrozenEdges(ctx, owner, contentEdges)
	}
	return r.detectLostFromDeclaredRefs(ctx, owner)
}

// detectLostFromFrozenEdges implements the frozen-edge mode of detectLostDeclaredChildren: the child
// SnapshotContent edge set is immutable, so each edge is inspected directly by its cluster-scoped name.
func (r *SnapshotContentController) detectLostFromFrozenEdges(
	ctx context.Context,
	owner *unstructured.Unstructured,
	edges []interface{},
) (string, string, error) {
	var deletedChild string
	for _, raw := range edges {
		m, ok := raw.(map[string]interface{})
		if !ok {
			return "", "", fmt.Errorf("owner %s/%s content has a malformed childrenSnapshotContentRefs entry %T", owner.GetNamespace(), owner.GetName(), raw)
		}
		name, _ := m["name"].(string)
		if name == "" {
			return "", "", fmt.Errorf("owner %s/%s content has an incomplete childrenSnapshotContentRefs entry %v", owner.GetNamespace(), owner.GetName(), m)
		}
		child := &storagev1alpha1.SnapshotContent{}
		if getErr := r.APIReader.Get(ctx, client.ObjectKey{Name: name}, child); getErr != nil {
			if errors.IsNotFound(getErr) {
				// Frozen child content is gone: unrecoverable (UID-derived names, immutable edge set).
				return snapshot.ReasonChildSnapshotLost,
					fmt.Sprintf("child SnapshotContent %q is gone; the captured child snapshot is unrecoverably lost (content names are UID-derived and the frozen child edge set is immutable) — a new snapshot is required", name), nil
			}
			return "", "", fmt.Errorf("get child SnapshotContent %q: %w", name, getErr)
		}
		crPresent, existsErr := r.childOwningSnapshotExists(ctx, child)
		if existsErr != nil {
			return "", "", existsErr
		}
		if crPresent {
			// The child CR is alive (or was manually restored): this edge is healthy. Checking the
			// CR live — instead of the monotonic status.parentDeleted latch — is what lets a restore
			// self-heal the owner mirror back to Ready.
			continue
		}
		// The child CR was deleted while its content survives in the recycle bin.
		if meta.IsStatusConditionTrue(child.Status.Conditions, snapshot.ConditionReady) {
			// Capture complete: the data survives in the recycle bin (non-terminal). Remember and keep
			// scanning — a terminal Lost elsewhere in the set wins.
			if deletedChild == "" {
				deletedChild = name
			}
			continue
		}
		// Surviving but not-Ready content: an incomplete capture cannot resume — terminal.
		return snapshot.ReasonChildSnapshotLost,
			fmt.Sprintf("child snapshot for SnapshotContent %q was deleted mid-capture (its content is not Ready and cannot resume); the child is unrecoverably lost — a new snapshot is required", name), nil
	}
	if deletedChild != "" {
		return snapshot.ReasonChildSnapshotDeleted,
			fmt.Sprintf("declared child snapshot for SnapshotContent %q was deleted; its captured content still survives in the recycle bin (the data is intact), but the namespaced child snapshot is gone", deletedChild), nil
	}
	return "", "", nil
}

// detectLostFromDeclaredRefs implements the not-yet-frozen mode of detectLostDeclaredChildren: the child
// content edges are still empty (post-Planned, pre-link), so the declared child CRs on the owner's
// status.childrenSnapshotRefs are inspected. Past barrier 1 every declared child CR was created, so an
// absent one was deleted and cannot relink into the frozen edge set the tree is building — terminal.
func (r *SnapshotContentController) detectLostFromDeclaredRefs(
	ctx context.Context,
	owner *unstructured.Unstructured,
) (string, string, error) {
	refs, found, err := unstructured.NestedSlice(owner.Object, "status", "childrenSnapshotRefs")
	if err != nil {
		return "", "", err
	}
	if !found || len(refs) == 0 {
		return "", "", nil
	}
	namespace := owner.GetNamespace()
	for _, raw := range refs {
		m, ok := raw.(map[string]interface{})
		if !ok {
			return "", "", fmt.Errorf("owner %s/%s has a malformed childrenSnapshotRefs entry %T", namespace, owner.GetName(), raw)
		}
		apiVersion, _ := m["apiVersion"].(string)
		kind, _ := m["kind"].(string)
		name, _ := m["name"].(string)
		if apiVersion == "" || kind == "" || name == "" {
			return "", "", fmt.Errorf("owner %s/%s has an incomplete childrenSnapshotRefs entry %v", namespace, owner.GetName(), m)
		}
		gv, gvErr := schema.ParseGroupVersion(apiVersion)
		if gvErr != nil {
			return "", "", gvErr
		}
		child := &unstructured.Unstructured{}
		child.SetGroupVersionKind(gv.WithKind(kind))
		if gErr := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, child); gErr != nil {
			if errors.IsNotFound(gErr) {
				return snapshot.ReasonChildSnapshotLost,
					fmt.Sprintf("declared child snapshot %s %q is gone after the plan was frozen (phase>=Planned); it cannot relink into the frozen child edge set — the child is unrecoverably lost, a new snapshot is required", kind, name), nil
			}
			return "", "", gErr
		}
	}
	return "", "", nil
}

// childOwningSnapshotExists reports whether the child SnapshotContent's owning child snapshot CR (resolved
// live via the content's own spec.snapshotRef) currently exists. It is the authoritative "is the child CR
// present?" signal for the frozen-edge fold: unlike the monotonic status.parentDeleted latch it flips back
// to true after a manual restore recreates the (deterministically-named) child CR, so the owner mirror
// self-heals. A content with no resolvable snapshotRef is treated as present (fail-open — never a false
// terminal). Namespace comes from the ref (child snapshots are namespaced); the CR is read UNCACHED.
func (r *SnapshotContentController) childOwningSnapshotExists(ctx context.Context, child *storagev1alpha1.SnapshotContent) (bool, error) {
	ref := child.Spec.SnapshotRef
	if ref == nil || ref.Name == "" || ref.APIVersion == "" || ref.Kind == "" {
		return true, nil
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return false, err
	}
	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(gv.WithKind(ref.Kind))
	if getErr := r.APIReader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, owner); getErr != nil {
		if errors.IsNotFound(getErr) {
			return false, nil
		}
		return false, getErr
	}
	return true, nil
}
