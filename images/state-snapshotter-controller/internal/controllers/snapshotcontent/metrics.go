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
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	metricsNamespace = "state_snapshotter"

	// EventReasonCaptureStateRestored is the Kubernetes event reason emitted after a confirmed
	// restore of status.captureState.commonController on a domain snapshot.
	EventReasonCaptureStateRestored = "CaptureStateRestored"
	// EventReasonPublishedIdentityHeld is the Kubernetes event reason emitted when a projection
	// refuses to rewrite an already published manifest or data identity.
	EventReasonPublishedIdentityHeld = "PublishedIdentityHeld"
)

var (
	registerMetricsOnce sync.Once

	captureStateRestoredTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "capture_state_restored_total",
		Help:      "Number of confirmed restores of status.captureState.commonController on a domain snapshot after the core-written legs were missing.",
	})
	publishedIdentityHeldTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "published_identity_held_total",
		Help:      "Number of times a SnapshotContent projection refused to rewrite an already published manifest checkpoint name or data-binding identity.",
	})
)

func registerSnapshotContentMetrics() {
	registerMetricsOnce.Do(func() {
		crmetrics.Registry.MustRegister(
			captureStateRestoredTotal,
			publishedIdentityHeldTotal,
		)
	})
}
