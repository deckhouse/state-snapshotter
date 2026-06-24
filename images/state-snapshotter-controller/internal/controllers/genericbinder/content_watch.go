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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// mapBoundContentToSnapshots returns a watch map function that, for a SnapshotContent change event, enqueues
// the generic Snapshot(s) of snapshotGVK whose status.boundSnapshotContentName references that content.
//
// Why this watch exists: SnapshotContent is the single source of truth for readiness and Snapshot.Ready is a
// verbatim mirror of it. Without a reverse wake-up the binder only re-mirrors while it is still polling
// pending content; once it has mirrored Ready=True it stops requeuing, so a later content degradation
// (Ready=True -> False, e.g. a damaged VSC/MCP surfaced by the SnapshotContentController) would leave
// Snapshot.Ready stale. This watch makes the mirror converge in both directions, event-driven, without any
// polling loop or forced periodic reconcile.
//
// Why List+filter and not a field index / ownerRef hop: the content carries no reverse Snapshot reference
// (its controller ownerRef points up the content tree to the root ObjectKeeper or a parent SnapshotContent,
// never to the namespaced Snapshot), so the bound Snapshot is resolved through its own truth ref
// (status.boundSnapshotContentName). A field index cannot be added once the informer has started, but binder
// watches may be registered at runtime via AddWatchForPair (CSD-driven), so a List+filter keeps the mapping
// valid for both bootstrap and runtime registration. The handler only enqueues; it never writes conditions.
//
// TODO: replace List+filter with a field index on status.boundSnapshotContentName when dynamic watch
// registration is removed, or when indexes can be registered before cache start for all CSD-discovered GVKs.
// This List+filter is a temporary dynamic-registration tradeoff: every SnapshotContent event triggers a
// List of all Snapshots of the paired GVK (per registered generic controller), which is fine at the current
// scale but is not the final scalable scheme.
func (r *GenericSnapshotBinderController) mapBoundContentToSnapshots(snapshotGVK schema.GroupVersionKind) func(context.Context, client.Object) []reconcile.Request {
	listGVK := schema.GroupVersionKind{
		Group:   snapshotGVK.Group,
		Version: snapshotGVK.Version,
		Kind:    snapshotGVK.Kind + "List",
	}
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		if obj == nil || obj.GetName() == "" {
			return nil
		}
		contentName := obj.GetName()
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(listGVK)
		if err := r.Client.List(ctx, list); err != nil {
			log.FromContext(ctx).V(1).Info("content wake-up: failed to list snapshots",
				"snapshotKind", snapshotGVK.Kind, "content", contentName, "error", err.Error())
			return nil
		}
		var reqs []reconcile.Request
		for i := range list.Items {
			bound, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "boundSnapshotContentName")
			if bound == "" || bound != contentName {
				continue
			}
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: list.Items[i].GetNamespace(),
				Name:      list.Items[i].GetName(),
			}})
		}
		if len(reqs) > 0 {
			log.FromContext(ctx).V(1).Info("snapshotcontent change enqueues bound snapshot(s)",
				"snapshotKind", snapshotGVK.Kind, "content", contentName, "count", len(reqs))
		}
		return reqs
	}
}
