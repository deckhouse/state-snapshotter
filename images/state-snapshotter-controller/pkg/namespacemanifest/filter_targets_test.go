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

package namespacemanifest

import "testing"

func TestManifestTargetDedupKey(t *testing.T) {
	k := ManifestTargetDedupKey("ns1", ManifestTarget{APIVersion: "v1", Kind: "ConfigMap", Name: "cm"})
	want := "v1|ConfigMap|ns1|cm"
	if k != want {
		t.Fatalf("got %q want %q", k, want)
	}
}

func TestManifestTargetDedupKey_NamespaceIsClusterScoped(t *testing.T) {
	k := ManifestTargetDedupKey("ns1", NamespaceManifestTarget("ns1"))
	want := "v1|Namespace|_cluster|ns1"
	if k != want {
		t.Fatalf("got %q want %q", k, want)
	}
}

func TestEnsureNamespaceManifestTarget_ReaddsFilteredNamespace(t *testing.T) {
	out := EnsureNamespaceManifestTarget([]ManifestTarget{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "cm"},
	}, "ns1")
	if len(out) != 2 {
		t.Fatalf("len %d", len(out))
	}
	got := map[ManifestTarget]bool{}
	for _, target := range out {
		got[target] = true
	}
	for _, want := range []ManifestTarget{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "cm"},
		NamespaceManifestTarget("ns1"),
	} {
		if !got[want] {
			t.Fatalf("expected target %#v in %#v", want, out)
		}
	}
}

func TestFilterManifestTargets_EmptyExcludePreservesOrder(t *testing.T) {
	base := []ManifestTarget{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "b"},
		{APIVersion: "v1", Kind: "ConfigMap", Name: "a"},
	}
	out := FilterManifestTargets(base, nil, "ns1")
	if len(out) != 2 {
		t.Fatalf("len %d", len(out))
	}
	// FilterManifestTargets sorts when exclude non-empty; nil exclude returns same slice reference
	if out[0].Name != "b" {
		t.Fatalf("expected unchanged order when exclude empty, got %#v", out)
	}
}
