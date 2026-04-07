/*
Copyright 2025 Flant JSC

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

package unifiedruntime

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const metricsNamespace = "state_snapshotter"

var (
	registerMetricsOnce sync.Once

	resolvedSnapshotGVKGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Subsystem: "unified_runtime",
		Name:      "resolved_snapshot_gvk_count",
		Help:      "Number of snapshot GVKs in the current resolved layered state (RESTMapper).",
	})
	activeMonotonicSnapshotGVKGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Subsystem: "unified_runtime",
		Name:      "active_monotonic_snapshot_gvk_count",
		Help:      "Number of snapshot GVK keys for which both Snapshot and SnapshotContent watches succeeded at least once in this process (monotonic; not cleared when resolved drops).",
	})
	staleActiveSnapshotGVKGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Subsystem: "unified_runtime",
		Name:      "stale_active_snapshot_gvk_count",
		Help:      "Number of active-monotonic snapshot GVK keys not present in the current resolved set (additive watches may still run; restart pod for clean informers).",
	})
)

func registerUnifiedRuntimeMetrics() {
	registerMetricsOnce.Do(func() {
		crmetrics.Registry.MustRegister(
			resolvedSnapshotGVKGauge,
			activeMonotonicSnapshotGVKGauge,
			staleActiveSnapshotGVKGauge,
		)
	})
}
