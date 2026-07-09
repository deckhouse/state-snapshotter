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
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// EnvPhaseATrace enables the Phase-A (creation -> ChildrenSnapshotReady) readiness-path trace: a set of
// Info-level, greppable "phaseA-trace" log lines that attribute each root Snapshot wake-up to the child
// watch relay vs the 30s child-graph backstop and measure the observe-lag between a child publishing
// ChildrenSnapshotReady=True and the parent observing it. It is diagnosis-only (no behaviour change) and
// off by default to keep production logs quiet; set STATE_SNAPSHOTTER_PHASE_A_TRACE=1 for a measurement run.
const EnvPhaseATrace = "STATE_SNAPSHOTTER_PHASE_A_TRACE"

// phaseAWakeSource describes why a root Snapshot reconcile is running, so the trace can separate an
// event-driven child relay wake (fast) from a periodic/queue wake (which, at ~30s spacing, means the
// child-graph poll backstop drove progress instead of a child event).
type phaseAWakeSource struct {
	source       string // "relay" | "relay-delete"; empty/"queue" when not carried by the relay
	childGVK     string
	childName    string
	childReadyAt time.Time // child's ChildrenSnapshotReady=True lastTransitionTime (for relay latency)
}

type phaseAWakeKey struct{}

func contextWithPhaseAWake(ctx context.Context, w phaseAWakeSource) context.Context {
	return context.WithValue(ctx, phaseAWakeKey{}, w)
}

func phaseAWakeFromContext(ctx context.Context) (phaseAWakeSource, bool) {
	w, ok := ctx.Value(phaseAWakeKey{}).(phaseAWakeSource)
	return w, ok
}

// phaseATracer is an env-gated, diagnosis-only tracer for the root Snapshot readiness path. All methods
// are cheap no-ops when disabled. It keeps only a per-root last-reconcile timestamp (to detect a wake
// that arrived on the ~30s child-graph backstop rather than a relay event).
type phaseATracer struct {
	enabled  bool
	mu       sync.Mutex
	lastSeen map[types.NamespacedName]time.Time
}

func newPhaseATracerFromEnv() *phaseATracer {
	return &phaseATracer{
		enabled:  phaseATraceEnabled(os.Getenv(EnvPhaseATrace)),
		lastSeen: make(map[types.NamespacedName]time.Time),
	}
}

func phaseATraceEnabled(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && n > 0 {
		return true
	}
	return false
}

// entry logs one line per root reconcile, attributing the wake source (relay vs queue) and flagging when
// the gap since the previous reconcile of this root is ~the 30s child-graph backstop (i.e. progress waited
// on the poll, not on a child event). It also records the relay latency (child Ready -> parent reconcile)
// when the relay carried the wake.
func (t *phaseATracer) entry(ctx context.Context, nn types.NamespacedName) {
	if t == nil || !t.enabled {
		return
	}
	now := time.Now()
	t.mu.Lock()
	prev, had := t.lastSeen[nn]
	t.lastSeen[nn] = now
	t.mu.Unlock()

	source := "queue"
	extra := make([]interface{}, 0, 6)
	if wake, ok := phaseAWakeFromContext(ctx); ok && wake.source != "" {
		source = wake.source
		if wake.childName != "" {
			extra = append(extra, "childGVK", wake.childGVK, "childName", wake.childName)
		}
		if !wake.childReadyAt.IsZero() {
			extra = append(extra, "relayLatencyMs", now.Sub(wake.childReadyAt).Milliseconds())
		}
	}

	gapMs := int64(-1)
	viaBackstop := false
	if had {
		gap := now.Sub(prev)
		gapMs = gap.Milliseconds()
		// A non-relay wake arriving ~30s after the previous reconcile means the child-graph poll backstop
		// (snapshotChildGraphPollInterval) fired: the relay did NOT wake the root. Tolerance covers jitter.
		if source == "queue" && gap >= 27*time.Second && gap <= 40*time.Second {
			viaBackstop = true
		}
	}

	args := append([]interface{}{
		"event", "root-reconcile-entry",
		"root", nn.String(),
		"wakeSource", source,
		"gapSincePrevMs", gapMs,
		"viaBackstop", viaBackstop,
	}, extra...)
	log.FromContext(ctx).Info("phaseA-trace", args...)
}

