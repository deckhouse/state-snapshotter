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
// status.boundSnapshotContentName. Coverage is read from each descendant node's own content, reusing
// coveredPVCUIDsForContent so the covered-UID semantics stay identical to the content-tree variant
// (CollectSubtreeCoveredPVCUIDs): the data-bearing decision comes AUTHORITATIVELY from the CSD (dataBearing
// / RequiresDataArtifact keyed on the owning snapshot kind), and a data-bearing node whose status.data is
// not published yet is covered via the owner fallback (in-flight VCR name / native-CSI snapshotSource.uid,
// design §8.5/§11.7).
//
// Invariants mirrored from the content-tree variant:
//   - the root itself is excluded (only its descendants count);
//   - orphan/residual-PVC VolumeSnapshot children are ordinary domain descendants now (content-single-writer
//     design §11.6): they are recursed into and cover their own PVC UID like every other data-bearing node —
//     there is no visibility-leaf carve-out;
//   - claiming the same PVC UID in two descendants is fail-closed (ErrDuplicateCoveredPVCUID);
//   - a descendant not yet bound (or a manifest-only node) contributes no covered UID. Callers gate the
//     residual wave on all domain children reaching capture barrier 1 (allDeclaredDomainChildSnapshotsReady,
//     relaxed to phase>=Planned in Block 5) so each child's VCR/snapshotSource — the owner-fallback inputs —
//     exists before coverage matters, without waiting for its full status.data (milestone B). A referenced
//     child object (or its named bound content) that cannot be read is a hard error (fail-closed): silently
//     under-covering would let an already-captured PVC be re-captured by the residual wave. The ONE exception
//     is an absent CSI VolumeSnapshot child — the residual wave's own deterministically-named (rootUID,
//     pvcUID) output: it is skipped (not an error) so its PVC re-classifies as residual and the wave recreates
//     it at the same name; failing closed there would wedge the wave before the recreate path runs.
func CollectSubtreeCoveredPVCUIDsFromSnapshot(
	ctx context.Context,
	c client.Reader,
	snap *storagev1alpha1.Snapshot,
	dataBearing DataBearingKindFunc,
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
	if err := walkSnapshotChildRefsForCoverage(ctx, c, namespace, snap.Status.ChildrenSnapshotRefs, visited, covered, uidOwner, dataBearing); err != nil {
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
	dataBearing DataBearingKindFunc,
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

		if err := addCoverageFromChildBoundContent(ctx, c, namespace, child, key, covered, uidOwner, dataBearing); err != nil {
			return err
		}

		grandRefs, err := childSnapshotChildRefs(child)
		if err != nil {
			return fmt.Errorf("Snapshot child %q: %w", key, err)
		}
		if err := walkSnapshotChildRefsForCoverage(ctx, c, namespace, grandRefs, visited, covered, uidOwner, dataBearing); err != nil {
			return err
		}
	}
	return nil
}

// addCoverageFromChildBoundContent adds the covered PVC UIDs of one descendant Snapshot node. The
// data-bearing decision is AUTHORITATIVE from the CSD (dataBearing keyed on the node's OWN kind), not the
// shape of the subtree: a non-data-bearing aggregate contributes nothing (the walk still recurses into it).
// A data-bearing node's UID comes from coveredPVCUIDsForSnapshotNode (bound-content status.data, else the
// owner fallback read straight off the node's captureState), which fails closed with ErrSubtreeDataRefsPending
// when no coverage is observable yet — so the residual wave waits rather than under-cover.
func addCoverageFromChildBoundContent(
	ctx context.Context,
	c client.Reader,
	namespace string,
	child *unstructured.Unstructured,
	childKey string,
	covered map[types.UID]struct{},
	uidOwner map[types.UID]string,
	dataBearing DataBearingKindFunc,
) error {
	if dataBearing == nil || !dataBearing(child.GetKind()) {
		return nil
	}
	uids, ownerLabel, err := coveredPVCUIDsForSnapshotNode(ctx, c, namespace, child, childKey)
	if err != nil {
		return err
	}
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		parsed := types.UID(uid)
		if prev, dup := uidOwner[parsed]; dup {
			return fmt.Errorf("%w: %s (%s and %s)", ErrDuplicateCoveredPVCUID, uid, prev, ownerLabel)
		}
		uidOwner[parsed] = ownerLabel
		covered[parsed] = struct{}{}
	}
	return nil
}

// coveredPVCUIDsForSnapshotNode returns the covered PVC UID(s) of a DATA-BEARING snapshot-graph node (the
// caller already applied the dataBearing gate). Preference: (A) the node's bound SnapshotContent status.data
// (milestone B); (B) the owner fallback read DIRECTLY off the node's own captureState — the in-flight VCR
// name / status.snapshotSource.uid, both published by Planned — so coverage is computable at the relaxed
// phase>=Planned wave gate even before the content is bound (this is the case the previous
// "boundSnapshotContentName empty -> contribute nothing" short-circuit missed). A data-bearing node with NO
// observable coverage yet returns ErrSubtreeDataRefsPending (fail-closed: the caller requeues instead of
// under-covering, which would let the orphan wave double-capture a PVC an in-flight child capture already
// targets). A named-but-absent bound content is a hard error. ownerLabel identifies the node for the
// duplicate-UID diagnostic (bound content name when set, else the child key).
func coveredPVCUIDsForSnapshotNode(
	ctx context.Context,
	c client.Reader,
	namespace string,
	node *unstructured.Unstructured,
	nodeKey string,
) (uids []string, ownerLabel string, err error) {
	contentName, _, err := unstructured.NestedString(node.Object, "status", "boundSnapshotContentName")
	if err != nil {
		return nil, "", fmt.Errorf("read Snapshot child %q status.boundSnapshotContentName: %w", nodeKey, err)
	}
	ownerLabel = "Snapshot " + nodeKey
	if contentName != "" {
		ownerLabel = "SnapshotContent " + contentName
		content := &storagev1alpha1.SnapshotContent{}
		if err := c.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, "", fmt.Errorf("bound SnapshotContent %q of Snapshot child %q not found: %w", contentName, nodeKey, err)
			}
			return nil, "", fmt.Errorf("get bound SnapshotContent %q of Snapshot child %q: %w", contentName, nodeKey, err)
		}
		fromData, derr := pvcUIDsFromSnapshotContentDataRefs(content)
		if derr != nil {
			return nil, "", derr
		}
		if len(fromData) > 0 {
			return fromData, ownerLabel, nil
		}
	}
	fromOwner, oerr := coveredPVCUIDsFromOwnerObject(ctx, c, namespace, node)
	if oerr != nil {
		return nil, "", oerr
	}
	if len(fromOwner) > 0 {
		return fromOwner, ownerLabel, nil
	}
	return nil, "", fmt.Errorf("%w: Snapshot child %q (kind %q, no status.data and no in-flight VCR/snapshotSource yet)", ErrSubtreeDataRefsPending, nodeKey, node.GetKind())
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
