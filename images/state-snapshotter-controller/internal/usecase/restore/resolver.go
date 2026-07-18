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

package restore

import (
	"context"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

type Resolver struct {
	client client.Client
	// isDomainKind reports whether a snapshot kind is owned by an out-of-process domain controller.
	// When it returns true for a node, the resolver marks that node as a domain boundary and does NOT
	// descend into it (the domain apiserver resolves the subtree). Nil means "no domain kinds": every
	// node is resolved generically (the default for focused tests and the per-resource view).
	isDomainKind func(kind string) bool
}

func NewResolver(client client.Client) *Resolver {
	return &Resolver{client: client}
}

// ResolveRestoreTree walks the Snapshot run tree (Snapshot -> status.childrenSnapshotRefs) starting
// at the root namespaced Snapshot and returns a RestoreNode tree carrying snapshot-CR identity, MCP
// names, dataRefs, and resolved orphan-PVC VolumeSnapshot leaves. It fails closed: any missing or
// not-ready node, or an unresolvable VolumeSnapshot leaf, aborts the whole resolution.
func (r *Resolver) ResolveRestoreTree(ctx context.Context, snapshotNamespace, snapshotName string) (*RestoreNode, error) {
	rootGVK := storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot")
	return r.ResolveRestoreSubtree(ctx, rootGVK, snapshotNamespace, snapshotName)
}

// ResolveRestoreSubtree resolves the restore tree starting from an arbitrary snapshot node identified
// by its GVK (the namespaced root Snapshot, or any domain snapshot CR, e.g. a per-VM or per-disk
// snapshot). It compiles that node and everything below it, so the restore endpoint can return
// apply-ready manifests for a single subtree, not only the whole namespace.
func (r *Resolver) ResolveRestoreSubtree(ctx context.Context, gvk schema.GroupVersionKind, snapshotNamespace, snapshotName string) (*RestoreNode, error) {
	// Domain boundary at the root: a kind owned by an out-of-process domain controller is restored by
	// the domain's aggregated apiserver, not here. Short-circuit BEFORE the Get so core never reads the
	// domain CR (it keeps core domain-free: no demo RBAC, no extra round-trip). compileNode delegates it.
	if r.isDomainKind != nil && r.isDomainKind(gvk.Kind) {
		return domainRestoreNode(gvk.GroupVersion().String(), gvk.Kind, snapshotName, snapshotNamespace), nil
	}
	rootObj := &unstructured.Unstructured{}
	rootObj.SetGroupVersionKind(gvk)
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: snapshotNamespace, Name: snapshotName}, rootObj); err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: snapshot %s/%s", ErrNotFound, snapshotNamespace, snapshotName)
		}
		return nil, fmt.Errorf("failed to get snapshot %s/%s: %w", snapshotNamespace, snapshotName, err)
	}
	return r.buildRestoreNode(ctx, rootObj, snapshotNamespace, map[string]struct{}{}, true)
}

// domainRestoreNode builds the marker node for a domain snapshot boundary. It carries only the
// identity (GVK/namespace/name) compileDomainNode needs to address the delegated restore; the domain
// apiserver resolves the subtree, fetches its base from core, and enforces readiness.
func domainRestoreNode(apiVersion, kind, name, namespace string) *RestoreNode {
	return &RestoreNode{
		SnapshotRef: snapshot.ObjectRef{
			APIVersion: apiVersion,
			Kind:       kind,
			Name:       name,
			Namespace:  namespace,
		},
		Domain: true,
	}
}

