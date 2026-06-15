package domain_rbac

import (
	"fmt"
	"strings"

	"github.com/deckhouse/state-snapshotter/api/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
		for _, entry := range csd.Spec.SnapshotResourceMapping {
			srcGVR, err := resolve(entry.Source)
			if err != nil {
				errMsgs = append(errMsgs, fmt.Sprintf("source %s/%s: %v", entry.Source.APIVersion, entry.Source.Kind, err))
			}
			if _, ok := seenSource[srcGVR]; !ok {
				seenSource[srcGVR] = struct{}{}
				sourceGVRs = append(sourceGVRs, srcGVR)
			}

			snapGVR, err := resolve(entry.Snapshot)
			if err != nil {
				errMsgs = append(errMsgs, fmt.Sprintf("snapshot %s/%s: %v", entry.Snapshot.APIVersion, entry.Snapshot.Kind, err))
			}
			if _, ok := seenSnapshot[snapGVR]; !ok {
				seenSnapshot[snapGVR] = struct{}{}
				snapshotGVRs = append(snapshotGVRs, snapGVR)
			}
		}
		if len(errMsgs) > 0 {
			pendingByName[csd.Name] = "GVR resolution failed: " + strings.Join(errMsgs, "; ")
		}
	}
	return sourceGVRs, snapshotGVRs, pendingByName
}
