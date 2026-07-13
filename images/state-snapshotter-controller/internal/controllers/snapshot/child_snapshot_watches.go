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

package snapshot

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// SnapshotChildrenRefsFieldIndex is the field index key for the identities listed in
// Snapshot.status.childrenSnapshotRefs. It lets the child-snapshot relay resolve the parent
// Snapshot(s) of a changed child with a cached, indexed List instead of a full-namespace
// SnapshotList on the uncached APIReader (the #1 audited reverse-lookup LIST hotspot).
const SnapshotChildrenRefsFieldIndex = "status.childrenSnapshotRefs.identity"

// childSnapshotRefIdentity builds the canonical, GVK-based identity used to index and query a child
// reference. Using the parsed GroupVersionKind (rather than the raw apiVersion string) keeps the index
// key stable regardless of how the apiVersion was spelled. Returns "" when the ref is not resolvable to
// a concrete GVK (such refs are intentionally left out of the index; the defensive re-match still holds).
func childSnapshotRefIdentity(apiVersion, kind, name string) string {
	if apiVersion == "" || kind == "" || name == "" {
		return ""
	}
	gvk := schema.FromAPIVersionAndKind(apiVersion, kind)
	if gvk.Kind == "" || gvk.Version == "" {
		return ""
	}
	return gvk.String() + "|" + name
}

// snapshotChildrenRefsIndexValues extracts the canonical child identities of a Snapshot for the
// SnapshotChildrenRefsFieldIndex. Shared by the production index registration and tests so both index
// the object exactly the same way.
func snapshotChildrenRefsIndexValues(rawObj client.Object) []string {
	snap, ok := rawObj.(*storagev1alpha1.Snapshot)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(snap.Status.ChildrenSnapshotRefs))
	for _, ref := range snap.Status.ChildrenSnapshotRefs {
		if id := childSnapshotRefIdentity(ref.APIVersion, ref.Kind, ref.Name); id != "" {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// registerSnapshotChildrenRefsFieldIndex registers the cached field index over every identity in a
// Snapshot's status.childrenSnapshotRefs. Must be called before the manager cache starts.
func registerSnapshotChildrenRefsFieldIndex(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &storagev1alpha1.Snapshot{}, SnapshotChildrenRefsFieldIndex, snapshotChildrenRefsIndexValues); err != nil {
		return fmt.Errorf("index Snapshot.status.childrenSnapshotRefs: %w", err)
	}
	return nil
}

// childSnapshotRefMatchesUnstructuredChild reports whether a strict SnapshotChildRef
// identifies the same object as an unstructured child snapshot from the API.
func childSnapshotRefMatchesUnstructuredChild(ref storagev1alpha1.SnapshotChildRef, child *unstructured.Unstructured, childName string) bool {
	if ref.Name != childName || ref.APIVersion == "" || ref.Kind == "" {
		return false
	}
	if ref.APIVersion == child.GetAPIVersion() && ref.Kind == child.GetKind() {
		return true
	}
	refGVK := schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind)
	if refGVK.Kind == "" || refGVK.Version == "" {
		return false
	}
	return refGVK == child.GroupVersionKind()
}

// findParentsReferencingChildSnapshot returns reconcile requests for Snapshot parents whose
// status.childrenSnapshotRefs match the child's apiVersion, kind, and name.
//
// Snapshot-run tree is namespace-local: only Snapshot objects in the child's namespace are
// considered (no cluster-wide list).
//
// The parent lookup is served from the manager cache via the SnapshotChildrenRefsFieldIndex
// (child identity -> parent Snapshot) instead of a full-namespace SnapshotList on the uncached
// APIReader, which was the #1 audited reverse-lookup LIST hotspot (~440 LIST/tree). The child object
// itself is still read read-after-write on the APIReader by the caller before this is invoked, so the
// only value taken from the cache here is the parent set. A defensive re-match on the returned parents
// guards against index-key collisions and transient staleness.
func findParentsReferencingChildSnapshot(ctx context.Context, c client.Client, child *unstructured.Unstructured) []reconcile.Request {
	if child == nil {
		return nil
	}
	childName := child.GetName()
	childNS := child.GetNamespace()
	if childNS == "" {
		// Relay is for namespaced child snapshot objects; cluster-scoped snapshot kinds are out of scope here.
		return nil
	}

	list := &storagev1alpha1.SnapshotList{}
	opts := []client.ListOption{client.InNamespace(childNS)}
	if identity := childSnapshotRefIdentity(child.GetAPIVersion(), child.GetKind(), childName); identity != "" {
		opts = append(opts, client.MatchingFields{SnapshotChildrenRefsFieldIndex: identity})
	}
	if err := c.List(ctx, list, opts...); err != nil {
		log.FromContext(ctx).Error(err, "findParentsReferencingChildSnapshot: list Snapshot", "namespace", childNS)
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		p := &list.Items[i]
		for _, ref := range p.Status.ChildrenSnapshotRefs {
			if !childSnapshotRefMatchesUnstructuredChild(ref, child, childName) {
				continue
			}
			out = append(out, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name},
			})
			break
		}
	}
	return out
}