// buildRestoreNode resolves one snapshot node and its subtree. isRoot marks the user-addressed entry node
// (vs a node reached via the trusted childrenSnapshotRefs tree-walk), which controls the back-reference
// mismatch status: 403 for the root, 409 for tree-walk children (see the anti-spoofing check below).
func (r *Resolver) buildRestoreNode(ctx context.Context, snapshotObj *unstructured.Unstructured, namespace string, visited map[string]struct{}, isRoot bool) (*RestoreNode, error) {
	key := snapshotObj.GetAPIVersion() + "/" + snapshotObj.GetKind() + "/" + snapshotObj.GetName()
	if _, ok := visited[key]; ok {
		return nil, fmt.Errorf("%w: snapshot run-tree cycle at %s", ErrContractViolation, key)
	}
	visited[key] = struct{}{}

	if err := ensureSnapshotReady(snapshotObj); err != nil {
		return nil, err
	}
	snapshotLike, err := snapshot.ExtractSnapshotLike(snapshotObj)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to parse Snapshot %s", ErrContractViolation, snapshotObj.GetName())
	}

	boundName := snapshotLike.GetStatusContentName()
	if boundName == "" {
		return nil, fmt.Errorf("%w: snapshot %s has empty boundSnapshotContentName", ErrNotReady, snapshotObj.GetName())
	}
	contentGVK := storagev1alpha1.SchemeGroupVersion.WithKind("SnapshotContent")
	content := &unstructured.Unstructured{}
	content.SetGroupVersionKind(contentGVK)
	if err := r.client.Get(ctx, client.ObjectKey{Name: boundName}, content); err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: bound SnapshotContent %s not found", ErrContractViolation, boundName)
		}
		return nil, fmt.Errorf("failed to get bound SnapshotContent %s: %w", boundName, err)
	}
	if err := ensureReady(content); err != nil {
		return nil, err
	}
	// Anti-spoofing handshake (unified facade contract): status.boundSnapshotContentName is written on the
	// snapshot side and is not trustworthy alone — a status writer could aim it at a foreign content. The
	// bound content must carry a spec.snapshotRef pointing back at this very snapshot subject
	// (apiVersion/kind/namespace/name; uid only when both sides carry one). For the user-addressed ROOT node
	// a mismatch is a 403 (Forbidden), mirroring the domain-side facade; for a CHILD node reached via the
	// trusted childrenSnapshotRefs tree-walk a mismatch is a data-integrity contract violation (409),
	// consistent with the sibling orphan-VS child leg (verifyOrphanContentSnapshotRef). Either way, fail-closed.
	if reason := snapshotRefMismatch(content, snapshotObj.GetAPIVersion(), snapshotObj.GetKind(), namespace, snapshotObj.GetName(), string(snapshotObj.GetUID())); reason != "" {
		if isRoot {
			return nil, errors.NewForbidden(
				schema.GroupResource{Group: contentGVK.Group, Resource: "snapshotcontents"}, boundName,
				fmt.Errorf("bound SnapshotContent %s does not belong to snapshot %s/%s: %s", boundName, namespace, snapshotObj.GetName(), reason),
			)
		}
		return nil, fmt.Errorf("%w: bound SnapshotContent %s does not belong to child snapshot %s/%s: %s", ErrContractViolation, boundName, namespace, snapshotObj.GetName(), reason)
	}
	contentLike, err := snapshot.ExtractSnapshotContentLike(content)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to parse SnapshotContent %s", ErrContractViolation, boundName)
	}
	mcpName := contentLike.GetStatusManifestCheckpointName()
	if mcpName == "" {
		return nil, fmt.Errorf("%w: manifestCheckpointName is empty for SnapshotContent %s", ErrContractViolation, boundName)
	}

	node := &RestoreNode{
		SnapshotRef: snapshot.ObjectRef{
			APIVersion: snapshotObj.GetAPIVersion(),
			Kind:       snapshotObj.GetKind(),
			Name:       snapshotObj.GetName(),
			Namespace:  namespace,
		},
		ManifestCheckpointName: mcpName,
		DataBindings:           cloneDataBindings(contentLike.GetStatusDataRefs()),
		VSCToVS:                map[string]string{},
	}

	// Read childrenSnapshotRefs directly: the unstructured SnapshotLike wrapper does not surface
	// apiVersion, which we need to tell VolumeSnapshot leaves from child snapshot CRs and to Get the
	// child with its own GVK.
	children := childSnapshotRefs(snapshotObj)
	sort.Slice(children, func(i, j int) bool {
		if children[i].Kind == children[j].Kind {
			return children[i].Name < children[j].Name
		}
		return children[i].Kind < children[j].Kind
	})
	for _, ref := range children {
		if ref.Name == "" {
			continue
		}
		// Domain boundary: a child owned by an out-of-process domain controller is delegated. Mark it
		// and do NOT Get it — core stays domain-free (no demo RBAC, no extra round-trip) and does not
		// descend; the domain apiserver resolves and restores the whole subtree. compileNode delegates.
		if r.isDomainKind != nil && r.isDomainKind(ref.Kind) {
			node.Children = append(node.Children, domainRestoreNode(ref.APIVersion, ref.Kind, ref.Name, namespace))
			continue
		}
		if isVolumeSnapshotLeaf(ref) {
			// Variant A (INV-ORPHAN4): an orphan-PVC CSI VolumeSnapshot is a namespaced handle to a
			// standalone child volume node. Its PVC manifest + dataRef live on that child SnapshotContent
			// (own MCP), not on this node, so resolve it into a child RestoreNode the generic per-node
			// compile path emits (PVC bound to its VolumeSnapshot dataSourceRef).
			childNode, err := r.resolveOrphanVolumeChildNode(ctx, namespace, ref.Name)
			if err != nil {
				return nil, err
			}
			node.Children = append(node.Children, childNode)
			continue
		}
		childObj := &unstructured.Unstructured{}
		childObj.SetGroupVersionKind(schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind))
		if err := r.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, childObj); err != nil {
			if errors.IsNotFound(err) {
				return nil, fmt.Errorf("%w: child snapshot %s/%s (%s) not found", ErrContractViolation, namespace, ref.Name, ref.Kind)
			}
			return nil, fmt.Errorf("failed to get child snapshot %s/%s: %w", namespace, ref.Name, err)
		}
		childNode, err := r.buildRestoreNode(ctx, childObj, namespace, visited, false)
		if err != nil {
			return nil, err
		}
		node.Children = append(node.Children, childNode)
	}
	return node, nil
}

