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
	"errors"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	vcctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// ErrDuplicateCoveredPVCUID is returned when the same PVC UID is claimed in more than one descendant SnapshotContent.
var ErrDuplicateCoveredPVCUID = errors.New("duplicate covered PVC UID in snapshot subtree")

// ErrSubtreeDataRefsPending is returned when descendant volume coverage is not yet observable (no dataRefs and no pending VCR targets).
var ErrSubtreeDataRefsPending = errors.New("subtree data volume coverage pending")

// DataBearingKindFunc reports whether a snapshot Kind carries a volume data leg. It is backed by the CSD
// spec.requiresDataArtifact (via snapshot.GVKRegistry.RequiresDataArtifact); unmarked/unknown kinds read
// false (manifest-only kinds, built-in pairs). Coverage (Block 5, design §8.5) uses it to decide
// AUTHORITATIVELY — from the CSD, not the shape of the subtree — whether a node contributes a covered PVC
// UID: a kind may carry both children and a data leg, so the old "a node with children has no data"
// (hasChildren) heuristic is gone. It MUST be non-nil in production; passing a permissive
// func(string) bool { return true } is only for unit tests that assert the dataRefs (A) path.
type DataBearingKindFunc func(snapshotKind string) bool

// CollectSubtreeCoveredPVCUIDs returns PVC UIDs already covered by descendant SnapshotContent nodes.
// A descendant contributes its covered PVC UID only when its owning snapshot kind is data-bearing per
// dataBearing (CSD RequiresDataArtifact); the UID is read from status.data, or, in the A->B window before
// status.data is published, from the owner fallback (in-flight VCR / native-CSI snapshotSource.uid). The
// root content itself is not included.
func CollectSubtreeCoveredPVCUIDs(
	ctx context.Context,
	c client.Reader,
	namespace string,
	rootContent *storagev1alpha1.SnapshotContent,
	dataBearing DataBearingKindFunc,
) (map[types.UID]struct{}, error) {
	if rootContent == nil {
		return nil, fmt.Errorf("root SnapshotContent is required")
	}
	if namespace == "" {
		return nil, fmt.Errorf("namespace is required for subtree PVC coverage")
	}
	covered := make(map[types.UID]struct{})
	uidOwner := make(map[types.UID]string)

	visit := func(_ context.Context, content *storagev1alpha1.SnapshotContent) error {
		if content.Name == rootContent.Name {
			return nil
		}
		// Orphan/residual-PVC VolumeSnapshot children are ordinary domain content now (content-single-writer
		// design §11.6): the aggregator projects their status.data from the bound VSC, so they cover their own
		// PVC UID here like every other data-bearing node — there is no visibility-leaf carve-out. Whether a
		// node is data-bearing is decided authoritatively by dataBearing (CSD RequiresDataArtifact), not the
		// shape of the tree (Block 5, design §8.5); the walk still recurses into every child unconditionally.
		uids, err := coveredPVCUIDsForContent(ctx, c, namespace, content, dataBearing)
		if err != nil {
			return err
		}
		for _, uid := range uids {
			if uid == "" {
				continue
			}
			parsed := types.UID(uid)
			if prev, dup := uidOwner[parsed]; dup {
				return fmt.Errorf("%w: %s (SnapshotContent %q and %q)", ErrDuplicateCoveredPVCUID, uid, prev, content.Name)
			}
			uidOwner[parsed] = content.Name
			covered[parsed] = struct{}{}
		}
		return nil
	}

	if err := walkSnapshotContentSubtree(ctx, c, rootContent.Name, visit); err != nil {
		return nil, err
	}
	return covered, nil
}

type snapshotContentVisit func(ctx context.Context, content *storagev1alpha1.SnapshotContent) error

// walkSnapshotContentSubtree mirrors usecase.WalkSnapshotContentSubtree without an import cycle.
func walkSnapshotContentSubtree(
	ctx context.Context,
	c client.Reader,
	rootContentName string,
	visit snapshotContentVisit,
) error {
	visited := make(map[string]struct{})
	return walkSnapshotContentSubtreeRec(ctx, c, rootContentName, visited, visit)
}

func walkSnapshotContentSubtreeRec(
	ctx context.Context,
	c client.Reader,
	contentName string,
	visited map[string]struct{},
	visit snapshotContentVisit,
) error {
	if _, ok := visited[contentName]; ok {
		return fmt.Errorf("SnapshotContent graph cycle at %q", contentName)
	}
	visited[contentName] = struct{}{}

	content := &storagev1alpha1.SnapshotContent{}
	if err := c.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("SnapshotContent %q not found: %w", contentName, err)
		}
		return fmt.Errorf("get SnapshotContent %q: %w", contentName, err)
	}
	if err := visit(ctx, content); err != nil {
		return err
	}
	children := append([]storagev1alpha1.SnapshotContentChildRef(nil), content.Status.ChildrenSnapshotContentRefs...)
	sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })
	for i := range children {
		if children[i].Name == "" {
			continue
		}
		if err := walkSnapshotContentSubtreeRec(ctx, c, children[i].Name, visited, visit); err != nil {
			return err
		}
	}
	return nil
}

