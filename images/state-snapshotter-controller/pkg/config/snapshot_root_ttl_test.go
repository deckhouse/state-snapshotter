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
func TestDefaultSnapshotRootOKTTLIs720h(t *testing.T) {
	if DefaultSnapshotRootOKTTL != 720*time.Hour {
		t.Fatalf("DefaultSnapshotRootOKTTL = %s, want 720h (30 days)", DefaultSnapshotRootOKTTL)
	}
}

func TestResolveSnapshotRootOKTTL(t *testing.T) {
	t.Run("empty env falls back to built-in 720h default", func(t *testing.T) {
		t.Setenv(EnvSnapshotRootOKTTL, "")
		t.Setenv(EnvSnapshotRootOKTTLAlt, "")
		if got := resolveSnapshotRootOKTTL(); got != 720*time.Hour {
			t.Fatalf("resolveSnapshotRootOKTTL() = %s, want 720h", got)
		}
	})

	t.Run("primary env overrides default", func(t *testing.T) {
		t.Setenv(EnvSnapshotRootOKTTL, "48h")
		t.Setenv(EnvSnapshotRootOKTTLAlt, "")
		if got := resolveSnapshotRootOKTTL(); got != 48*time.Hour {
			t.Fatalf("resolveSnapshotRootOKTTL() = %s, want 48h", got)
		}
	})

	t.Run("alt env used when primary unset", func(t *testing.T) {
		t.Setenv(EnvSnapshotRootOKTTL, "")
		t.Setenv(EnvSnapshotRootOKTTLAlt, "12h")
		if got := resolveSnapshotRootOKTTL(); got != 12*time.Hour {
			t.Fatalf("resolveSnapshotRootOKTTL() = %s, want 12h (alt env)", got)
		}
	})

	t.Run("invalid/non-positive env falls back to default", func(t *testing.T) {
		t.Setenv(EnvSnapshotRootOKTTL, "not-a-duration")
		t.Setenv(EnvSnapshotRootOKTTLAlt, "0s")
		if got := resolveSnapshotRootOKTTL(); got != 720*time.Hour {
			t.Fatalf("resolveSnapshotRootOKTTL() = %s, want 720h fallback", got)
		}
	})
}
