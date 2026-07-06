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
// the generic Snapshot of snapshotGVK whose status.boundSnapshotContentName references that content.
//
// Why this watch exists: SnapshotContent is the single source of truth for readiness and Snapshot.Ready is a
// verbatim mirror of it. Without a reverse wake-up the binder only re-mirrors while it is still polling
// pending content; once it has mirrored Ready=True it stops requeuing, so a later content degradation
// (Ready=True -> False, e.g. a damaged VSC/MCP surfaced by the SnapshotContentController) would leave
// Snapshot.Ready stale. This watch makes the mirror converge in both directions, event-driven, without any
// polling loop or forced periodic reconcile.
//
// Routing is O(1) and index-free: the content's OWN spec.snapshotRef is the binding-subject snapshot — the
// exact object whose status.boundSnapshotContentName points back at this content. The binder writes both
// sides atomically (creates the content with snapshotRef=this snapshot in Reconcile Step 3, then sets the
// snapshot's status.boundSnapshotContentName), and spec.snapshotRef is immutable, so it is a reliable
// inverse. This replaces the former List-of-all-snapshots-of-the-paired-GVK + filter, which ran a full
// unstructured List + JSON decode on every SnapshotContent event (the SETS=10 CPU/allocation hotspot,
// T-index). The handler only enqueues; it never writes conditions. A missed event is backstopped by the
// snapshot's own 5s Reconcile requeue (no List fallback — that would reintroduce the hot path).
func mapBoundContentToSnapshots(snapshotGVK schema.GroupVersionKind) func(context.Context, client.Object) []reconcile.Request {
	wantAPIVersion := snapshotGVK.GroupVersion().String()
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		statBoundContentToSnapshots.invoked.Add(1)
		content, ok := obj.(*unstructured.Unstructured)
		if !ok || content == nil {
			return nil
		}
		apiVersion, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "apiVersion")
		kind, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "kind")
		name, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "name")
		namespace, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "namespace")
		// Only route to this controller's paired snapshot GVK; other pairs' controllers handle their own.
		if name == "" || kind != snapshotGVK.Kind || apiVersion != wantAPIVersion {
			return nil
		}
		statBoundContentToSnapshots.enqueued.Add(1)
		log.FromContext(ctx).V(1).Info("snapshotcontent change enqueues bound snapshot",
			"snapshotKind", snapshotGVK.Kind, "content", content.GetName(), "snapshot", name)
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}}
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
// Routing (index-free, no full List): every SnapshotContent carries spec.snapshotRef — the binding-subject
// snapshot it belongs to, which is exactly the PARENT of the children we must wake. We read the parent
// snapshot's published status.childrenSnapshotRefs (the declared graph edges, populated by the domain/
// namespace planner before the parent content exists) and enqueue the refs of childGVK. This is one small
// Get by the parent's own ref plus a walk of its child edges, replacing the former List-of-all-childGVK +
// ownerRef filter that ran a full unstructured List + JSON decode on every SnapshotContent event (the
// SETS=10 hotspot, T-index). It covers both tree levels: the namespace-root Snapshot's content wakes
// first-level domain children, and a domain parent's content (e.g. a VM snapshot) wakes its own children
// (e.g. the VM's disk). Child namespace equals the parent Snapshot namespace (SnapshotChildRef is
// name/apiVersion/kind only). The handler only enqueues; a missed event is backstopped by the child
// Reconcile's RequeueAfter (no List fallback — that would reintroduce the hot path).
func (r *GenericSnapshotBinderController) mapParentContentToChildSnapshots(childGVK schema.GroupVersionKind) func(context.Context, client.Object) []reconcile.Request {
	wantChildAPIVersion := childGVK.GroupVersion().String()
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		statParentContentToChildren.invoked.Add(1)
		content, ok := obj.(*unstructured.Unstructured)
		if !ok || content == nil {
			return nil
		}
		parentName, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "name")
		parentNS, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "namespace")
		parentAPIVersion, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "apiVersion")
		parentKind, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "kind")
		if parentName == "" || parentAPIVersion == "" || parentKind == "" {
			return nil
		}
		parentGV, err := schema.ParseGroupVersion(parentAPIVersion)
		if err != nil {
			return nil
		}
		parent := &unstructured.Unstructured{}
		parent.SetGroupVersionKind(parentGV.WithKind(parentKind))
		if err := r.Get(ctx, client.ObjectKey{Namespace: parentNS, Name: parentName}, parent); err != nil {
			log.FromContext(ctx).V(1).Info("parent-content wake-up: failed to get parent snapshot",
				"childKind", childGVK.Kind, "content", content.GetName(), "parent", parentName, "error", err.Error())
			return nil
		}
		childRefs, _, _ := unstructured.NestedSlice(parent.Object, "status", "childrenSnapshotRefs")
		var reqs []reconcile.Request
		for _, raw := range childRefs {
			m, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			kind, _ := m["kind"].(string)
			apiVersion, _ := m["apiVersion"].(string)
			name, _ := m["name"].(string)
			if name == "" || kind != childGVK.Kind || apiVersion != wantChildAPIVersion {
				continue
			}
			// SnapshotChildRef is namespace-less; child namespace equals the parent Snapshot namespace.
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: parentNS, Name: name}})
		}
		statParentContentToChildren.enqueued.Add(int64(len(reqs)))
		if len(reqs) > 0 {
			log.FromContext(ctx).V(1).Info("parent SnapshotContent change enqueues waiting child snapshot(s)",
				"childKind", childGVK.Kind, "content", content.GetName(), "count", len(reqs))
		}
		return reqs
	}
}