// ResolveVolumeSnapshotRestoreNode resolves a generic-PVC extended VolumeSnapshot leaf — the entrypoint
// of the subresources.snapshot.storage.k8s.io connector (C8) — into the standalone child volume
// RestoreNode behind it (PVC manifest + dataRef + VSC->VS mapping). It reuses the orphan-PVC volume-leaf
// resolution: the VolumeSnapshot is itself the leaf and carries no snapshot children, so there is no
// recursion. The compile path then emits the PVC bound to its VolumeSnapshot dataSourceRef.
//
// Unlike resolveOrphanVolumeChildNode (reached from a trusted childrenSnapshotRefs tree-walk, where a
// missing leaf is a contract violation), this is a top-level user-addressed entrypoint: a missing
// VolumeSnapshot is a plain NotFound (404), matching the manifests-download sibling. The existence
// precheck maps that case to ErrNotFound before delegating to the shared (409-on-missing) resolver.
func (r *Resolver) ResolveVolumeSnapshotRestoreNode(ctx context.Context, namespace, vsName string) (*RestoreNode, error) {
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(schema.GroupVersionKind{Group: snapshot.CSISnapshotGroup, Version: snapshot.CSISnapshotVersion, Kind: snapshot.KindVolumeSnapshot})
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: vsName}, vs); err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: VolumeSnapshot %s/%s", ErrNotFound, namespace, vsName)
		}
		return nil, fmt.Errorf("failed to get VolumeSnapshot %s/%s: %w", namespace, vsName, err)
	}
	return r.resolveOrphanVolumeChildNode(ctx, namespace, vsName)
}

// resolveOrphanVolumeChildNode builds the child RestoreNode behind an orphan-PVC CSI VolumeSnapshot
// leaf (Variant A INV-ORPHAN4). The VS is a namespaced handle whose status.boundSnapshotContentName
// points at a standalone child SnapshotContent that owns the orphan PVC's manifest (its own
// ManifestCheckpoint) and the single dataRef to the durable VolumeSnapshotContent. The returned node
// carries that MCP + dataRef plus the VSC->VS mapping, so the generic per-node compile path emits the
// PVC bound to its VolumeSnapshot dataSourceRef — the orphan PVC is no longer carried on the parent.
func (r *Resolver) resolveOrphanVolumeChildNode(ctx context.Context, namespace, vsName string) (*RestoreNode, error) {
	boundVSC, childContentName, vsUID, err := r.resolveVolumeSnapshotLeaf(ctx, namespace, vsName)
	if err != nil {
		return nil, err
	}
	if childContentName == "" {
		return nil, fmt.Errorf("%w: orphan VolumeSnapshot %s/%s has empty boundSnapshotContentName (child volume node handle not yet published)", ErrNotReady, namespace, vsName)
	}

	contentGVK := storagev1alpha1.SchemeGroupVersion.WithKind("SnapshotContent")
	childContent := &unstructured.Unstructured{}
	childContent.SetGroupVersionKind(contentGVK)
	if err := r.client.Get(ctx, client.ObjectKey{Name: childContentName}, childContent); err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: orphan child SnapshotContent %s not found", ErrContractViolation, childContentName)
		}
		return nil, fmt.Errorf("failed to get orphan child SnapshotContent %s: %w", childContentName, err)
	}
	if err := ensureReady(childContent); err != nil {
		return nil, err
	}
	// Anti-spoofing handshake: the bound child content must point its spec.snapshotRef back at this very
	// VolumeSnapshot handle. status.boundSnapshotContentName alone is not trustworthy (a status writer
	// could aim it at a foreign content); requiring the reverse reference closes that gap, mirroring the
	// CSI VolumeSnapshot<->VolumeSnapshotContent back-binding handshake.
	if err := verifyOrphanContentSnapshotRef(childContent, namespace, vsName, vsUID, childContentName); err != nil {
		return nil, err
	}
	contentLike, err := snapshot.ExtractSnapshotContentLike(childContent)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to parse orphan child SnapshotContent %s", ErrContractViolation, childContentName)
	}
	mcpName := contentLike.GetStatusManifestCheckpointName()
	if mcpName == "" {
		return nil, fmt.Errorf("%w: manifestCheckpointName is empty for orphan child SnapshotContent %s", ErrContractViolation, childContentName)
	}

	return &RestoreNode{
		SnapshotRef: snapshot.ObjectRef{
			APIVersion: snapshot.CSISnapshotAPIVersion,
			Kind:       snapshot.KindVolumeSnapshot,
			Name:       vsName,
			Namespace:  namespace,
		},
		ManifestCheckpointName: mcpName,
		DataBindings:           cloneDataBindings(contentLike.GetStatusDataRefs()),
		VSCToVS:                map[string]string{boundVSC: vsName},
	}, nil
}

