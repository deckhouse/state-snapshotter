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

package config

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

const (
	// EnvUnifiedSnapshotEnabled: when "false", "0", "no", "off" (case-insensitive), unified Snapshot/SnapshotContent
	// wiring and unifiedruntime.Sync are disabled; DSC reconciler still runs with nil sync. Unset = enabled.
	EnvUnifiedSnapshotEnabled = "STATE_SNAPSHOTTER_UNIFIED_ENABLED"
	// EnvUnifiedBootstrapPairs: unset/empty = use DefaultUnifiedRuntimeBootstrapPairs()
	// (legacy alias DefaultDesiredUnifiedSnapshotPairs()); literal "empty"/"none"/"dsc-only"
	// = bootstrap-only-from-DSC (empty static list); else semicolon-separated pairs
	// "group/version/Kind|group/version/Kind" (snapshot side | content side).
	EnvUnifiedBootstrapPairs = "STATE_SNAPSHOTTER_UNIFIED_BOOTSTRAP_PAIRS"
)

// UnifiedBootstrapMode selects the static bootstrap list merged with eligible DSC before RESTMapper resolve.
type UnifiedBootstrapMode int

const (
	UnifiedBootstrapDefault UnifiedBootstrapMode = iota
	UnifiedBootstrapEmpty
	UnifiedBootstrapCustom
)

// parseUnifiedSnapshotDisabled returns true if unified wiring should be skipped.
func parseUnifiedSnapshotDisabled(env string) bool {
	s := strings.TrimSpace(strings.ToLower(env))
	if s == "" {
		return false
	}
	switch s {
	case "false", "0", "no", "off":
		return true
	default:
		return false
	}
}

// ParseUnifiedBootstrapPairsEnv interprets STATE_SNAPSHOTTER_UNIFIED_BOOTSTRAP_PAIRS.
func ParseUnifiedBootstrapPairsEnv(env string) (mode UnifiedBootstrapMode, pairs []unifiedbootstrap.UnifiedGVKPair, err error) {
	s := strings.TrimSpace(env)
	if s == "" {
		return UnifiedBootstrapDefault, nil, nil
	}
	low := strings.ToLower(s)
	if low == "empty" || low == "none" || low == "dsc-only" {
		return UnifiedBootstrapEmpty, nil, nil
	}
	var out []unifiedbootstrap.UnifiedGVKPair
	for _, seg := range strings.Split(s, ";") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		pair, perr := parseUnifiedPairSegment(seg)
		if perr != nil {
			return 0, nil, perr
		}
		out = append(out, pair)
	}
	if len(out) == 0 {
		return UnifiedBootstrapEmpty, nil, nil
	}
	return UnifiedBootstrapCustom, out, nil
}

func parseUnifiedPairSegment(seg string) (unifiedbootstrap.UnifiedGVKPair, error) {
	parts := strings.SplitN(seg, "|", 2)
	if len(parts) != 2 {
		return unifiedbootstrap.UnifiedGVKPair{}, fmt.Errorf("pair %q: expected snapshot|content GVK separated by '|'", seg)
	}
	snap, err := parseGVKTriple(strings.TrimSpace(parts[0]))
	if err != nil {
		return unifiedbootstrap.UnifiedGVKPair{}, fmt.Errorf("snapshot side: %w", err)
	}
	content, err := parseGVKTriple(strings.TrimSpace(parts[1]))
	if err != nil {
		return unifiedbootstrap.UnifiedGVKPair{}, fmt.Errorf("content side: %w", err)
	}
	return unifiedbootstrap.UnifiedGVKPair{Snapshot: snap, SnapshotContent: content}, nil
}

// parseGVKTriple parses "group/version/Kind" (three slash-separated segments; group may contain dots).
func parseGVKTriple(s string) (schema.GroupVersionKind, error) {
	var zero schema.GroupVersionKind
	s = strings.TrimSpace(s)
	if s == "" {
		return zero, fmt.Errorf("empty GVK")
	}
	idx := strings.LastIndex(s, "/")
	if idx <= 0 {
		return zero, fmt.Errorf("GVK %q: need group/version/Kind", s)
	}
	kind := s[idx+1:]
	rest := s[:idx]
	idx2 := strings.LastIndex(rest, "/")
	if idx2 < 0 {
		return zero, fmt.Errorf("GVK %q: need group/version/Kind", s)
	}
	version := rest[idx2+1:]
	group := rest[:idx2]
	if kind == "" || version == "" || group == "" {
		return zero, fmt.Errorf("GVK %q: empty group, kind or version", s)
	}
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}, nil
}

// EffectiveUnifiedBootstrapPairs returns the static bootstrap slice for merge with eligible DSC.
func (o *Options) EffectiveUnifiedBootstrapPairs() []unifiedbootstrap.UnifiedGVKPair {
	switch o.UnifiedBootstrapMode {
	case UnifiedBootstrapDefault:
		return unifiedbootstrap.DefaultUnifiedRuntimeBootstrapPairs()
	case UnifiedBootstrapEmpty:
		return nil
	case UnifiedBootstrapCustom:
		return append([]unifiedbootstrap.UnifiedGVKPair(nil), o.UnifiedBootstrapCustomPairs...)
	default:
		return unifiedbootstrap.DefaultUnifiedRuntimeBootstrapPairs()
	}
}
