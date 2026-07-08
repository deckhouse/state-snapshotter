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

package volumecapture

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// CollectSubtreeCoveredPVCUIDsFromSnapshot returns the PVC UIDs already covered by descendant snapshot
// nodes, discovering the descendants through the Snapshot child graph (status.childrenSnapshotRefs) rather
// than the ROOT's bound SnapshotContent tree — hence "root-content-free".
//
// wave7 ("late Planned"): residual/orphan-PVC coverage (and the root manifest-exclude set derived from it)
// must be computable BEFORE the ROOT SnapshotContent is bound, so coverage cannot start from the root
// content's childrenSnapshotContentRefs. The Snapshot child graph is populated at planning time (before the
// root content bind), and every descendant node exposes its OWN already-bound content via
// status.boundSnapshotContentName. Coverage is read from each descendant node's own content
// (status.data.source.uid + any in-flight VCR), reusing coveredPVCUIDsForContent so the covered-UID
// semantics stay identical to the content-tree variant (CollectSubtreeCoveredPVCUIDs).
//
// Invariants mirrored from the content-tree variant:
//   - the root itself is excluded (only its descendants count);
//   - orphan/residual-PVC VolumeSnapshot children are ordinary domain descendants now (content-single-writer
//     design §11.6): they are recursed into and cover their own PVC UID via their bound content's status.data
//     like every other data-bearing node — there is no visibility-leaf carve-out (the full coverage rewrite,
//     CSD RequiresDataArtifact + native-CSI snapshotSource fallback, lands in Block 5);
//   - claiming the same PVC UID in two descendants is fail-closed (ErrDuplicateCoveredPVCUID);
//   - a descendant not yet bound (or whose content has no data yet) contributes no covered UID; callers gate
//     the residual wave on all domain children being Ready (allDeclaredDomainChildSnapshotsReady) so the
//     content and its data exist before coverage matters. A referenced child object (or its named bound
//     content) that cannot be read is a hard error (fail-closed): silently under-covering would let an
//     already-captured PVC be re-captured by the residual wave. The ONE exception is an absent CSI
//     VolumeSnapshot child — the residual wave's own deterministically-named (rootUID, pvcUID) output: it
//     is skipped (not an error) so its PVC re-classifies as residual and the wave recreates it at the same
//     name; failing closed there would wedge the wave before the recreate path runs.
func CollectSubtreeCoveredPVCUIDsFromSnapshot(
	ctx context.Context,
	c client.Reader,
	snap *storagev1alpha1.Snapshot,
) (map[types.UID]struct{}, error) {
	if snap == nil {
		return nil, fmt.Errorf("root Snapshot is required")
	}
	namespace := snap.Namespace
	if namespace == "" {
		return nil, fmt.Errorf("namespace is required for subtree PVC coverage")
	}
	covered := make(map[types.UID]struct{})
	uidOwner := make(map[types.UID]string)
	visited := make(map[string]struct{})

	// The root's own node is not counted (a namespace root aggregator owns no data, and the content-tree
	// variant likewise excludes the root); start from the root's direct children.
	if err := walkSnapshotChildRefsForCoverage(ctx, c, namespace, snap.Status.ChildrenSnapshotRefs, visited, covered, uidOwner); err != nil {
		return nil, err
	}
	return covered, nil
}

