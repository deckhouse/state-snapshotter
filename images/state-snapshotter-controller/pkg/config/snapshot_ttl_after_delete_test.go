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

package config

import (
	"testing"
	"time"
)

// The production recycle-bin retention default must be 30 days (720h), not the former 1m DEBUG value.
func TestDefaultSnapshotTTLAfterDeleteIs720h(t *testing.T) {
	if DefaultSnapshotTTLAfterDelete != 720*time.Hour {
		t.Fatalf("DefaultSnapshotTTLAfterDelete = %s, want 720h (30 days)", DefaultSnapshotTTLAfterDelete)
	}
}

func TestResolveSnapshotTTLAfterDelete(t *testing.T) {
	t.Run("empty env falls back to built-in 720h default", func(t *testing.T) {
		t.Setenv(EnvSnapshotTTLAfterDelete, "")
		if got := resolveSnapshotTTLAfterDelete(); got != 720*time.Hour {
			t.Fatalf("resolveSnapshotTTLAfterDelete() = %s, want 720h", got)
		}
	})

	t.Run("env overrides default", func(t *testing.T) {
		t.Setenv(EnvSnapshotTTLAfterDelete, "48h")
		if got := resolveSnapshotTTLAfterDelete(); got != 48*time.Hour {
			t.Fatalf("resolveSnapshotTTLAfterDelete() = %s, want 48h", got)
		}
	})

	t.Run("invalid/non-positive env falls back to default", func(t *testing.T) {
		t.Setenv(EnvSnapshotTTLAfterDelete, "not-a-duration")
		if got := resolveSnapshotTTLAfterDelete(); got != 720*time.Hour {
			t.Fatalf("resolveSnapshotTTLAfterDelete() = %s, want 720h fallback", got)
		}
	})
}
