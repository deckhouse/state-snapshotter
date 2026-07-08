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

package usecase

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

// rootCaptureTestScheme is the shared fake-client scheme for the root-capture / subtree resolve / content
// graph unit tests (child_snapshot_resolve_refresh_test.go, snapshot_content_graph_test.go). It registers
// the core snapshot machinery (ssv1alpha1) and the storage snapshot types (storagev1alpha1).
func rootCaptureTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := ssv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := storagev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme storage: %v", err)
	}
	return s
}

// The in-reconciler subtree archive read (collectRunSubtreeManifestExcludeKeys + the wave barrier
// requireContentManifestsArchived + the per-MCP archive decode) moved to the subtree-manifest-identities
// service endpoint (internal/usecase/subtree_manifest_identities.go BuildSubtreeManifestIdentities +
// pkg/snapshotsdk SubtreeManifestIdentities). Its fail-closed / not-Ready / double-capture coverage lives
// in subtree_manifest_identities_test.go (server) and pkg/snapshotsdk/subtree_identities_test.go (client).
// What remains unit-tested here is the pure exclude-key shaping BuildRootNamespaceManifestCaptureTargets
// does with the identities the endpoint returns (the full builder needs a live dynamic/discovery client
// and is covered at the integration level).

func TestSubtreeIdentityExcludeKey_MatchesManifestTargetDedupKey(t *testing.T) {
	// A namespaced identity captured in a descendant subtree must produce the same dedup key the base
	// target for that object carries, so FilterManifestTargets drops it. uid is NOT part of the key.
	id := snapshotsdk.SubtreeManifestIdentity{
		APIVersion: "v1", Kind: "ConfigMap", Namespace: "ns1", Name: "covered", UID: "uid-1",
	}
	got := subtreeIdentityExcludeKey(id)
	want := namespacemanifest.ManifestTargetDedupKey("ns1", namespacemanifest.ManifestTarget{
		APIVersion: "v1", Kind: "ConfigMap", Name: "covered",
	})
	if got != want {
		t.Fatalf("subtreeIdentityExcludeKey = %q, want %q (must match ManifestTargetDedupKey)", got, want)
	}
}

func TestSubtreeIdentityExcludeKey_ClusterScopedNormalizesNamespace(t *testing.T) {
	// A cluster-scoped identity (empty namespace) must normalize to the "_cluster" bucket, matching
	// ManifestTargetDedupKey's empty-namespace handling.
	id := snapshotsdk.SubtreeManifestIdentity{APIVersion: "v1", Kind: "PersistentVolume", Name: "pv-1"}
	got := subtreeIdentityExcludeKey(id)
	want := namespacemanifest.ManifestTargetDedupKey("", namespacemanifest.ManifestTarget{
		APIVersion: "v1", Kind: "PersistentVolume", Name: "pv-1",
	})
	if got != want {
		t.Fatalf("cluster-scoped subtreeIdentityExcludeKey = %q, want %q", got, want)
	}
}

func TestFilterManifestTargets_RemovesExcludedKeys(t *testing.T) {
	base := []namespacemanifest.ManifestTarget{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "keep"},
		{APIVersion: "v1", Kind: "ConfigMap", Name: "drop"},
	}
	excl := map[string]struct{}{
		namespacemanifest.ManifestTargetDedupKey("ns1", namespacemanifest.ManifestTarget{
			APIVersion: "v1", Kind: "ConfigMap", Name: "drop",
		}): {},
	}
	out := namespacemanifest.FilterManifestTargets(base, excl, "ns1")
	if len(out) != 1 || out[0].Name != "keep" {
		t.Fatalf("unexpected filter result: %#v", out)
	}
}

// TestRootNamespaceTarget_SurvivesSubtreeExclude documents the injection caveat: BuildRootNamespaceManifest-
// CaptureTargets appends the namespace's own Namespace object (name == targetNamespace) to the root's base
// targets. Its FilterManifestTargets dedup key is built with the target namespace (non-empty), while ANY
// descendant-reported cluster-scoped identity normalizes its empty namespace to the "_cluster" bucket. Since
// no descendant ever captures the Namespace, its key can never appear in the subtree exclude set — but even
// if a "_cluster"-bucketed Namespace identity did, the two keys differ and the injected target still
// survives the filter. (The full builder needs a live dynamic/discovery client and is covered by e2e.)
func TestRootNamespaceTarget_SurvivesSubtreeExclude(t *testing.T) {
	const ns = "ns1"
	nsTarget := namespacemanifest.ManifestTarget{APIVersion: "v1", Kind: "Namespace", Name: ns}
	base := []namespacemanifest.ManifestTarget{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "cm1"},
		nsTarget,
	}

	// The injected target's dedup key uses the (non-empty) target namespace...
	injectedKey := namespacemanifest.ManifestTargetDedupKey(ns, nsTarget)
	// ...while a hypothetical subtree identity for the same Namespace normalizes to "_cluster". They differ.
	subtreeKey := subtreeIdentityExcludeKey(snapshotsdk.SubtreeManifestIdentity{
		APIVersion: "v1", Kind: "Namespace", Name: ns,
	})
	if injectedKey == subtreeKey {
		t.Fatalf("injected Namespace dedup key %q must differ from the cluster-bucket subtree key %q", injectedKey, subtreeKey)
	}

	out := namespacemanifest.FilterManifestTargets(base, map[string]struct{}{subtreeKey: {}}, ns)

	var haveNS bool
	for _, tgt := range out {
		if tgt == nsTarget {
			haveNS = true
		}
	}
	if !haveNS {
		t.Fatalf("Namespace target must survive the subtree exclude set, got %#v", out)
	}
}