func walkSnapshotChildRefsForCoverage(
	ctx context.Context,
	c client.Reader,
	namespace string,
	refs []storagev1alpha1.SnapshotChildRef,
	visited map[string]struct{},
	covered map[types.UID]struct{},
	uidOwner map[types.UID]string,
) error {
	sorted := append([]storagev1alpha1.SnapshotChildRef(nil), refs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, ref := range sorted {
		if ref.Name == "" {
			continue
		}
		key := ref.APIVersion + "/" + ref.Kind + "/" + ref.Name
		if _, ok := visited[key]; ok {
			return fmt.Errorf("Snapshot child graph cycle at %q", key)
		}
		visited[key] = struct{}{}

		child := &unstructured.Unstructured{}
		child.SetGroupVersionKind(schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind))
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, child); err != nil {
			if apierrors.IsNotFound(err) {
				// An absent CSI VolumeSnapshot child is the residual/orphan wave's OWN output, named
				// deterministically by (rootUID, pvcUID): skipping it re-classifies its PVC as residual so
				// the wave RECREATES it at the same name (idempotent create/adopt) — never a double-capture.
				// Failing closed here instead would WEDGE the wave, because coverage errors requeue in
				// ensureOrphanVolumeSnapshotsPrePlanned before the recreate path (EnsureChildren) can run.
				// Every OTHER kind of missing child stays fail-closed: it is not self-recreating, so silently
				// under-covering could let an already-captured PVC be re-captured by the residual wave.
				if ref.APIVersion == snapshotpkg.CSISnapshotAPIVersion && ref.Kind == snapshotpkg.KindVolumeSnapshot {
					continue
				}
				return fmt.Errorf("Snapshot child %q not found: %w", key, err)
			}
			return fmt.Errorf("get Snapshot child %q: %w", key, err)
		}

		if err := addCoverageFromChildBoundContent(ctx, c, namespace, child, key, covered, uidOwner); err != nil {
			return err
		}

		grandRefs, err := childSnapshotChildRefs(child)
		if err != nil {
			return fmt.Errorf("Snapshot child %q: %w", key, err)
		}
		if err := walkSnapshotChildRefsForCoverage(ctx, c, namespace, grandRefs, visited, covered, uidOwner); err != nil {
			return err
		}
	}
	return nil
}

// addCoverageFromChildBoundContent reads the covered PVC UIDs of one descendant Snapshot node from its OWN
// bound SnapshotContent (status.boundSnapshotContentName), reusing coveredPVCUIDsForContent so the covered
// set is identical to the content-tree walk. An unbound node contributes nothing; a named-but-unreadable
// bound content is a hard error (fail-closed).
func addCoverageFromChildBoundContent(
	ctx context.Context,
	c client.Reader,
	namespace string,
	child *unstructured.Unstructured,
	childKey string,
	covered map[types.UID]struct{},
	uidOwner map[types.UID]string,
) error {
	contentName, _, err := unstructured.NestedString(child.Object, "status", "boundSnapshotContentName")
	if err != nil {
		return fmt.Errorf("read Snapshot child %q status.boundSnapshotContentName: %w", childKey, err)
	}
	if contentName == "" {
		return nil
	}
	content := &storagev1alpha1.SnapshotContent{}
	if err := c.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("bound SnapshotContent %q of Snapshot child %q not found: %w", contentName, childKey, err)
		}
		return fmt.Errorf("get bound SnapshotContent %q of Snapshot child %q: %w", contentName, childKey, err)
	}
	uids, err := coveredPVCUIDsForContent(ctx, c, namespace, content)
	if err != nil {
		return err
	}
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		parsed := types.UID(uid)
		if prev, dup := uidOwner[parsed]; dup {
			return fmt.Errorf("%w: %s (SnapshotContent %q and %q)", ErrDuplicateCoveredPVCUID, uid, prev, contentName)
		}
		uidOwner[parsed] = contentName
		covered[parsed] = struct{}{}
	}
	return nil
}

// childSnapshotChildRefs extracts status.childrenSnapshotRefs from an unstructured snapshot-like object
// (core Snapshot or a domain CR — the field is top-level on every snapshot object).
func childSnapshotChildRefs(obj *unstructured.Unstructured) ([]storagev1alpha1.SnapshotChildRef, error) {
	raw, found, err := unstructured.NestedSlice(obj.Object, "status", "childrenSnapshotRefs")
	if err != nil {
		return nil, fmt.Errorf("read status.childrenSnapshotRefs: %w", err)
	}
	if !found {
		return nil, nil
	}
	out := make([]storagev1alpha1.SnapshotChildRef, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		apiVersion, _, _ := unstructured.NestedString(m, "apiVersion")
		kind, _, _ := unstructured.NestedString(m, "kind")
		name, _, _ := unstructured.NestedString(m, "name")
		out = append(out, storagev1alpha1.SnapshotChildRef{APIVersion: apiVersion, Kind: kind, Name: name})
	}
	return out, nil
}
