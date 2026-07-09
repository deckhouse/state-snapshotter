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
	"fmt"
	"os"
	"strconv"
	"strings"
)

// EnvRelayMaxConcurrentReconciles overrides the per-child-GVK child-snapshot watch relay
// (nss-chw-*, internal/controllers/snapshot/dynamic_watch.go) MaxConcurrentReconciles. The relay forwards
// child ChildrenSnapshotReady transitions to the parent Snapshot; it defaults to the controller-runtime
// value of 1 (single goroutine per GVK), which serializes every namespace's child events through one
// worker. Raising it parallelizes the parent wake-ups (the child->parent lookup is a cached indexed read
// with no shared mutable state). Read ONCE at process start; changing it requires a pod/rollout restart.
const EnvRelayMaxConcurrentReconciles = "STATE_SNAPSHOTTER_RELAY_MAX_CONCURRENT_RECONCILES"

// ParseMaxConcurrentReconciles reads a positive-integer MaxConcurrentReconciles override from envName,
// falling back to def when unset/empty. An invalid (non-numeric or non-positive) value returns an error so
// the caller can fail fast rather than silently running on an unintended concurrency.
func ParseMaxConcurrentReconciles(envName string, def int) (int, error) {
	return resolveMaxConcurrentReconciles(envName, os.Getenv(envName), def)
}

// resolveMaxConcurrentReconciles is the pure parser (no env access) so it can be unit-tested directly.
func resolveMaxConcurrentReconciles(name, raw string, def int) (int, error) {
	n := def
	if v := strings.TrimSpace(raw); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("%s=%q: not a valid integer: %w", name, v, err)
		}
		if p <= 0 {
			return 0, fmt.Errorf("%s=%q: must be > 0", name, v)
		}
		n = p
	}
	return n, nil
}
