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

// Package unifiedbootstrap resolves which unified snapshot GVKs exist in the
// apiserver (REST discovery) so the controller can start without crashing when
// optional module CRDs are absent (S1–S2).
package unifiedbootstrap

import (
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// UnifiedGVKPair is a Snapshot-like kind and the common SnapshotContent kind.
type UnifiedGVKPair struct {
	Snapshot        schema.GroupVersionKind
	SnapshotContent schema.GroupVersionKind
	// RequiresDataArtifact marks that the snapshot kind carries a volume data leg (CSD
	// spec.requiresDataArtifact). Built-in/bootstrap pairs leave it false. It is carried through
	// merge/resolve so the generic controller's GVKRegistry learns which snapshot kinds expect a data
	// artifact.
	RequiresDataArtifact bool
}

// CommonSnapshotContentGVK returns the common storage SnapshotContent GVK used by every snapshot kind.
func CommonSnapshotContentGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "SnapshotContent"}
}

// DefaultSnapshotPair returns the built-in Snapshot pair used
// by graph registry defaults and generic runtime bootstrap.
func DefaultSnapshotPair() UnifiedGVKPair {
	return UnifiedGVKPair{
		Snapshot:        schema.GroupVersionKind{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "Snapshot"},
		SnapshotContent: CommonSnapshotContentGVK(),
	}
}

// DefaultGraphRegistryBuiltInPairs lists the only Snapshot↔SnapshotContent pairs the controller ships
// with out of the box. It contains just the core namespace-root Snapshot pair (always present in this
// repo's CRDs, always covered by static controller RBAC). Domain-specific kinds (e.g. virtualization,
// demo) are intentionally NOT built in: they enter discovery and get watches exclusively through
// eligible CustomSnapshotDefinition resources (+ the module RBAC hook that sets RBACReady=True).
//
// This is the single source of built-in pairs: it seeds both the Snapshot graph registry and the
// generic unified runtime bootstrap default (see config.EffectiveUnifiedBootstrapPairs). There is no
// separate, broader "runtime bootstrap" list anymore — a hardcoded domain pair without an RBAC
// contract would silently widen the watch surface and produce forbidden list/watch loops.
func DefaultGraphRegistryBuiltInPairs() []UnifiedGVKPair {
	return []UnifiedGVKPair{
		DefaultSnapshotPair(),
	}
}

// ResolveAvailableUnifiedGVKPairs keeps only pairs where both Snapshot and SnapshotContent
// resolve via the RESTMapper. Returns two slices of equal length: index i is one logical pair.
// Missing either side is skipped with an Info log that includes the full pair (operational visibility).
func ResolveAvailableUnifiedGVKPairs(mapper meta.RESTMapper, pairs []UnifiedGVKPair, log logr.Logger) (snapshotGVKs, snapshotContentGVKs []schema.GroupVersionKind) {
	for _, p := range pairs {
		if _, err := mapper.RESTMapping(p.Snapshot.GroupKind(), p.Snapshot.Version); err != nil {
			log.Info("skipping unified snapshot GVK pair: snapshot kind not available in API",
				"snapshot", p.Snapshot.String(), "snapshotContent", p.SnapshotContent.String(), "err", err)
			continue
		}
		if _, err := mapper.RESTMapping(p.SnapshotContent.GroupKind(), p.SnapshotContent.Version); err != nil {
			log.Info("skipping unified snapshot GVK pair: snapshot content kind not available in API",
				"snapshot", p.Snapshot.String(), "snapshotContent", p.SnapshotContent.String(), "err", err)
			continue
		}
		snapshotGVKs = append(snapshotGVKs, p.Snapshot)
		snapshotContentGVKs = append(snapshotContentGVKs, p.SnapshotContent)
	}
	return snapshotGVKs, snapshotContentGVKs
}
