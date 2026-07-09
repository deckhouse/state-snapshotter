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
	"os"
	"sync"
	"sync/atomic"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// Diagnostics-only reverse-watch mapper stats.
//
// After T-index the three reverse-watch mappers route event -> owning/child Snapshot(s) by direct reference
// (content.spec.snapshotRef / MCR ownerRef / parent.status.childrenSnapshotRefs) instead of a full
// List+decode. That removes the CPU/allocation hot path, but it does NOT change how many events arrive nor
// how many reconcile.Requests are enqueued. These counters answer that separate question — "did enqueue
// count stay the same while CPU dropped?" (mapper cost vs critical path) — without needing a profiler.
//
// The atomic counters below are always incremented (allocation-free, lock-free) so there is no gating on the
// hot path. The periodic REPORTER that logs them is gated behind STATE_SNAPSHOTTER_WATCH_MAP_STATS so normal
// operation stays quiet; set it to a Go duration (e.g. "30s") or "true"/"1" (defaults to 30s) to enable.
const watchMapStatsEnvName = "STATE_SNAPSHOTTER_WATCH_MAP_STATS"

// mapperCounter holds monotonic invocation/enqueue counters for one reverse-watch mapper. Fields are
// incremented directly on the (lock-free) mapper hot path: invoked once per event, enqueued by the number
// of reconcile.Requests returned.
type mapperCounter struct {
	invoked  atomic.Int64
	enqueued atomic.Int64
}

var (
	statBoundContentToSnapshots mapperCounter // mapBoundContentToSnapshots
	statParentContentToChildren mapperCounter // mapParentContentToChildSnapshots
	statMCRToOwningSnapshots    mapperCounter // mapMCRToOwningSnapshots

	statsReporterOnce sync.Once
)

func watchMapStatsInterval() (time.Duration, bool) {
	v := os.Getenv(watchMapStatsEnvName)
	if v == "" {
		return 0, false
	}
	switch v {
	case "1", "true", "TRUE", "on":
		return 30 * time.Second, true
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 30 * time.Second, true
	}
	return d, true
}

// ensureWatchMapStatsReporter starts (at most once, process-wide) the gated background reporter as a manager
// runnable so it shares the manager's lifecycle/context. No-op unless STATE_SNAPSHOTTER_WATCH_MAP_STATS is
// set. Safe to call from both SetupWithManager and AddWatchForPair (runtime/CSD path).
func (r *GenericSnapshotBinderController) ensureWatchMapStatsReporter(mgr ctrl.Manager) {
	statsReporterOnce.Do(func() {
		interval, enabled := watchMapStatsInterval()
		if !enabled {
			return
		}
		_ = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			runWatchMapStatsReporter(ctx, interval)
			return nil
		}))
	})
}

func runWatchMapStatsReporter(ctx context.Context, interval time.Duration) {
	logger := ctrl.Log.WithName("genericbinder-watch-map-stats")
	logger.Info("reverse-watch mapper stats reporter enabled", "interval", interval.String())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logger.Info("reverse-watch mapper stats (cumulative)",
				"mapBoundContentToSnapshots.invoked", statBoundContentToSnapshots.invoked.Load(),
				"mapBoundContentToSnapshots.enqueued", statBoundContentToSnapshots.enqueued.Load(),
				"mapParentContentToChildSnapshots.invoked", statParentContentToChildren.invoked.Load(),
				"mapParentContentToChildSnapshots.enqueued", statParentContentToChildren.enqueued.Load(),
				"mapMCRToOwningSnapshots.invoked", statMCRToOwningSnapshots.invoked.Load(),
				"mapMCRToOwningSnapshots.enqueued", statMCRToOwningSnapshots.enqueued.Load(),
			)
		}
	}
}
