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

// mapMCRToOwningSnapshots returns a watch map function that, for a ManifestCaptureRequest change, enqueues
// the snapshot(s) of snapshotGVK in the MCR's namespace whose status.manifestCaptureRequestName references
// it.
//
// Why this watch exists (event-driven handoff): ensureSnapshotContentLinks waits for the MCR to publish
// status.checkpointName before it can publish SnapshotContent.status.manifestCheckpointName (which in turn
// lets SnapshotContentController take ownership of the checkpoint and finalize the MCR). Without a reverse
// wake-up the binder only re-checked the MCR on the Reconcile RequeueAfter fallback, so a fast checkpoint
// still waited a full poll interval before the binder published the link. This watch makes the
// MCR -> owning-snapshot link converge event-driven; the RequeueAfter path remains only as a safety net for
// missed events.
//
// Why List+filter and not a field index / ownerRef hop: the MCR carries no reverse Snapshot reference and
// binder watches may be registered at runtime via AddWatchForPair (CSD-driven), so a field index cannot be
// guaranteed before cache start. The snapshot owns the truth ref (status.manifestCaptureRequestName), so a
// namespaced List+filter resolves the owner for both bootstrap and runtime registration. The handler only
// enqueues; it never writes status. Mirrors mapBoundContentToSnapshots.
func (r *GenericSnapshotBinderController) mapMCRToOwningSnapshots(snapshotGVK schema.GroupVersionKind) func(context.Context, client.Object) []reconcile.Request {
	listGVK := schema.GroupVersionKind{
		Group:   snapshotGVK.Group,
		Version: snapshotGVK.Version,
		Kind:    snapshotGVK.Kind + "List",
	}
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		if obj == nil || obj.GetName() == "" {
			return nil
		}
		mcrName := obj.GetName()
		mcrNS := obj.GetNamespace()

		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(listGVK)
		opts := []client.ListOption{}
		if mcrNS != "" {
			opts = append(opts, client.InNamespace(mcrNS))
		}
		if err := r.List(ctx, list, opts...); err != nil {
			log.FromContext(ctx).V(1).Info("mcr wake-up: failed to list snapshots",
				"snapshotKind", snapshotGVK.Kind, "mcr", mcrName, "error", err.Error())
			return nil
		}
		var reqs []reconcile.Request
		for i := range list.Items {
			name, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "manifestCaptureRequestName")
			if name == "" || name != mcrName {
				continue
			}
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: list.Items[i].GetNamespace(),
				Name:      list.Items[i].GetName(),
			}})
		}
		if len(reqs) > 0 {
			log.FromContext(ctx).V(1).Info("ManifestCaptureRequest change enqueues owning snapshot(s)",
				"snapshotKind", snapshotGVK.Kind, "mcr", mcrName, "count", len(reqs))
		}
		return reqs
	}
}
