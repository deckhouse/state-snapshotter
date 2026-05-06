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

package dscregistry

import (
	"context"
	"fmt"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// EligibleResourceSnapshotMapping is one DSC row resolved to concrete resource, snapshot, and content types.
type EligibleResourceSnapshotMapping struct {
	ResourceGVR     schema.GroupVersionResource
	ResourceGVK     schema.GroupVersionKind
	SnapshotGVK     schema.GroupVersionKind
	SnapshotContent schema.GroupVersionKind
}

// EligibleUnifiedGVKPairs returns UnifiedGVKPair entries from every snapshotResourceMapping row
// of DSC objects that satisfy DSCWatchEligible. Duplicate snapshot GVKs in the output are skipped
// (first wins). Caller should merge with bootstrap pairs; invalid CRDs are skipped (no error).
func EligibleUnifiedGVKPairs(ctx context.Context, c client.Reader) ([]unifiedbootstrap.UnifiedGVKPair, error) {
	var list ssv1alpha1.DomainSpecificSnapshotControllerList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	var out []unifiedbootstrap.UnifiedGVKPair
	for i := range list.Items {
		d := &list.Items[i]
		if !DSCWatchEligible(d) {
			continue
		}
		for _, entry := range d.Spec.SnapshotResourceMapping {
			snapCRD, err := getCRD(ctx, c, entry.SnapshotCRDName)
			if err != nil {
				continue
			}
			snapGVK, err := gvkFromCRD(snapCRD)
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

// EligibleResourceSnapshotMappings returns DSC resource→snapshot mappings used by Snapshot
// parent-owned graph construction. Invalid or cluster-scoped resource rows are skipped fail-closed.
func EligibleResourceSnapshotMappings(ctx context.Context, c client.Reader) ([]EligibleResourceSnapshotMapping, error) {
	var list ssv1alpha1.DomainSpecificSnapshotControllerList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	var out []EligibleResourceSnapshotMapping
	for i := range list.Items {
		d := &list.Items[i]
		if !DSCWatchEligible(d) {
			continue
		}
		for _, entry := range d.Spec.SnapshotResourceMapping {
			resourceCRD, err := getCRD(ctx, c, entry.ResourceCRDName)
			if err != nil || resourceCRD.Spec.Scope != extv1.NamespaceScoped {
				continue
			}
			snapCRD, err := getCRD(ctx, c, entry.SnapshotCRDName)
			if err != nil {
				continue
			}
			resourceGVK, err := gvkFromCRD(resourceCRD)
			if err != nil {
				continue
			}
			snapshotGVK, err := gvkFromCRD(snapCRD)
			if err != nil {
				continue
			}
			key := resourceGVK.String() + "=>" + snapshotGVK.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, EligibleResourceSnapshotMapping{
				ResourceGVR: schema.GroupVersionResource{
					Group:    resourceGVK.Group,
					Version:  resourceGVK.Version,
					Resource: resourceCRD.Spec.Names.Plural,
				},
				ResourceGVK:     resourceGVK,
				SnapshotGVK:     snapshotGVK,
				SnapshotContent: unifiedbootstrap.CommonSnapshotContentGVK(),
			})
		}
	}
	return out, nil
}

func getCRD(ctx context.Context, c client.Reader, name string) (*extv1.CustomResourceDefinition, error) {
	crd := &extv1.CustomResourceDefinition{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, crd); err != nil {
		return nil, err
	}
	return crd, nil
}

func gvkFromCRD(crd *extv1.CustomResourceDefinition) (schema.GroupVersionKind, error) {
	ver := ""
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Storage {
			ver = crd.Spec.Versions[i].Name
			break
		}
	}
	if ver == "" {
		return schema.GroupVersionKind{}, fmt.Errorf("CRD %q has no storage version", crd.Name)
	}
	return schema.GroupVersionKind{
		Group:   crd.Spec.Group,
		Version: ver,
		Kind:    crd.Spec.Names.Kind,
	}, nil
}
