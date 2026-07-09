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

package domain_rbac

import (
	"fmt"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

// gvrResolver resolves a SnapshotGVKRef to a GVR.
// Using a function type enables deterministic unit testing without a live API server.
type gvrResolver func(ref v1alpha1.SnapshotGVKRef) (schema.GroupVersionResource, error)

// restMapperResolver returns a gvrResolver backed by the given RESTMapper.
func restMapperResolver(mapper apimeta.RESTMapper) gvrResolver {
	return func(ref v1alpha1.SnapshotGVKRef) (schema.GroupVersionResource, error) {
		return resolveGVKRef(mapper, ref)
	}
}

// resolveGVKRef resolves a SnapshotGVKRef (apiVersion + kind) to a GVR via discovery.
func resolveGVKRef(mapper apimeta.RESTMapper, ref v1alpha1.SnapshotGVKRef) (schema.GroupVersionResource, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("parse apiVersion %q: %w", ref.APIVersion, err)
	}

	gvk := gv.WithKind(ref.Kind)

	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("RESTMapping for %s: %w", gvk.String(), err)
	}

	return mapping.Resource, nil
}

// resolveEligibleGVRs resolves source and snapshot GVKs to GVRs for every eligible CSD.
// It returns ordered, deduplicated source and snapshot GVR slices (insertion order, cross-CSD
// dedup), and a map of CSD names → pending reason for CSDs where any resolution failed.
// Successfully resolved GVRs from a partially-failed CSD are still included (they benefit other CSDs).
func resolveEligibleGVRs(
	eligible []v1alpha1.CustomSnapshotDefinition,
	resolve gvrResolver,
) (sourceGVRs, snapshotGVRs []schema.GroupVersionResource, pendingByName map[string]string) {
	pendingByName = make(map[string]string)
	seenSource := make(map[schema.GroupVersionResource]struct{})
	seenSnapshot := make(map[schema.GroupVersionResource]struct{})

	for _, csd := range eligible {
		var errMsgs []string
		// Flat CSD schema: one source GVK and one snapshot GVK per object. The snapshot GVK is the
		// object's own apiVersion/kind; the source is referenced by spec.source.
		snapshotRef := v1alpha1.SnapshotGVKRef{APIVersion: csd.Spec.APIVersion, Kind: csd.Spec.Kind}

		// On resolve error the returned GVR is the zero value; never collect it, or the
		// ClusterRole would gain an empty (resource: "") rule. Record the failure so the
		// CSD stays pending and is retried once discovery catches up.
		srcGVR, err := resolve(csd.Spec.Source)
		if err != nil {
			errMsgs = append(errMsgs, fmt.Sprintf("source %s/%s: %v", csd.Spec.Source.APIVersion, csd.Spec.Source.Kind, err))
		} else if _, ok := seenSource[srcGVR]; !ok {
			seenSource[srcGVR] = struct{}{}
			sourceGVRs = append(sourceGVRs, srcGVR)
		}

		snapGVR, err := resolve(snapshotRef)
		if err != nil {
			errMsgs = append(errMsgs, fmt.Sprintf("snapshot %s/%s: %v", snapshotRef.APIVersion, snapshotRef.Kind, err))
		} else if _, ok := seenSnapshot[snapGVR]; !ok {
			seenSnapshot[snapGVR] = struct{}{}
			snapshotGVRs = append(snapshotGVRs, snapGVR)
		}

		if len(errMsgs) > 0 {
			pendingByName[csd.Name] = "GVR resolution failed: " + strings.Join(errMsgs, "; ")
		}
	}
	return sourceGVRs, snapshotGVRs, pendingByName
}