// resolveVolumeSnapshotLeaf reads the durable VolumeSnapshotContent and the child-volume-node
// SnapshotContent bound to an orphan-PVC VolumeSnapshot handle. The dataRefs artifact references the
// VSC (returned first); status.boundSnapshotContentName references the child content (returned second,
// may be "" on a not-yet-published handle, which the caller treats as not-ready).
//
// It fails closed: the endpoint emits an apply-ready PVC with dataSourceRef to this VolumeSnapshot,
// so a leaf that is deleting, not readyToUse, or not yet bound must not yield a manifest.
func (r *Resolver) resolveVolumeSnapshotLeaf(ctx context.Context, namespace, vsName string) (boundVSC string, boundContentName string, vsUID string, err error) {
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(schema.GroupVersionKind{Group: snapshot.CSISnapshotGroup, Version: snapshot.CSISnapshotVersion, Kind: snapshot.KindVolumeSnapshot})
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: vsName}, vs); err != nil {
		if errors.IsNotFound(err) {
			return "", "", "", fmt.Errorf("%w: VolumeSnapshot leaf %s/%s not found", ErrContractViolation, namespace, vsName)
		}
		return "", "", "", fmt.Errorf("failed to get VolumeSnapshot %s/%s: %w", namespace, vsName, err)
	}
	if vs.GetDeletionTimestamp() != nil {
		return "", "", "", fmt.Errorf("%w: VolumeSnapshot leaf %s/%s is being deleted", ErrContractViolation, namespace, vsName)
	}
	readyToUse, _, rerr := unstructured.NestedBool(vs.Object, "status", "readyToUse")
	if rerr != nil {
		return "", "", "", fmt.Errorf("%w: VolumeSnapshot %s/%s readyToUse unreadable", ErrContractViolation, namespace, vsName)
	}
	if !readyToUse {
		return "", "", "", fmt.Errorf("%w: VolumeSnapshot leaf %s/%s is not readyToUse", ErrNotReady, namespace, vsName)
	}
	boundVSC, _, err = unstructured.NestedString(vs.Object, "status", "boundVolumeSnapshotContentName")
	if err != nil {
		return "", "", "", fmt.Errorf("%w: VolumeSnapshot %s/%s boundVolumeSnapshotContentName unreadable", ErrContractViolation, namespace, vsName)
	}
	if boundVSC == "" {
		return "", "", "", fmt.Errorf("%w: VolumeSnapshot leaf %s/%s has empty boundVolumeSnapshotContentName", ErrNotReady, namespace, vsName)
	}
	boundContentName, _, err = unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName")
	if err != nil {
		return "", "", "", fmt.Errorf("%w: VolumeSnapshot %s/%s boundSnapshotContentName unreadable", ErrContractViolation, namespace, vsName)
	}
	return boundVSC, boundContentName, string(vs.GetUID()), nil
}

