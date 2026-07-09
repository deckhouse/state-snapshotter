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

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// mapMCRToOwningSnapshots returns a watch map function that, for a ManifestCaptureRequest change, enqueues
// the snapshot of snapshotGVK that owns it.
//
// Why this watch exists (event-driven handoff): ensureSnapshotContentLinks waits for the MCR to publish
// status.checkpointName before it can publish SnapshotContent.status.manifestCheckpointName (which in turn
// lets SnapshotContentController take ownership of the checkpoint and finalize the MCR). Without a reverse
// wake-up the binder only re-checked the MCR on the Reconcile RequeueAfter fallback, so a fast checkpoint
// still waited a full poll interval before the binder published the link. This watch makes the
// MCR -> owning-snapshot link converge event-driven; the RequeueAfter path remains only as a safety net for
// missed events.
//
// Routing is O(1) and index-free: the MCR carries a controller ownerRef to its owning snapshot (set at
// creation by snapshotsdk.ownerRef for domain kinds and by snapshot.capture for the namespace root). We
// match the ownerRef of snapshotGVK and enqueue it directly. This replaces the former namespaced
// List-of-all-snapshots + status.manifestCaptureRequestName filter, which ran a full unstructured List +
// JSON decode on every MCR event (the SETS=10 hotspot, T-index). The handler only enqueues; a missed event
// is backstopped by the binder's RequeueAfter (no List fallback — that would reintroduce the hot path).
func mapMCRToOwningSnapshots(snapshotGVK schema.GroupVersionKind) func(context.Context, client.Object) []reconcile.Request {
	wantAPIVersion := snapshotGVK.GroupVersion().String()
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		statMCRToOwningSnapshots.invoked.Add(1)
		if obj == nil || obj.GetName() == "" {
			return nil
		}
		for _, ref := range obj.GetOwnerReferences() {
			if ref.Name == "" || ref.Kind != snapshotGVK.Kind || ref.APIVersion != wantAPIVersion {
				continue
			}
			statMCRToOwningSnapshots.enqueued.Add(1)
			log.FromContext(ctx).V(1).Info("ManifestCaptureRequest change enqueues owning snapshot",
				"snapshotKind", snapshotGVK.Kind, "mcr", obj.GetName(), "snapshot", ref.Name)
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: ref.Name}}}
		}
		return nil
	}
}
