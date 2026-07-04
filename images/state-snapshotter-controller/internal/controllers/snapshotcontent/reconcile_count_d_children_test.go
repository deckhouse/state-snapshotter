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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// readyArchivedChildContent builds a typed child SnapshotContent that is both Ready=True and has its
// subtreeManifestsPersisted latch set, so a parent aggregating it reaches Ready=True.
func readyArchivedChildContent(name string) *storagev1alpha1.SnapshotContent {
	c := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     storagev1alpha1.SnapshotContentStatus{SubtreeManifestsPersisted: true},
	}
	meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
		Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: snapshot.ReasonCompleted, Message: "ready",
	})
	return c
}

// newAggregatorHarness builds a fake store with a ready parent tree (parent + ready/archived child + ready
// MCP) and returns the controller wired with split counting clients (optional cache lag on the child
// content GVK) plus the parent object refreshed with its resourceVersion so the aggregator can write its
// status. The data leg is empty (manifest-only), so Ready is gated only on the child legs.
func newAggregatorHarness(t *testing.T, cacheLag map[schema.GroupVersionKind]int) (*SnapshotContentController, *unstructured.Unstructured, *splitCounters) {
	t.Helper()
	ctx := context.Background()
	scheme := aggScheme(t)

	parent := commonContentWithStatus("parent", "mcp-p", "child-1")
	mcp := manifestCheckpointWithReady("mcp-p", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	child := readyArchivedChildContent("child-1")

	base := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(parent, child, mcp).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		Build()

	freshParent := &unstructured.Unstructured{}
	freshParent.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	if err := base.Get(ctx, client.ObjectKey{Name: "parent"}, freshParent); err != nil {
		t.Fatalf("get parent: %v", err)
	}

	counters := newSplitCounters()
	cache, apiReader := counters.splitClients(base, cacheLag)
	r := &SnapshotContentController{Client: cache, APIReader: apiReader, Scheme: scheme, GVKRegistry: snapshot.NewGVKRegistry()}
	return r, freshParent, counters
}

// Swap D (child content reads): validateCommonContentChildren (child Ready) and
// aggregateChildrenManifestsArchived (child ManifestsArchived) read each linked child via the cached
// r.Client. With no cache lag, the aggregator converges to Ready=True with no extra passes/requeues, and
// the child-content GETs go entirely through the Client path (never the APIReader).
func TestChildContentReadFromCacheInAggregation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, parent, counters := newAggregatorHarness(t, nil)

	res, err := driveToSteady(counters, func() (bool, error) {
		return r.ReconcileCommonSnapshotContentStatus(ctx, parent)
	}, 10)
	if err != nil {
		t.Fatalf("drive to steady: %v", err)
	}
	// No lag -> Ready on the first pass, stable on the second; no extra reconciles.
	if res.requeues != 0 {
		t.Fatalf("requeues = %d, want 0 (everything ready, no lag)", res.requeues)
	}
	if res.passes != 2 {
		t.Fatalf("passes = %d, want 2 (ready immediately, confirmed stable)", res.passes)
	}
	// Confirm the converged state is Ready and stable.
	ready, err := r.ReconcileCommonSnapshotContentStatus(ctx, parent)
	if err != nil || !ready {
		t.Fatalf("post-convergence: ready=%v err=%v, want ready/no-error", ready, err)
	}

	childGVK := unifiedbootstrap.CommonSnapshotContentGVK()
	if n := counters.getCount(roleClient, childGVK); n < 1 {
		t.Fatalf("child content GET via Client = %d, want >=1 after swap", n)
	}
	if n := counters.getCount(roleAPIReader, childGVK); n != 0 {
		t.Fatalf("child content GET via APIReader = %d, want 0 after swap", n)
	}
}