// layerReady logs a priority-layer transition: when every child in the layer has published a current
// ChildrenSnapshotReady=True, with the observe-lag = now - the newest child ChildrenSnapshotReady=True
// lastTransitionTime. A small lag means the relay woke the parent promptly; a lag near 30s means the
// parent only advanced on the poll backstop.
func (t *phaseATracer) layerReady(ctx context.Context, nn types.NamespacedName, priority int32, layerSize int, maxChildReadyAt time.Time) {
	if t == nil || !t.enabled {
		return
	}
	observeLagMs := int64(-1)
	if !maxChildReadyAt.IsZero() {
		observeLagMs = time.Since(maxChildReadyAt).Milliseconds()
	}
	log.FromContext(ctx).Info("phaseA-trace",
		"event", "layer-ready",
		"root", nn.String(),
		"priority", priority,
		"layerSize", layerSize,
		"observeLagMs", observeLagMs,
	)
}

// rootReady logs the completing pass, where the root publishes ChildrenSnapshotReady=True: the server-side
// Phase-A total (creationTimestamp -> now) and the final observe-lag (now - newest child ready in the last
// layer). phaseATotalMs is the robust per-root number; finalObserveLagMs isolates the last hop.
func (t *phaseATracer) rootReady(ctx context.Context, nsSnap *storagev1alpha1.Snapshot, layers int, lastLayerMaxReadyAt time.Time) {
	if t == nil || !t.enabled {
		return
	}
	now := time.Now()
	totalMs := int64(-1)
	if !nsSnap.CreationTimestamp.IsZero() {
		totalMs = now.Sub(nsSnap.CreationTimestamp.Time).Milliseconds()
	}
	observeLagMs := int64(-1)
	if !lastLayerMaxReadyAt.IsZero() {
		observeLagMs = now.Sub(lastLayerMaxReadyAt).Milliseconds()
	}
	log.FromContext(ctx).Info("phaseA-trace",
		"event", "root-children-ready",
		"root", nsSnap.Namespace+"/"+nsSnap.Name,
		"phaseATotalMs", totalMs,
		"finalObserveLagMs", observeLagMs,
		"layers", layers,
	)
}

// layerMaxChildrenReadyTransition returns the newest ChildrenSnapshotReady=True lastTransitionTime among
// a layer's child snapshots (only current-generation True conditions count, matching the readiness gate).
// It is only called when the trace is enabled, and reads through the per-pass readCache (no extra API
// load). A child that is missing, unreadable, or not yet current is skipped (zero contribution).
func (r *SnapshotReconciler) layerMaxChildrenReadyTransition(ctx context.Context, namespace string, refs []storagev1alpha1.SnapshotChildRef, readCache *childSnapshotReadCache) time.Time {
	var maxTS time.Time
	for _, ref := range refs {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			continue
		}
		child := &unstructured.Unstructured{}
		child.SetGroupVersionKind(gv.WithKind(ref.Kind))
		if err := readCache.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, child); err != nil {
			continue
		}
		conditions, _, err := unstructured.NestedSlice(child.Object, "status", "conditions")
		if err != nil {
			continue
		}
		if ts, ok := conditionCurrentTrueLastTransition(conditions, snapshotpkg.ConditionChildrenSnapshotReady, child.GetGeneration()); ok && ts.After(maxTS) {
			maxTS = ts
		}
	}
	return maxTS
}

// conditionCurrentTrueLastTransition extracts the lastTransitionTime of a current-generation
// Status=True condition of the given type. Mirrors conditionSliceHasCurrentTrue's strictness: a missing
// or stale observedGeneration, or an unparseable timestamp, returns ok=false.
func conditionCurrentTrueLastTransition(conditions []interface{}, typ string, generation int64) (time.Time, bool) {
	for _, raw := range conditions {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] != typ || m["status"] != string(metav1.ConditionTrue) {
			continue
		}
		observed, ok := conditionObservedGeneration(m)
		if !ok || observed != generation {
			return time.Time{}, false
		}
		ltt, _ := m["lastTransitionTime"].(string)
		if ltt == "" {
			return time.Time{}, false
		}
		parsed, err := time.Parse(time.RFC3339, ltt)
		if err != nil {
			return time.Time{}, false
		}
		return parsed, true
	}
	return time.Time{}, false
}