// snapshotRefMismatch is the shared anti-spoofing primitive: it compares a SnapshotContent's
// spec.snapshotRef back-reference against the expected snapshot subject identity
// (apiVersion/kind/namespace/name; uid only when both the ref and the live subject carry one). It returns
// a human-readable reason describing the first mismatch, or "" when the back-reference is present and
// points at the expected subject. Callers wrap the reason in their own status (409 contract violation for
// the orphan-VS tree-walk leg, 403 Forbidden for user-addressed snapshot-node resolution).
func snapshotRefMismatch(content *unstructured.Unstructured, wantAPIVersion, wantKind, wantNamespace, wantName, wantUID string) string {
	ref, found, err := unstructured.NestedMap(content.Object, "spec", "snapshotRef")
	if err != nil || !found || len(ref) == 0 {
		return "missing spec.snapshotRef back-reference"
	}
	apiVersion, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "apiVersion")
	kind, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "kind")
	ns, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "namespace")
	name, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "name")
	uid, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "uid")
	if apiVersion != wantAPIVersion || kind != wantKind || ns != wantNamespace || name != wantName {
		return fmt.Sprintf("spec.snapshotRef (apiVersion=%q kind=%q %s/%s) does not point back at %s/%s (apiVersion=%q kind=%q)", apiVersion, kind, ns, name, wantNamespace, wantName, wantAPIVersion, wantKind)
	}
	if uid != "" && wantUID != "" && uid != wantUID {
		return fmt.Sprintf("spec.snapshotRef.uid %q does not match subject %s/%s uid %q", uid, wantNamespace, wantName, wantUID)
	}
	return ""
}

// verifyOrphanContentSnapshotRef enforces the anti-spoofing handshake for the orphan child volume node:
// the bound child SnapshotContent must carry a spec.snapshotRef that points back at the VolumeSnapshot
// handle (apiVersion/kind/namespace/name). The UID is matched only when the ref carries one — both the
// import path (extended VolumeSnapshot) and the capture path (wave4B) now stamp it with the live VS UID,
// and recycle-bin restore re-points it (relaxed-CEL) to the re-created VS handle's new UID; we verify it
// against the live VS whenever present. Any missing/mismatched field is a contract violation (409), never
// a transient not-ready.
func verifyOrphanContentSnapshotRef(content *unstructured.Unstructured, namespace, vsName, vsUID, childContentName string) error {
	if reason := snapshotRefMismatch(content, snapshot.CSISnapshotAPIVersion, snapshot.KindVolumeSnapshot, namespace, vsName, vsUID); reason != "" {
		return fmt.Errorf("%w: orphan child SnapshotContent %s %s (expected VolumeSnapshot %s/%s)", ErrContractViolation, childContentName, reason, namespace, vsName)
	}
	return nil
}

func isVolumeSnapshotLeaf(ref snapshot.ObjectRef) bool {
	return ref.APIVersion == snapshot.CSISnapshotAPIVersion && ref.Kind == snapshot.KindVolumeSnapshot
}

// childSnapshotRefs reads status.childrenSnapshotRefs[] (apiVersion/kind/name) from a snapshot object.
func childSnapshotRefs(obj *unstructured.Unstructured) []snapshot.ObjectRef {
	refsRaw, _, err := unstructured.NestedSlice(obj.Object, "status", "childrenSnapshotRefs")
	if err != nil {
		return nil
	}
	out := make([]snapshot.ObjectRef, 0, len(refsRaw))
	for _, r := range refsRaw {
		m, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		ref := snapshot.ObjectRef{}
		ref.APIVersion, _ = m["apiVersion"].(string)
		ref.Kind, _ = m["kind"].(string)
		ref.Name, _ = m["name"].(string)
		out = append(out, ref)
	}
	return out
}

func ensureReady(obj *unstructured.Unstructured) error {
	contentLike, err := snapshot.ExtractSnapshotContentLike(obj)
	if err != nil {
		return fmt.Errorf("%w: failed to parse SnapshotContent", ErrContractViolation)
	}
	conditions := contentLike.GetStatusConditions()
	ready := meta.FindStatusCondition(conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionTrue {
		return fmt.Errorf("%w: SnapshotContent %s is not Ready", ErrNotReady, obj.GetName())
	}
	return nil
}

func ensureSnapshotReady(snapshotObj *unstructured.Unstructured) error {
	snapshotLike, err := snapshot.ExtractSnapshotLike(snapshotObj)
	if err != nil {
		return fmt.Errorf("%w: failed to parse Snapshot", ErrContractViolation)
	}
	conditions := snapshotLike.GetStatusConditions()
	ready := meta.FindStatusCondition(conditions, "Ready")
	// A snapshot used in the restore tree must be explicitly Ready=True. A missing Ready condition
	// (e.g. mid-reconcile) is treated as not ready: the restore compiler must never compile from an
	// unfinished snapshot node.
	if ready == nil || ready.Status != metav1.ConditionTrue {
		return fmt.Errorf("%w: Snapshot %s is not Ready", ErrNotReady, snapshotObj.GetName())
	}
	return nil
}