func coveredPVCUIDsForContent(
	ctx context.Context,
	c client.Reader,
	namespace string,
	content *storagev1alpha1.SnapshotContent,
	dataBearing DataBearingKindFunc,
) ([]string, error) {
	// Data-bearing decision is AUTHORITATIVE from the CSD (RequiresDataArtifact via dataBearing), keyed by
	// the owning snapshot kind (content.spec.snapshotRef.kind) — NOT the shape of the tree (Block 5, design
	// §8.5). A manifest-only aggregate (RequiresDataArtifact==false) contributes no covered PVC UID; the
	// caller still recurses into its children unconditionally. A kind may legitimately carry BOTH children
	// and a data leg, which the old `if hasChildren { return nil }` heuristic wrongly excluded.
	kind := ""
	if content.Spec.SnapshotRef != nil {
		kind = content.Spec.SnapshotRef.Kind
	}
	if dataBearing == nil || !dataBearing(kind) {
		return nil, nil
	}
	// A (milestone B of the write model): status.data published — the authoritative covered UID.
	fromDataRefs, err := pvcUIDsFromSnapshotContentDataRefs(content)
	if err != nil {
		return nil, err
	}
	if len(fromDataRefs) > 0 {
		return fromDataRefs, nil
	}
	// A->B window: status.data is not published yet. Fall back via the OWNING snapshot resolved from
	// content.spec.snapshotRef (design §8.5/§11.7), NOT a content-UID-derived VCR (a domain data-leaf's VCR
	// is snapshot-owned and its real name is only published on the owner's captureState).
	ref := content.Spec.SnapshotRef
	if ref != nil && ref.Name != "" {
		ownerNS := ref.Namespace
		if ownerNS == "" {
			ownerNS = namespace
		}
		owner := &unstructured.Unstructured{}
		owner.SetGroupVersionKind(schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind))
		if err := c.Get(ctx, client.ObjectKey{Namespace: ownerNS, Name: ref.Name}, owner); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("get owning snapshot %s/%s of SnapshotContent %q: %w", ownerNS, ref.Name, content.Name, err)
			}
			// NotFound owner: fall through to the pending signal below (the wave re-evaluates).
		} else if fromOwner, err := coveredPVCUIDsFromOwnerObject(ctx, c, ownerNS, owner); err != nil {
			return nil, err
		} else if len(fromOwner) > 0 {
			return fromOwner, nil
		}
	}
	// Data-bearing node with NO observable coverage yet (no status.data and no in-flight VCR/snapshotSource):
	// fail closed so the caller requeues instead of under-covering — silently dropping this UID would let the
	// orphan wave double-capture a PVC an in-flight child capture already targets (INV coverage completeness).
	return nil, fmt.Errorf("%w: SnapshotContent %q (kind %q, no status.data and owner has no in-flight VCR/snapshotSource yet)", ErrSubtreeDataRefsPending, content.Name, kind)
}

// coveredPVCUIDsFromOwnerObject reads the covered PVC UID(s) DIRECTLY from an owning xxxSnapshot object in
// hand (design §8.5/§11.7): the in-flight VCR name on status.captureState.domainSpecificController.
// volumeCaptureRequestName → that VCR's spec.targets[].uid (VCR-based domains), or status.sourceRef.uid
// (native-CSI VolumeSnapshot, no VCR). Both are published by capture barrier 1 (Planned), so coverage is
// computable at the relaxed phase>=Planned wave gate even before the node's SnapshotContent is bound.
// Returns nil (no error) when neither signal is present yet; the caller decides pending vs benign.
func coveredPVCUIDsFromOwnerObject(
	ctx context.Context,
	c client.Reader,
	namespace string,
	owner *unstructured.Unstructured,
) ([]string, error) {
	ownerNS := owner.GetNamespace()
	if ownerNS == "" {
		ownerNS = namespace
	}
	if vcrName, _, _ := unstructured.NestedString(owner.Object, "status", "captureState", "domainSpecificController", "volumeCaptureRequestName"); vcrName != "" {
		return pvcUIDsFromNamedVCR(ctx, c, ownerNS, vcrName)
	}
	if owner.GetAPIVersion() == snapshotpkg.CSISnapshotAPIVersion && owner.GetKind() == snapshotpkg.KindVolumeSnapshot {
		if uid, _, _ := unstructured.NestedString(owner.Object, "status", "sourceRef", "uid"); uid != "" {
			return []string{uid}, nil
		}
	}
	return nil, nil
}

func pvcUIDsFromSnapshotContentDataRefs(content *storagev1alpha1.SnapshotContent) ([]string, error) {
	refs := content.DataList()
	if len(refs) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(refs))
	for i := range refs {
		b := refs[i]
		uid := string(b.SourceRef.UID)
		if uid == "" {
			return nil, fmt.Errorf("SnapshotContent %q data: empty source uid", content.Name)
		}
		out = append(out, uid)
	}
	return out, nil
}

func pvcUIDsFromPendingVCR(ctx context.Context, c client.Reader, namespace string, contentUID types.UID) ([]string, error) {
	if contentUID == "" {
		return nil, nil
	}
	return pvcUIDsFromNamedVCR(ctx, c, namespace, vcpkg.SnapshotContentVCRName(contentUID))
}

// pvcUIDsFromNamedVCR reads the covered PVC UIDs (spec.targets[].uid) from a VolumeCaptureRequest addressed
// by an explicit name in namespace. Used by the coverage owner-fallback for a domain data-leaf whose VCR is
// snapshot-owned (its name comes from the owner's captureState.volumeCaptureRequestName, not the content
// UID). A missing VCR contributes nothing (the wave re-evaluates); a parse/read error is a hard error.
func pvcUIDsFromNamedVCR(ctx context.Context, c client.Reader, namespace, name string) ([]string, error) {
	if name == "" {
		return nil, nil
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get VolumeCaptureRequest %s/%s: %w", namespace, name, err)
	}
	targets, err := vcctrl.ParseVolumeCaptureTargets(obj)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.UID != "" {
			out = append(out, t.UID)
		}
	}
	return out, nil
}
