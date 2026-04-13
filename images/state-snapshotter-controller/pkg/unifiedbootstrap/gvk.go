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

// Package unifiedbootstrap resolves which unified snapshot GVKs exist in the
// apiserver (REST discovery) so the controller can start without crashing when
// optional module CRDs are absent (S1–S2).
package unifiedbootstrap

import (
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/go-logr/logr"
)

// UnifiedGVKPair is a Snapshot-like kind and its SnapshotContent kind in the same API group/version.
type UnifiedGVKPair struct {
	Snapshot        schema.GroupVersionKind
	SnapshotContent schema.GroupVersionKind
}

// DefaultDesiredUnifiedSnapshotPairs lists unified snapshot types the controller
// can support until registration is driven by DomainSpecificSnapshotController.
func DefaultDesiredUnifiedSnapshotPairs() []UnifiedGVKPair {
	return []UnifiedGVKPair{
		{
			Snapshot:        schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "Snapshot"},
			SnapshotContent: schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "SnapshotContent"},
		},
		{
			Snapshot:        schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "NamespaceSnapshot"},
			SnapshotContent: schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "SnapshotContent"},
		},
		{
			Snapshot:        schema.GroupVersionKind{Group: "snapshot.internal.virtualization.deckhouse.io", Version: "v1alpha1", Kind: "InternalVirtualizationVirtualMachineSnapshot"},
			SnapshotContent: schema.GroupVersionKind{Group: "snapshot.internal.virtualization.deckhouse.io", Version: "v1alpha1", Kind: "InternalVirtualizationVirtualMachineSnapshotContent"},
		},
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
