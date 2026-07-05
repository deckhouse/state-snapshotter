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

package csdregistry

import (
	"context"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// EligibleResourceSnapshotMapping is one CSD resolved to concrete resource, snapshot, and content types.
type EligibleResourceSnapshotMapping struct {
	SourceGVR       schema.GroupVersionResource
	SourceGVK       schema.GroupVersionKind
	SnapshotGVR     schema.GroupVersionResource
	SnapshotGVK     schema.GroupVersionKind
	SnapshotContent schema.GroupVersionKind
	Weight          int32
	// RequiresDataArtifact mirrors CSD spec.requiresDataArtifact: the snapshot kind carries a volume
	// data leg.
	RequiresDataArtifact bool
}

// EligibleUnifiedGVKPairs returns one UnifiedGVKPair per CSD object that satisfies CSDWatchEligible.
// Duplicate snapshot GVKs in the output are skipped (first wins). Caller should merge with bootstrap
// pairs; invalid CRDs are skipped (no error). The pair carries spec.requiresDataArtifact so the generic
// controller's registry learns which snapshot kinds expect a data artifact.
func EligibleUnifiedGVKPairs(ctx context.Context, c client.Reader) ([]unifiedbootstrap.UnifiedGVKPair, error) {
	var list ssv1alpha1.CustomSnapshotDefinitionList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	var out []unifiedbootstrap.UnifiedGVKPair
	for i := range list.Items {
		d := &list.Items[i]
		if !CSDWatchEligible(d) {
			continue
		}
		_, snapGVK, err := resolveCSDGVKs(d)
		if err != nil {
			continue
		}
		key := snapGVK.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, unifiedbootstrap.UnifiedGVKPair{
			Snapshot:             snapGVK,
			SnapshotContent:      unifiedbootstrap.CommonSnapshotContentGVK(),
			RequiresDataArtifact: d.Spec.RequiresDataArtifact,
		})
	}
	return out, nil
}

// EligibleResourceSnapshotMappings returns CSD resource→snapshot mappings used by Snapshot
// parent-owned graph construction. Invalid or cluster-scoped resource rows are skipped fail-closed.
func EligibleResourceSnapshotMappings(ctx context.Context, c client.Reader, mapper meta.RESTMapper) ([]EligibleResourceSnapshotMapping, error) {
	var list ssv1alpha1.CustomSnapshotDefinitionList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	var out []EligibleResourceSnapshotMapping
	for i := range list.Items {
		d := &list.Items[i]
		if !CSDWatchEligible(d) {
			continue
		}
		sourceGVK, snapshotGVK, err := resolveCSDGVKs(d)
		if err != nil {
			continue
		}
		sourceMapping, err := mapper.RESTMapping(sourceGVK.GroupKind(), sourceGVK.Version)
		if err != nil || sourceMapping.Scope.Name() != meta.RESTScopeNameNamespace {
			continue
		}
		snapshotMapping, err := mapper.RESTMapping(snapshotGVK.GroupKind(), snapshotGVK.Version)
		if err != nil {
			continue
		}
		key := sourceGVK.String() + "=>" + snapshotGVK.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, EligibleResourceSnapshotMapping{
			SourceGVR:            sourceMapping.Resource,
			SourceGVK:            sourceGVK,
			SnapshotGVR:          snapshotMapping.Resource,
			SnapshotGVK:          snapshotGVK,
			SnapshotContent:      unifiedbootstrap.CommonSnapshotContentGVK(),
			Weight:               d.Spec.Weight,
			RequiresDataArtifact: d.Spec.RequiresDataArtifact,
		})
	}
	// Ascending by Weight: lower numeric weight runs first (earlier traversal wave).
	// Tie-break by SourceGVK for a stable, deterministic order within a weight layer.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight < out[j].Weight
		}
		return out[i].SourceGVK.String() < out[j].SourceGVK.String()
	})
	return out, nil
}

// resolveCSDGVKs resolves the source and snapshot GVKs from a flat CSD spec.
func resolveCSDGVKs(d *ssv1alpha1.CustomSnapshotDefinition) (schema.GroupVersionKind, schema.GroupVersionKind, error) {
	sourceGVK, err := gvkFromRef(d.Spec.Source)
	if err != nil {
		return schema.GroupVersionKind{}, schema.GroupVersionKind{}, fmt.Errorf("source GVK: %w", err)
	}
	snapshotGVK, err := gvkFromRef(ssv1alpha1.SnapshotGVKRef{APIVersion: d.Spec.APIVersion, Kind: d.Spec.Kind})
	if err != nil {
		return schema.GroupVersionKind{}, schema.GroupVersionKind{}, fmt.Errorf("snapshot GVK: %w", err)
	}
	return sourceGVK, snapshotGVK, nil
}

func gvkFromRef(ref ssv1alpha1.SnapshotGVKRef) (schema.GroupVersionKind, error) {
	if ref.APIVersion == "" || ref.Kind == "" {
		return schema.GroupVersionKind{}, fmt.Errorf("source/snapshot apiVersion and kind are required")
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	return gv.WithKind(ref.Kind), nil
}
