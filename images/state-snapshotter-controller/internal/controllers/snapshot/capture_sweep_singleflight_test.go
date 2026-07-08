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
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestCaptureSweepSingleflight_OneHolderPerUID(t *testing.T) {
	s := newCaptureSweepSingleflight()
	uid := types.UID("uid-a")

	if !s.TryAcquire(uid) {
		t.Fatal("first TryAcquire must succeed")
	}
	if s.TryAcquire(uid) {
		t.Fatal("second concurrent TryAcquire for the same UID must fail while held")
	}
	s.Release(uid)
	if !s.TryAcquire(uid) {
		t.Fatal("TryAcquire must succeed again after Release")
	}
	s.Release(uid)
}

func TestCaptureSweepSingleflight_DistinctUIDsIndependent(t *testing.T) {
	s := newCaptureSweepSingleflight()
	if !s.TryAcquire(types.UID("uid-a")) {
		t.Fatal("acquire uid-a must succeed")
	}
	if !s.TryAcquire(types.UID("uid-b")) {
		t.Fatal("acquire uid-b must succeed while uid-a is held (distinct Snapshots plan in parallel)")
	}
	s.Release(types.UID("uid-a"))
	s.Release(types.UID("uid-b"))
}

func TestCaptureSweepSingleflight_ReleaseUnheldIsNoop(t *testing.T) {
	s := newCaptureSweepSingleflight()
	s.Release(types.UID("never-held")) // must not panic
	if !s.TryAcquire(types.UID("never-held")) {
		t.Fatal("acquire after a no-op release must succeed")
	}
	s.Release(types.UID("never-held"))
}

// TestCaptureSweepSingleflight_ConcurrentRace models the H5 case: many concurrent reconciles of the SAME
// Snapshot UID race for the flight. At most one holder may own the UID at any instant (run with -race).
func TestCaptureSweepSingleflight_ConcurrentRace(t *testing.T) {
	s := newCaptureSweepSingleflight()
	uid := types.UID("uid-race")

	const goroutines = 64
	var (
		wg          sync.WaitGroup
		concurrent  int32 // holders currently inside the critical section
		maxObserved int32
		acquired    int32 // total successful acquisitions
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if !s.TryAcquire(uid) {
					continue
				}
				atomic.AddInt32(&acquired, 1)
				cur := atomic.AddInt32(&concurrent, 1)
				for {
					m := atomic.LoadInt32(&maxObserved)
					if cur <= m || atomic.CompareAndSwapInt32(&maxObserved, m, cur) {
						break
					}
				}
				atomic.AddInt32(&concurrent, -1)
				s.Release(uid)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxObserved); got != 1 {
		t.Fatalf("at most one concurrent holder per UID expected, observed %d", got)
	}
	if atomic.LoadInt32(&acquired) == 0 {
		t.Fatal("expected at least one successful acquisition")
	}
}