// Swap D boundary: declaredNonLeafChildContentNames reads the owning Snapshot
// (spec.snapshotRef -> status.childrenSnapshotRefs) DELIBERATELY through the uncached r.APIReader, not the
// cache. That status list is the authoritative declared-child set fail-closing the one-way ManifestsArchived
// latch; a stale cache could return a smaller set that omits a just-declared child while still reporting
// complete, permanently latching the archive over an unlinked subtree (duplicate-root-capture). This test
// pins that the owner read stays on the APIReader even though the heavy child-content reads moved to cache.
func TestOwnerSnapshotReadStaysOnAPIReaderForDeclaredChildren(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := harnessTestScheme(t)

	content := contentWithSnapshotRef("c", "", "ns1", "snap-1")
	owner := ownerSnapshotWithChildren("ns1", "snap-1") // declares no non-leaf children
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner).Build()

	counters := newSplitCounters()
	cache, apiReader := counters.splitClients(base, nil)
	r := &SnapshotContentController{Client: cache, APIReader: apiReader, Scheme: scheme, GVKRegistry: snapshot.NewGVKRegistry()}

	names, complete, err := r.declaredNonLeafChildContentNames(ctx, content)
	if err != nil {
		t.Fatalf("declared children: %v", err)
	}
	if !complete || len(names) != 0 {
		t.Fatalf("got names=%v complete=%v, want []/true (owner declares no non-leaf children)", names, complete)
	}

	ownerGVK := schema.GroupVersionKind{Group: storagev1alpha1.SchemeGroupVersion.Group, Version: storagev1alpha1.SchemeGroupVersion.Version, Kind: "Snapshot"}
	if n := counters.getCount(roleAPIReader, ownerGVK); n < 1 {
		t.Fatalf("owner Snapshot GET via APIReader = %d, want >=1 (declared-set authority must stay uncached)", n)
	}
	if n := counters.getCount(roleClient, ownerGVK); n != 0 {
		t.Fatalf("owner Snapshot GET via Client = %d, want 0 (must not read the declared set from the cache)", n)
	}
}

// Swap D staleness guard: with the child content read from the (lagging) cache, a not-yet-synced child
// makes the parent stay not-ready and self-requeue (fail-closed), then converge to Ready=True once the
// cache catches up. Ready never regresses (monotonic latches), the number of status writes stays bounded
// (no write-storm while spinning on the stale child), and the reads remain on the Client path throughout.
func TestChildContentCacheLagConvergesWithoutFlap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	childGVK := unifiedbootstrap.CommonSnapshotContentGVK()
	// Lag the child content reads for the first several Gets to model informer staleness after linking.
	r, parent, counters := newAggregatorHarness(t, map[schema.GroupVersionKind]int{childGVK: 6})

	res, err := driveToSteady(counters, func() (bool, error) {
		return r.ReconcileCommonSnapshotContentStatus(ctx, parent)
	}, 20)
	if err != nil {
		t.Fatalf("drive to steady: %v", err)
	}
	if res.passes >= 20 {
		t.Fatalf("did not converge within cap: passes=%d", res.passes)
	}
	if res.requeues < 1 {
		t.Fatalf("expected cache lag to cause >=1 not-ready (requeue) pass, got %d", res.requeues)
	}
	// No write-storm: writes track real condition transitions, not the number of stale spins. The whole
	// wave has only a couple of transitions (initial pending -> archived/ready), independent of lag length.
	if res.statusWrites > 4 {
		t.Fatalf("status writes = %d, want bounded (<=4); a storm would scale with the %d stale passes", res.statusWrites, res.requeues)
	}
	// Converged Ready and stable on a further pass (no flap back to not-ready).
	ready, err := r.ReconcileCommonSnapshotContentStatus(ctx, parent)
	if err != nil || !ready {
		t.Fatalf("post-convergence: ready=%v err=%v, want ready/no-error (no flap)", ready, err)
	}
	// Routing held under lag: every child read went to the cache, never the APIReader.
	if n := counters.getCount(roleAPIReader, childGVK); n != 0 {
		t.Fatalf("child content GET via APIReader under lag = %d, want 0", n)
	}
	if n := counters.getCount(roleClient, childGVK); n < 1 {
		t.Fatalf("child content GET via Client under lag = %d, want >=1", n)
	}
}
