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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// SnapshotBoundContentFieldIndex is the field index key for Snapshot.status.boundSnapshotContentName.
const SnapshotBoundContentFieldIndex = "status.boundSnapshotContentName"

func registerSnapshotBoundContentFieldIndex(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &storagev1alpha1.Snapshot{}, SnapshotBoundContentFieldIndex, func(rawObj client.Object) []string {
		snap, ok := rawObj.(*storagev1alpha1.Snapshot)
		if !ok || snap.Status.BoundSnapshotContentName == "" {
			return nil
		}
		return []string{snap.Status.BoundSnapshotContentName}
	}); err != nil {
		return fmt.Errorf("index Snapshot.status.boundSnapshotContentName: %w", err)
	}
	return nil
}

// MapSnapshotContentToBoundSnapshots enqueues Snapshots whose status.boundSnapshotContentName matches the content.
func MapSnapshotContentToBoundSnapshots(ctx context.Context, c client.Client, obj client.Object) []reconcile.Request {
	content, ok := obj.(*storagev1alpha1.SnapshotContent)
	if !ok || content.Name == "" {
		return nil
	}
	var snaps storagev1alpha1.SnapshotList
	if err := c.List(ctx, &snaps, client.MatchingFields{SnapshotBoundContentFieldIndex: content.Name}); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(snaps.Items))
	for i := range snaps.Items {
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: snaps.Items[i].Namespace,
				Name:      snaps.Items[i].Name,
			},
		})
	}
	return out
}

func logSnapshotContentEnqueues(ctx context.Context, content *storagev1alpha1.SnapshotContent, eventType string, reqs []reconcile.Request) {
	if len(reqs) == 0 {
		return
	}
	logger := log.FromContext(ctx).V(1)
	for _, req := range reqs {
		logger.Info("snapshotcontent update enqueues bound snapshot",
			"snapshotContent", content.Name,
			"snapshot", req.Namespace+"/"+req.Name,
			"eventType", eventType,
		)
	}
}

// enqueueContentDrivenSnapshots enqueues the Snapshots woken by a SnapshotContent event. It ALWAYS wakes
// the OWNING Snapshot(s) the content is bound to (status.boundSnapshotContentName) so they mirror the
// content's Ready/ManifestsArchived legs. When includeGatedParents is set (the content's ManifestsArchived
// just latched True), it ALSO wakes the PARENT Snapshot(s) whose root manifest-capture gate
// (usecase.requireContentManifestsArchived) reads THIS content's archive latch directly — see
// gatedParentRequestsFromContent. That is the short-circuit that lets a root MCR be planned as soon as its
// last direct-child content is archived, instead of waiting for the child Snapshot's mirror hop plus the
// child-snapshot relay. Requests are deduplicated within the event: the bound owner and a gated parent are
// distinct objects, but a resync/overlapping channel could otherwise add the same key twice.
func enqueueContentDrivenSnapshots(
	ctx context.Context,
	c client.Client,
	obj client.Object,
	eventType string,
	includeGatedParents bool,
	q workqueue.TypedRateLimitingInterface[reconcile.Request],
) {
	if obj == nil {
		return
	}
	reqs := MapSnapshotContentToBoundSnapshots(ctx, c, obj)
	content, isContent := obj.(*storagev1alpha1.SnapshotContent)
	if includeGatedParents && isContent {
		reqs = append(reqs, gatedParentRequestsFromContent(ctx, c, content)...)
	}
	if isContent {
		logSnapshotContentEnqueues(ctx, content, eventType, reqs)
	}
	seen := make(map[types.NamespacedName]struct{}, len(reqs))
	for _, req := range reqs {
		if _, dup := seen[req.NamespacedName]; dup {
			continue
		}
		seen[req.NamespacedName] = struct{}{}
		q.Add(req)
	}
}

// snapshotContentManifestsArchivedTrue reports whether the content carries ManifestsArchived=True.
func snapshotContentManifestsArchivedTrue(obj client.Object) bool {
	content, ok := obj.(*storagev1alpha1.SnapshotContent)
	if !ok || content == nil {
		return false
	}
	cond := meta.FindStatusCondition(content.Status.Conditions, snapshotpkg.ConditionManifestsArchived)
	return cond != nil && cond.Status == metav1.ConditionTrue
}

// gatedParentRequestsFromContent maps a content to the parent Snapshot(s) whose root manifest-capture gate
// reads this content's ManifestsArchived latch. Link: content.spec.snapshotRef is the immutable owning
// child Snapshot S; the parents are the Snapshots that list S in status.childrenSnapshotRefs
// (findParentsReferencingChildSnapshot, namespace-local, matched by apiVersion/kind/name). Returns nil for a
// content whose owning snapshot is a root (no parent references it) or whose snapshotRef is absent, so the
// root content's own archive transition never enqueues a spurious parent.
func gatedParentRequestsFromContent(ctx context.Context, c client.Reader, content *storagev1alpha1.SnapshotContent) []reconcile.Request {
	ref := content.Spec.SnapshotRef
	if ref == nil || ref.APIVersion == "" || ref.Kind == "" || ref.Name == "" {
		return nil
	}
	owningChild := &unstructured.Unstructured{}
	owningChild.SetGroupVersionKind(schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind))
	owningChild.SetName(ref.Name)
	owningChild.SetNamespace(ref.Namespace)
	return findParentsReferencingChildSnapshot(ctx, c, owningChild)
}

func snapshotContentToSnapshotEnqueueHandler(c client.Client) handler.EventHandler {
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			// Resync/restart: a content may already be archived; wake gated parents too so a root that
			// missed the live transition still plans its MCR without waiting for the 500ms poll backstop.
			enqueueContentDrivenSnapshots(ctx, c, e.Object, "create", snapshotContentManifestsArchivedTrue(e.Object), q)
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			// Gated-parent wake fires ONLY on the ManifestsArchived False->True transition — the single
			// edge that changes the parent's requireContentManifestsArchived gate. The latch is monotonic,
			// so this is at most one parent wake per direct child (no per-write churn on the root).
			archivedTransition := !snapshotContentManifestsArchivedTrue(e.ObjectOld) && snapshotContentManifestsArchivedTrue(e.ObjectNew)
			enqueueContentDrivenSnapshots(ctx, c, e.ObjectNew, "update", archivedTransition, q)
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueueContentDrivenSnapshots(ctx, c, e.Object, "delete", false, q)
		},
		GenericFunc: func(ctx context.Context, e event.GenericEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueueContentDrivenSnapshots(ctx, c, e.Object, "generic", false, q)
		},
	}
}
