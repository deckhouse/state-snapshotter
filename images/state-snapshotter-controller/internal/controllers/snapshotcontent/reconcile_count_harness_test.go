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

// Reconcile-count harness for the APIReader->cache swaps (Batch 1).
//
// The existing unit tests build the controller as {Client: cl, APIReader: cl} over a single fake store,
// so a swap from r.APIReader.Get to r.Client.Get is invisible: both paths hit the same object. This
// harness backs the SAME fake store with TWO interceptor wrappers - one assigned to r.Client (the cached
// manager client) and one to r.APIReader (the uncached reader) - and counts every read per path and GVK.
// That makes each swap provable ("this GET moved from APIReader to Client") and lets a before/after run
// assert that the migration did not add reconcile passes, requeues, or status writes.
//
// The cache wrapper can optionally be told to lag (first K Gets per GVK return NotFound) to model an
// informer that has not yet observed a just-created/just-linked object; the swap tests use it to prove
// Ready never regresses and the wave still converges within K extra passes.

import (
	"context"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// clientRole identifies which of the controller's two read paths performed a read. Both roles are backed
// by ONE fake store; the role only records WHERE the read went, not WHAT it returned.
type clientRole int

const (
	roleClient    clientRole = iota // r.Client (cached manager client)
	roleAPIReader                   // r.APIReader (uncached direct reader)
)

func (r clientRole) String() string {
	if r == roleClient {
		return "Client"
	}
	return "APIReader"
}

// splitCounters accumulates read/write counts per role and GVK across a reconcile run. It is safe for the
// controller's concurrent workers, though the harness drives reconciles serially.
type splitCounters struct {
	mu           sync.Mutex
	get          map[clientRole]map[schema.GroupVersionKind]int
	list         map[clientRole]map[schema.GroupVersionKind]int
	statusUpdate int
	statusPatch  int
}

func newSplitCounters() *splitCounters {
	return &splitCounters{
		get:  map[clientRole]map[schema.GroupVersionKind]int{},
		list: map[clientRole]map[schema.GroupVersionKind]int{},
	}
}

func (c *splitCounters) recordGet(role clientRole, gvk schema.GroupVersionKind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.get[role] == nil {
		c.get[role] = map[schema.GroupVersionKind]int{}
	}
	c.get[role][gvk]++
}

func (c *splitCounters) recordList(role clientRole, gvk schema.GroupVersionKind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.list[role] == nil {
		c.list[role] = map[schema.GroupVersionKind]int{}
	}
	c.list[role][gvk]++
}

func (c *splitCounters) recordStatusUpdate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statusUpdate++
}

func (c *splitCounters) recordStatusPatch() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statusPatch++
}

// getCount returns how many Gets of gvk went through role.
func (c *splitCounters) getCount(role clientRole, gvk schema.GroupVersionKind) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.get[role] == nil {
		return 0
	}
	return c.get[role][gvk]
}

// statusWrites is the total of status Update + Patch calls (both flavors route through the status
// subresource regardless of whether the caller used Status().Update or Status().Patch).
func (c *splitCounters) statusWrites() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusUpdate + c.statusPatch
}

// snapshotGet returns a deep copy of the per-role per-GVK Get counters at call time.
func (c *splitCounters) snapshotGet() map[clientRole]map[schema.GroupVersionKind]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[clientRole]map[schema.GroupVersionKind]int{}
	for role, byGVK := range c.get {
		out[role] = map[schema.GroupVersionKind]int{}
		for gvk, n := range byGVK {
			out[role][gvk] = n
		}
	}
	return out
}

// gvkOf resolves an object's GVK, preferring the embedded TypeMeta (set for unstructured) and falling back
// to the scheme (typed objects passed to Get usually have empty TypeMeta).
func gvkOf(c client.Client, obj runtime.Object) schema.GroupVersionKind {
	if gvk := obj.GetObjectKind().GroupVersionKind(); !gvk.Empty() {
		return gvk
	}
	if gvk, err := c.GroupVersionKindFor(obj); err == nil {
		return gvk
	}
	return schema.GroupVersionKind{}
}

// splitClients wraps a single backing store with two counted interceptor clients. The first return value
// is meant for r.Client (cache role, optionally lagging); the second for r.APIReader (uncached role).
//
// cacheLag, if non-nil, makes the cache role return NotFound for the first cacheLag[gvk] Gets of that GVK
// (then serve normally), modelling an informer that has not yet observed the object. The APIReader role
// never lags (it reads through to the API server).
func (c *splitCounters) splitClients(
	base client.WithWatch,
	cacheLag map[schema.GroupVersionKind]int,
) (cache client.WithWatch, apiReader client.WithWatch) {
	var lagMu sync.Mutex
	lagRemaining := map[schema.GroupVersionKind]int{}
	for gvk, n := range cacheLag {
		lagRemaining[gvk] = n
	}
	// consumeLag reports whether this Get of gvk should be served a NotFound (modelling informer
	// staleness) and decrements the remaining lag. Guarded because the cache client this wraps may be
	// driven by concurrent reconcile workers.
	consumeLag := func(gvk schema.GroupVersionKind) bool {
		lagMu.Lock()
		defer lagMu.Unlock()
		if lagRemaining[gvk] > 0 {
			lagRemaining[gvk]--
			return true
		}
		return false
	}

	cacheFuncs := interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			gvk := gvkOf(cl, obj)
			c.recordGet(roleClient, gvk)
			if consumeLag(gvk) {
				return apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: gvk.Kind}, key.Name)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			c.recordList(roleClient, gvkOf(cl, list))
			return cl.List(ctx, list, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if sub == "status" {
				c.recordStatusUpdate()
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if sub == "status" {
				c.recordStatusPatch()
			}
			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}

	apiReaderFuncs := interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			c.recordGet(roleAPIReader, gvkOf(cl, obj))
			return cl.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			c.recordList(roleAPIReader, gvkOf(cl, list))
			return cl.List(ctx, list, opts...)
		},
	}

	return interceptor.NewClient(base, cacheFuncs), interceptor.NewClient(base, apiReaderFuncs)
}

