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

// EligibleResourceSnapshotMapping is one CSD row resolved to concrete resource, snapshot, and content types.
type EligibleResourceSnapshotMapping struct {
	SourceGVR       schema.GroupVersionResource
	SourceGVK       schema.GroupVersionKind
	SnapshotGVR     schema.GroupVersionResource
	SnapshotGVK     schema.GroupVersionKind
	SnapshotContent schema.GroupVersionKind
	Priority        int32
}

// EligibleUnifiedGVKPairs returns UnifiedGVKPair entries from every snapshotResourceMapping row
// of CSD objects that satisfy CSDWatchEligible. Duplicate snapshot GVKs in the output are skipped
// (first wins). Caller should merge with bootstrap pairs; invalid CRDs are skipped (no error).
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
		for _, entry := range d.Spec.SnapshotResourceMapping {
			_, snapGVK, err := resolveEntryGVKs(ctx, c, entry)
			if err != nil {
				continue
			}
			key := snapGVK.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, unifiedbootstrap.UnifiedGVKPair{
				Snapshot:        snapGVK,
				SnapshotContent: unifiedbootstrap.CommonSnapshotContentGVK(),
			})
		}
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
		for _, entry := range d.Spec.SnapshotResourceMapping {
			sourceGVK, snapshotGVK, err := resolveEntryGVKs(ctx, c, entry)
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
				SourceGVR:       sourceMapping.Resource,
				SourceGVK:       sourceGVK,
				SnapshotGVR:     snapshotMapping.Resource,
				SnapshotGVK:     snapshotGVK,
				SnapshotContent: unifiedbootstrap.CommonSnapshotContentGVK(),
				Priority:        entry.Priority,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].SourceGVK.String() < out[j].SourceGVK.String()
	})
	return out, nil
}

func resolveEntryGVKs(ctx context.Context, c client.Reader, entry ssv1alpha1.SnapshotResourceMappingEntry) (schema.GroupVersionKind, schema.GroupVersionKind, error) {
	_ = ctx
	_ = c
	sourceGVK, err := gvkFromRef(entry.Source)
	if err != nil {
		return schema.GroupVersionKind{}, schema.GroupVersionKind{}, fmt.Errorf("source GVK: %w", err)
	}
	snapshotGVK, err := gvkFromRef(entry.Snapshot)
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
