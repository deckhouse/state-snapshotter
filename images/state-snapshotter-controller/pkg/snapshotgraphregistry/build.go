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

// Package snapshotgraphregistry builds and refreshes the generic Snapshot graph GVK registry
// from graph built-ins, eligible DomainSpecificSnapshotController rows, and RESTMapper discovery.
package snapshotgraphregistry

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/dscregistry"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// BuildRegistry merges graph built-ins + eligible DSC snapshot↔content pairs, filters to GVKs present in the
// apiserver (RESTMapper), and returns a new GVKRegistry snapshot (immutable after return).
// Eligible pairs come from a fresh List of DSC objects each call — deleted or no longer eligible DSC rows
// disappear from the merged set on the next refresh (no sticky DSC-derived kinds).
func BuildRegistry(ctx context.Context, mapper meta.RESTMapper, apiReader client.Reader, cfg *config.Options, log logr.Logger) (*snapshot.GVKRegistry, error) {
	if cfg == nil {
		return nil, fmt.Errorf("snapshot graph registry: config is nil")
	}
	if mapper == nil {
		return nil, fmt.Errorf("snapshot graph registry: RESTMapper is nil")
	}
	if apiReader == nil {
		return nil, fmt.Errorf("snapshot graph registry: API reader is nil")
	}
	dscPairs, err := dscregistry.EligibleUnifiedGVKPairs(ctx, apiReader)
	if err != nil {
		log.Info("snapshot graph registry: DSC list/parse failed; using graph built-ins only", "error", err)
		dscPairs = nil
	}
	merged := unifiedbootstrap.MergeBootstrapAndDSCPairs(unifiedbootstrap.DefaultGraphRegistryBuiltInPairs(), dscPairs)
	snapGVKs, contentGVKs := unifiedbootstrap.ResolveAvailableUnifiedGVKPairs(mapper, merged, log.WithName("snapshot-graph-registry-build"))
	reg, err := snapshot.NewGVKRegistryFromParallelSnapshotContentPairs(snapGVKs, contentGVKs)
	if err != nil {
		return nil, fmt.Errorf("snapshot graph registry: NewGVKRegistryFromParallelSnapshotContentPairs: %w", err)
	}
	return reg, nil
}
