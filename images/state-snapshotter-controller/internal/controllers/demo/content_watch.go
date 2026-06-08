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

package demo

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// DemoSnapshotBoundContentFieldIndex indexes demo snapshots by status.boundSnapshotContentName.
//
// Why this watch exists (INV-MIRROR): demo VM/Disk snapshots are dedicated kinds (excluded from the
// generic binder, see unifiedbootstrap.DedicatedSnapshotControllerKinds), so the generic binder's
// SnapshotContent -> Snapshot wake-up does NOT cover them. Their dedicated reconcilers reach a steady
// state (content Ready=True) and then stop requeuing, so without an event-driven wake a later degradation
// of the bound common SnapshotContent (Ready=True -> False) would leave the demo Snapshot.Ready stale.
// This index + map function let a SnapshotContent status change enqueue the bound demo Snapshot so its
// reconcile re-mirrors the bound content Ready (the same role snapshotContentToSnapshotEnqueueHandler
// plays for the root Snapshot and mapBoundContentToSnapshots for generic Snapshots). The handler is
// enqueue-only; it never writes conditions.
const DemoSnapshotBoundContentFieldIndex = "status.boundSnapshotContentName"

func registerDemoDiskBoundContentFieldIndex(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &demov1alpha1.DemoVirtualDiskSnapshot{}, DemoSnapshotBoundContentFieldIndex, func(rawObj client.Object) []string {
		s, ok := rawObj.(*demov1alpha1.DemoVirtualDiskSnapshot)
		if !ok || s.Status.BoundSnapshotContentName == "" {
			return nil
		}
		return []string{s.Status.BoundSnapshotContentName}
	}); err != nil {
		return fmt.Errorf("index DemoVirtualDiskSnapshot.status.boundSnapshotContentName: %w", err)
	}
	return nil
}

func registerDemoVMBoundContentFieldIndex(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &demov1alpha1.DemoVirtualMachineSnapshot{}, DemoSnapshotBoundContentFieldIndex, func(rawObj client.Object) []string {
		s, ok := rawObj.(*demov1alpha1.DemoVirtualMachineSnapshot)
		if !ok || s.Status.BoundSnapshotContentName == "" {
			return nil
		}
		return []string{s.Status.BoundSnapshotContentName}
	}); err != nil {
		return fmt.Errorf("index DemoVirtualMachineSnapshot.status.boundSnapshotContentName: %w", err)
	}
	return nil
}

// mapContentToBoundDemoDiskSnapshots enqueues the DemoVirtualDiskSnapshot bound to the changed
// SnapshotContent (by status.boundSnapshotContentName). Enqueue-only; never writes status.
func mapContentToBoundDemoDiskSnapshots(c client.Client) func(context.Context, client.Object) []reconcile.Request {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		content, ok := obj.(*storagev1alpha1.SnapshotContent)
		if !ok || content.Name == "" {
			return nil
		}
		var list demov1alpha1.DemoVirtualDiskSnapshotList
		if err := c.List(ctx, &list, client.MatchingFields{DemoSnapshotBoundContentFieldIndex: content.Name}); err != nil {
			log.FromContext(ctx).V(1).Info("failed to list DemoVirtualDiskSnapshots bound to content; dropping wake-up (revalidation backstops on next reconcile)",
				"snapshotContent", content.Name, "error", err)
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: list.Items[i].Namespace,
				Name:      list.Items[i].Name,
			}})
		}
		return out
	}
}

// mapContentToBoundDemoVMSnapshots enqueues the DemoVirtualMachineSnapshot bound to the changed
// SnapshotContent (by status.boundSnapshotContentName). Enqueue-only; never writes status.
func mapContentToBoundDemoVMSnapshots(c client.Client) func(context.Context, client.Object) []reconcile.Request {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		content, ok := obj.(*storagev1alpha1.SnapshotContent)
		if !ok || content.Name == "" {
			return nil
		}
		var list demov1alpha1.DemoVirtualMachineSnapshotList
		if err := c.List(ctx, &list, client.MatchingFields{DemoSnapshotBoundContentFieldIndex: content.Name}); err != nil {
			log.FromContext(ctx).V(1).Info("failed to list DemoVirtualMachineSnapshots bound to content; dropping wake-up (revalidation backstops on next reconcile)",
				"snapshotContent", content.Name, "error", err)
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: list.Items[i].Namespace,
				Name:      list.Items[i].Name,
			}})
		}
		return out
	}
}
