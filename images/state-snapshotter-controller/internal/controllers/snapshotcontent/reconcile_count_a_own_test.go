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

package snapshotcontent

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// Swap A (controller.go Reconcile, own-object GVK probe): the probe that reads the reconciled object now
// uses the cached r.Client instead of the uncached r.APIReader. This test backs ONE fake store with the
// split counting clients and proves the own-content GET moved from the APIReader path to the Client path,
// while the reconcile result is unchanged (a not-ready leaf still self-requeues at the fixed cadence).
//
// A bare leaf (finalizer already present, no manifestCheckpointName, no dataRefs, no children, no
// spec.snapshotRef) is the cleanest probe: the only object GET is the :215 probe, and aggregation makes no
// further reads (fillOwnLegs short-circuits on an empty MCP name, the child/owner walks are empty), so the
// content-GVK GET counters isolate exactly the swapped read.
func TestReconcileProbeReadsOwnContentFromCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := harnessTestScheme(t)
	gvk := unifiedbootstrap.CommonSnapshotContentGVK()

	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "leaf",
			Finalizers: []string{snapshot.FinalizerParentProtect},
		},
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(content).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).Build()

	counters := newSplitCounters()
	cache, apiReader := counters.splitClients(base, nil)
	r := &SnapshotContentController{
		Client:      cache,
		APIReader:   apiReader,
		Scheme:      scheme,
		GVKRegistry: snapshot.NewGVKRegistry(),
		clusterGVKs: []schema.GroupVersionKind{gvk},
	}

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "leaf"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Routing (the point of swap A): the own-object probe reads via the cache, never via the APIReader.
	if n := counters.getCount(roleAPIReader, gvk); n != 0 {
		t.Fatalf("own content GET via APIReader = %d, want 0 after swap", n)
	}
	if n := counters.getCount(roleClient, gvk); n < 1 {
		t.Fatalf("own content GET via Client = %d, want >=1 after swap", n)
	}

	// Parity: the swap changes only WHERE the object is read, not the outcome. A not-ready leaf still asks
	// for the fixed 500ms self-requeue.
	if res.RequeueAfter != defaultSnapshotContentRequeueAfter {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, defaultSnapshotContentRequeueAfter)
	}
	if res.Requeue {
		t.Fatalf("unexpected immediate Requeue=true for a not-ready leaf: %+v", res)
	}
}
