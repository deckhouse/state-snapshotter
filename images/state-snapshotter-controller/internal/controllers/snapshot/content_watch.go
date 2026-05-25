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

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
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

func snapshotContentToSnapshotEnqueueHandler(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		return MapSnapshotContentToBoundSnapshots(ctx, c, obj)
	})
}
