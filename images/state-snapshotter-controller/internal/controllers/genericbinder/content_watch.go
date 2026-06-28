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
	"strings"

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

// mapParentContentToChildSnapshots returns a watch map function that, for a (parent) SnapshotContent change,
// enqueues the child snapshots of childGVK that are waiting for that content to exist before they can resolve
// their parent's SnapshotContent ownerRef (controllercommon.ResolveParentSnapshotContentOwnerRef in Step 2
// of Reconcile).
//
// Why this watch exists (event-driven parent->child unblock): a child snapshot cannot create/own its own
// SnapshotContent until the PARENT's SnapshotContent exists (the child's content ownerRef points at the
// parent content). Until then Reconcile returns the RequeueAfter fallback, so the child only re-checked the
// parent once per poll interval. This watch wakes the waiting children the moment the parent content appears
// (or changes), collapsing the multi-second ladder; the RequeueAfter path remains only as a safety net.
//
// Resolution: every SnapshotContent carries spec.snapshotRef — the binding-subject snapshot it belongs to,
// which is exactly the PARENT of the children we must wake. We list childGVK in the subject's namespace and
// match the snapshot-parent ownerRef (kind suffix "Snapshot", same name, and uid when present) against the
// subject. This covers both tree levels: the namespace-root Snapshot's content wakes first-level domain
// children, and a domain parent's content (e.g. a VM snapshot) wakes its own children (e.g. the VM's disk).
//
// Why List+filter: same dynamic-registration tradeoff as mapBoundContentToSnapshots — a field index cannot
// be guaranteed before cache start for runtime-registered (CSD-driven) GVKs. The handler only enqueues.
func (r *GenericSnapshotBinderController) mapParentContentToChildSnapshots(childGVK schema.GroupVersionKind) func(context.Context, client.Object) []reconcile.Request {
	listGVK := schema.GroupVersionKind{
		Group:   childGVK.Group,
		Version: childGVK.Version,
		Kind:    childGVK.Kind + "List",
	}
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		content, ok := obj.(*unstructured.Unstructured)
		if !ok || content == nil {
			return nil
		}
		parentName, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "name")
		if parentName == "" {
			return nil
		}
		parentUID, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "uid")
		parentNS, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "namespace")

		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(listGVK)
		opts := []client.ListOption{}
		if parentNS != "" {
			opts = append(opts, client.InNamespace(parentNS))
		}
		if err := r.Client.List(ctx, list, opts...); err != nil {
			log.FromContext(ctx).V(1).Info("parent-content wake-up: failed to list child snapshots",
				"childKind", childGVK.Kind, "content", content.GetName(), "error", err.Error())
			return nil
		}
		var reqs []reconcile.Request
		for i := range list.Items {
			child := &list.Items[i]
			for _, ref := range child.GetOwnerReferences() {
				if !strings.HasSuffix(ref.Kind, "Snapshot") || ref.Name != parentName {
					continue
				}
				if parentUID != "" && string(ref.UID) != parentUID {
					continue
				}
				reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
					Namespace: child.GetNamespace(),
					Name:      child.GetName(),
				}})
				break
			}
		}
		if len(reqs) > 0 {
			log.FromContext(ctx).V(1).Info("parent SnapshotContent change enqueues waiting child snapshot(s)",
				"childKind", childGVK.Kind, "content", content.GetName(), "count", len(reqs))
		}
		return reqs
	}
}
