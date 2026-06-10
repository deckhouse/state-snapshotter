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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// addOrphanVolumeSnapshotWatch adds an enqueue-only watch on CSI VolumeSnapshot so orphan-PVC data-leg
// readiness wakes the owning namespace Snapshot instead of depending solely on the polling requeue.
//
// The watch is:
//   - guarded by RESTMapping so a not-yet-installed CRD (e.g. under envtest) degrades to "no watch"
//     and the reconcile-time requeue still drives progress (INV-RECONCILE-TRUTH);
//   - routed strictly by ownerRef -> Snapshot, so the VS stays a visibility leaf and never becomes a
//     SnapshotContent-backed child.
func (r *SnapshotReconciler) addOrphanVolumeSnapshotWatch(b *builder.Builder, mgr ctrl.Manager) *builder.Builder {
	logger := ctrl.Log.WithName("snapshot-controller")
	if mapper := mgr.GetRESTMapper(); mapper != nil {
		if _, err := mapper.RESTMapping(csiVolumeSnapshotGVK.GroupKind(), csiVolumeSnapshotGVK.Version); err != nil {
			logger.Info("orphan VolumeSnapshot watch skipped (GVK not RESTMappable yet); relying on reconcile-time requeue",
				"gvk", csiVolumeSnapshotGVK.String(), "reason", err.Error())
			return b
		}
	}
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
	return b.Watches(vs, handler.EnqueueRequestsFromMapFunc(mapVolumeSnapshotToOwningSnapshot))
}

// mapVolumeSnapshotToOwningSnapshot routes a VolumeSnapshot event to its owning namespace Snapshot by
// ownerRef only. It never writes status; a missing/foreign ownerRef drops the event (the next reconcile
// recomputes from truth refs). Tombstones on delete still carry ownerRefs, so routing works.
func mapVolumeSnapshotToOwningSnapshot(_ context.Context, obj client.Object) []reconcile.Request {
	if obj == nil {
		return nil
	}
	for _, ref := range obj.GetOwnerReferences() {
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "Snapshot" && ref.Name != "" {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: ref.Name}}}
		}
	}
	return nil
}
