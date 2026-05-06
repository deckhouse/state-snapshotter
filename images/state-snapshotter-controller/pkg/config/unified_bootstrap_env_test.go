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

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestParseUnifiedBootstrapPairsEnv(t *testing.T) {
	t.Parallel()
	mode, pairs, err := ParseUnifiedBootstrapPairsEnv("")
	if err != nil || mode != UnifiedBootstrapDefault || pairs != nil {
		t.Fatalf("empty: mode=%v pairs=%v err=%v", mode, pairs, err)
	}
	mode, pairs, err = ParseUnifiedBootstrapPairsEnv(" empty ")
	if err != nil || mode != UnifiedBootstrapEmpty || pairs != nil {
		t.Fatalf("empty keyword: mode=%v", mode)
	}
	mode, pairs, err = ParseUnifiedBootstrapPairsEnv("test.deckhouse.io/v1alpha1/Foo|test.deckhouse.io/v1alpha1/FooContent")
	if err != nil || mode != UnifiedBootstrapCustom || len(pairs) != 1 {
		t.Fatalf("custom: %v %v %v", mode, pairs, err)
	}
	if pairs[0].Snapshot != (schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "Foo"}) {
		t.Fatalf("snap: %+v", pairs[0].Snapshot)
	}
}

func TestParseGVKTripleErrors(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "a/b", "only/kind", "/v1/K"} {
		if _, err := parseGVKTriple(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}
