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
	"testing"
	"time"

	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func TestPhaseATraceEnabled(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"  ", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"on", true},
		{"yes", true},
		{"2", true},
	} {
		if got := phaseATraceEnabled(tc.raw); got != tc.want {
			t.Errorf("phaseATraceEnabled(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestConditionCurrentTrueLastTransition(t *testing.T) {
	const gen int64 = 7
	ts := "2026-07-09T12:34:56Z"
	want, _ := time.Parse(time.RFC3339, ts)

	cond := func(status string, observed interface{}, ltt string) []interface{} {
		m := map[string]interface{}{
			"type":               snapshotpkg.ConditionChildrenSnapshotReady,
			"status":             status,
			"lastTransitionTime": ltt,
		}
		if observed != nil {
			m["observedGeneration"] = observed
		}
		return []interface{}{m}
	}

	t.Run("current true returns parsed timestamp", func(t *testing.T) {
		got, ok := conditionCurrentTrueLastTransition(cond("True", int64(gen), ts), snapshotpkg.ConditionChildrenSnapshotReady, gen)
		if !ok || !got.Equal(want) {
			t.Fatalf("got (%v,%v), want (%v,true)", got, ok, want)
		}
	})
	t.Run("float64 observedGeneration (unstructured) is honored", func(t *testing.T) {
		if _, ok := conditionCurrentTrueLastTransition(cond("True", float64(gen), ts), snapshotpkg.ConditionChildrenSnapshotReady, gen); !ok {
			t.Fatal("float64 observedGeneration should be treated as current")
		}
	})
	t.Run("stale generation is not current", func(t *testing.T) {
		if _, ok := conditionCurrentTrueLastTransition(cond("True", int64(gen-1), ts), snapshotpkg.ConditionChildrenSnapshotReady, gen); ok {
			t.Fatal("stale observedGeneration must not be reported as current")
		}
	})
	t.Run("missing observedGeneration is not current", func(t *testing.T) {
		if _, ok := conditionCurrentTrueLastTransition(cond("True", nil, ts), snapshotpkg.ConditionChildrenSnapshotReady, gen); ok {
			t.Fatal("missing observedGeneration must not be reported as current")
		}
	})
	t.Run("false status is skipped", func(t *testing.T) {
		if _, ok := conditionCurrentTrueLastTransition(cond("False", int64(gen), ts), snapshotpkg.ConditionChildrenSnapshotReady, gen); ok {
			t.Fatal("Status=False must not return a timestamp")
		}
	})
	t.Run("unparseable timestamp fails closed", func(t *testing.T) {
		if _, ok := conditionCurrentTrueLastTransition(cond("True", int64(gen), "not-a-time"), snapshotpkg.ConditionChildrenSnapshotReady, gen); ok {
			t.Fatal("unparseable lastTransitionTime must return ok=false")
		}
	})
}
