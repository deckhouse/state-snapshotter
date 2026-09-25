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

package unifiedruntime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var testVolumeSnapshotGVK = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}

// lateMapper is a RESTMapper whose one kind becomes mappable when the test says so — the CRD being
// installed by another module after this process started.
type lateMapper struct {
	meta.RESTMapper
	mu     sync.Mutex
	served bool
}

func (m *lateMapper) serve() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.served = true
}

func (m *lateMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.served {
		return nil, &meta.NoKindMatchError{GroupKind: gk, SearchedVersions: versions}
	}
	return &meta.RESTMapping{GroupVersionKind: gk.WithVersion(versions[0])}, nil
}

func TestRunWhenMappableWiresOnceTheKindAppears(t *testing.T) {
	mapper := &lateMapper{}
	var calls atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- RunWhenMappable(context.Background(), logr.Discard(), mapper, testVolumeSnapshotGVK, 5*time.Millisecond,
			func(context.Context) error { calls.Add(1); return nil })
	}()

	time.Sleep(30 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("wire ran %d times while the kind was not served", n)
	}

	mapper.serve()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunWhenMappable returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunWhenMappable did not return after the kind became mappable")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("wire ran %d times, want exactly 1", n)
	}
}

func TestRunWhenMappableRetriesAFailedWire(t *testing.T) {
	mapper := &lateMapper{}
	mapper.serve()
	var calls atomic.Int32
	err := RunWhenMappable(context.Background(), logr.Discard(), mapper, testVolumeSnapshotGVK, time.Millisecond,
		func(context.Context) error {
			if calls.Add(1) < 3 {
				return errors.New("informer not ready")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("RunWhenMappable returned %v", err)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("wire ran %d times, want 3 (two failures, then success)", n)
	}
}

func TestRunWhenMappableStopsWithTheContext(t *testing.T) {
	mapper := &lateMapper{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWhenMappable(ctx, logr.Discard(), mapper, testVolumeSnapshotGVK, time.Millisecond,
			func(context.Context) error { t.Error("wire must not run for a kind that never appears"); return nil })
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a cancelled wait must not be an error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunWhenMappable did not return after its context was cancelled")
	}
}
