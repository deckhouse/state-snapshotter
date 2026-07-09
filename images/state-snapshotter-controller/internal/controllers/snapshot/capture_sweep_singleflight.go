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
	"sync"

	"k8s.io/apimachinery/pkg/types"
)

// captureSweepSingleflight de-duplicates the pre-MCR namespace capture sweep across CONCURRENT reconciles
// of the SAME root Snapshot (keyed by UID). It is a non-blocking, in-process keyed lock: TryAcquire returns
// true to exactly one holder per UID until Release, and false to any concurrent caller for the same UID.
//
// Why this exists (H5): several reconciles of one root Snapshot can run at once (the child-watch relay calls
// Reconcile directly and MaxConcurrentReconciles>1). Before any of them creates the root ManifestCaptureRequest
// they all pass the MCR-gate NotFound and each runs the full, identical namespace sweep (the extra Creates land
// on AlreadyExists). This gate lets only one reconcile plan while the others requeue and take the frozen
// mcr-present branch. It is a CONCURRENCY dedup only — temporal dedup is still owned by the MCR-gate, and the
// plan result is never cached across time (a holder that fails to create the MCR releases so a later reconcile
// re-plans).
//
// It never blocks and holds no state per UID beyond the in-flight marker, so a cancelled/failed reconcile
// cannot poison future ones: Release always removes the marker via defer.
type captureSweepSingleflight struct {
	mu       sync.Mutex
	inflight map[types.UID]struct{}
}

func newCaptureSweepSingleflight() *captureSweepSingleflight {
	return &captureSweepSingleflight{inflight: make(map[types.UID]struct{})}
}

// TryAcquire records uid as in-flight and returns true if no other holder currently owns uid; otherwise it
// returns false and does not change state. The caller that gets true MUST call Release (defer) when done.
func (s *captureSweepSingleflight) TryAcquire(uid types.UID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.inflight[uid]; ok {
		return false
	}
	s.inflight[uid] = struct{}{}
	return true
}

// Release clears the in-flight marker for uid. Safe to call for a uid that is not held (no-op).
func (s *captureSweepSingleflight) Release(uid types.UID) {
	s.mu.Lock()
	delete(s.inflight, uid)
	s.mu.Unlock()
}