// steadyResult is the outcome of driving a reconcile step to a stable Ready (or the pass cap).
type steadyResult struct {
	passes       int // total step invocations
	requeues     int // not-ready passes (each would self-requeue in production)
	statusWrites int // status Update + Patch calls observed during the run
	getByRoleGVK map[clientRole]map[schema.GroupVersionKind]int
}

// driveToSteady runs step until it reports ready twice in a row (stable), errors, or maxPasses is reached.
// step models one reconcile pass: in production a not-ready pass self-requeues after 500ms, so the number
// of not-ready passes equals the number of requeues the controller would issue to converge.
func driveToSteady(counters *splitCounters, step func() (ready bool, err error), maxPasses int) (steadyResult, error) {
	res := steadyResult{}
	stable := 0
	for res.passes < maxPasses {
		res.passes++
		ready, err := step()
		if err != nil {
			res.statusWrites = counters.statusWrites()
			res.getByRoleGVK = counters.snapshotGet()
			return res, err
		}
		if ready {
			stable++
			if stable >= 2 {
				break
			}
		} else {
			stable = 0
			res.requeues++
		}
	}
	res.statusWrites = counters.statusWrites()
	res.getByRoleGVK = counters.snapshotGet()
	return res, nil
}

// --- harness self-tests ----------------------------------------------------

func harnessTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

// TestSplitCounters_RoutesAndCountsPerRole proves the two wrappers share one store but record reads
// against distinct roles: a Get via the APIReader wrapper increments only the APIReader counter, and a
// Get via the cache wrapper increments only the Client counter, both returning the same stored object.
func TestSplitCounters_RoutesAndCountsPerRole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := harnessTestScheme(t)
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root"}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(content).Build()

	counters := newSplitCounters()
	cache, apiReader := counters.splitClients(base, nil)

	gvk, err := apiReader.GroupVersionKindFor(&storagev1alpha1.SnapshotContent{})
	if err != nil {
		t.Fatalf("gvk: %v", err)
	}

	got := &storagev1alpha1.SnapshotContent{}
	if err := apiReader.Get(ctx, client.ObjectKey{Name: "root"}, got); err != nil {
		t.Fatalf("apiReader get: %v", err)
	}
	if got := counters.getCount(roleAPIReader, gvk); got != 1 {
		t.Fatalf("APIReader get count = %d, want 1", got)
	}
	if got := counters.getCount(roleClient, gvk); got != 0 {
		t.Fatalf("Client get count = %d, want 0 before cache read", got)
	}

	if err := cache.Get(ctx, client.ObjectKey{Name: "root"}, got); err != nil {
		t.Fatalf("cache get: %v", err)
	}
	if got := counters.getCount(roleClient, gvk); got != 1 {
		t.Fatalf("Client get count = %d, want 1", got)
	}
}

// TestSplitCounters_CacheLagReturnsNotFoundThenServes proves the optional lag mode: the cache role
// returns NotFound for the configured number of Gets per GVK (modelling informer staleness) and then
// serves the object normally, while the underlying store is untouched.
func TestSplitCounters_CacheLagReturnsNotFoundThenServes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := harnessTestScheme(t)
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root"}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(content).Build()

	counters := newSplitCounters()
	gvk, err := base.GroupVersionKindFor(&storagev1alpha1.SnapshotContent{})
	if err != nil {
		t.Fatalf("gvk: %v", err)
	}
	cache, _ := counters.splitClients(base, map[schema.GroupVersionKind]int{gvk: 2})

	got := &storagev1alpha1.SnapshotContent{}
	for i := 0; i < 2; i++ {
		if err := cache.Get(ctx, client.ObjectKey{Name: "root"}, got); !apierrors.IsNotFound(err) {
			t.Fatalf("lagged get %d: err = %v, want NotFound", i, err)
		}
	}
	if err := cache.Get(ctx, client.ObjectKey{Name: "root"}, got); err != nil {
		t.Fatalf("post-lag get: %v", err)
	}
	if n := counters.getCount(roleClient, gvk); n != 3 {
		t.Fatalf("cache get count = %d, want 3 (2 lagged + 1 served)", n)
	}
}

// TestDriveToSteady_CountsPassesAndRequeues proves the loop bookkeeping: a step that is not-ready for the
// first two passes and ready afterward yields exactly two requeues and stops once Ready is stable.
func TestDriveToSteady_CountsPassesAndRequeues(t *testing.T) {
	t.Parallel()
	counters := newSplitCounters()
	notReady := 2
	calls := 0
	step := func() (bool, error) {
		calls++
		if notReady > 0 {
			notReady--
			return false, nil
		}
		return true, nil
	}
	res, err := driveToSteady(counters, step, 10)
	if err != nil {
		t.Fatalf("driveToSteady: %v", err)
	}
	if res.requeues != 2 {
		t.Fatalf("requeues = %d, want 2", res.requeues)
	}
	// 2 not-ready passes + 2 ready passes (stable confirmation) = 4.
	if res.passes != 4 {
		t.Fatalf("passes = %d, want 4", res.passes)
	}
}
