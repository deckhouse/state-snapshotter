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
}

func NewResolver(client client.Client) *Resolver {
	return &Resolver{client: client}
}

func (r *Resolver) ResolveSnapshotTree(ctx context.Context, snapshotNamespace, snapshotName string) (*SnapshotContentNode, error) {
	snapshotGVK := schema.GroupVersionKind{
		Group:   "state-snapshotter.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "Snapshot",
	}
	snapshotObj := &unstructured.Unstructured{}
	snapshotObj.SetGroupVersionKind(snapshotGVK)
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: snapshotNamespace, Name: snapshotName}, snapshotObj); err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: snapshot %s/%s", ErrNotFound, snapshotNamespace, snapshotName)
		}
		return nil, fmt.Errorf("failed to get snapshot %s/%s: %w", snapshotNamespace, snapshotName, err)
	}
	if err := ensureSnapshotReady(snapshotObj); err != nil {
		return nil, err
	}

	snapshotLike, err := snapshot.ExtractSnapshotLike(snapshotObj)
	if err != nil {
		return nil, err
	}

	contentGVK := schema.GroupVersionKind{
		Group:   snapshotGVK.Group,
		Version: snapshotGVK.Version,
		Kind:    snapshotGVK.Kind + "Content",
	}

	boundName := snapshotLike.GetStatusContentName()
	if boundName != "" {
		rootContent := &unstructured.Unstructured{}
		rootContent.SetGroupVersionKind(contentGVK)
		if err := r.client.Get(ctx, client.ObjectKey{Name: boundName}, rootContent); err != nil {
			if errors.IsNotFound(err) {
				return nil, fmt.Errorf("%w: bound SnapshotContent %s not found", ErrContractViolation, boundName)
			}
			return nil, fmt.Errorf("failed to get bound SnapshotContent %s: %w", boundName, err)
		}
		if err := ensureReady(rootContent); err != nil {
			return nil, err
		}
		return r.buildTree(ctx, contentGVK, rootContent)
	}

	return nil, fmt.Errorf("%w: snapshot %s/%s has empty boundSnapshotContentName", ErrNotReady, snapshotNamespace, snapshotName)
}

func (r *Resolver) buildTree(ctx context.Context, contentGVK schema.GroupVersionKind, root *unstructured.Unstructured) (*SnapshotContentNode, error) {
	contentLike, err := snapshot.ExtractSnapshotContentLike(root)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to parse SnapshotContent", ErrContractViolation)
	}
	if contentLike.GetStatusManifestCheckpointName() == "" {
		return nil, fmt.Errorf("%w: manifestCheckpointName is empty for SnapshotContent %s", ErrContractViolation, root.GetName())
	}

	node := &SnapshotContentNode{
		Content:                root,
		ManifestCheckpointName: contentLike.GetStatusManifestCheckpointName(),
		DataBindings:           cloneDataBindings(contentLike.GetStatusDataRefs()),
	}

	children := contentLike.GetStatusChildrenSnapshotContentRefs()
	sort.Slice(children, func(i, j int) bool {
		if children[i].Kind == children[j].Kind {
			return children[i].Name < children[j].Name
		}
		return children[i].Kind < children[j].Kind
	})
	for _, child := range children {
		gvk := contentGVK
		if child.Kind != "" && child.Kind != contentGVK.Kind {
			return nil, fmt.Errorf("%w: child SnapshotContent kind mismatch: %s", ErrContractViolation, child.Kind)
		}
		childObj := &unstructured.Unstructured{}
		childObj.SetGroupVersionKind(gvk)
		if err := r.client.Get(ctx, client.ObjectKey{Name: child.Name}, childObj); err != nil {
			return nil, fmt.Errorf("%w: child SnapshotContent %s not found", ErrContractViolation, child.Name)
		}
		if err := ensureReady(childObj); err != nil {
			return nil, err
		}
		childNode, err := r.buildTree(ctx, contentGVK, childObj)
		if err != nil {
			return nil, err
		}
		node.Children = append(node.Children, childNode)
	}
	return node, nil
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
// by its GVK (the namespaced root Snapshot, or a domain snapshot CR such as DemoVirtualMachineSnapshot
// / DemoVirtualDiskSnapshot). It compiles that node and everything below it, so the restore endpoint
// can return apply-ready manifests for a single subtree, not only the whole namespace.
func (r *Resolver) ResolveRestoreSubtree(ctx context.Context, gvk schema.GroupVersionKind, snapshotNamespace, snapshotName string) (*RestoreNode, error) {
	rootObj := &unstructured.Unstructured{}
	rootObj.SetGroupVersionKind(gvk)
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: snapshotNamespace, Name: snapshotName}, rootObj); err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: snapshot %s/%s", ErrNotFound, snapshotNamespace, snapshotName)
		}
		return nil, fmt.Errorf("failed to get snapshot %s/%s: %w", snapshotNamespace, snapshotName, err)
	}
	return r.buildRestoreNode(ctx, rootObj, snapshotNamespace, map[string]struct{}{})
}

func (r *Resolver) buildRestoreNode(ctx context.Context, snapshotObj *unstructured.Unstructured, namespace string, visited map[string]struct{}) (*RestoreNode, error) {
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
		if isVolumeSnapshotLeaf(ref) {
			vsc, err := r.resolveVolumeSnapshotLeaf(ctx, namespace, ref.Name)
			if err != nil {
				return nil, err
			}
			// A VolumeSnapshotContent must be bound to exactly one VolumeSnapshot leaf; 0 or >1 is a
			// contract violation (ADR 2026-06-10).
			if existing, ok := node.VSCToVS[vsc]; ok && existing != ref.Name {
				return nil, fmt.Errorf(
					"%w: VolumeSnapshotContent %q is referenced by multiple VolumeSnapshot leaves (%s, %s)",
					ErrContractViolation, vsc, existing, ref.Name,
				)
			}
			node.VSCToVS[vsc] = ref.Name
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
		childNode, err := r.buildRestoreNode(ctx, childObj, namespace, visited)
		if err != nil {
			return nil, err
		}
		node.Children = append(node.Children, childNode)
	}
	return node, nil
}

// resolveVolumeSnapshotLeaf reads the durable VolumeSnapshotContent bound to an orphan-PVC
// VolumeSnapshot visibility leaf. The dataRefs artifact references that VSC, so the returned name is
// the key used to map a captured PVC to its restore VolumeSnapshot.
//
// It fails closed: the endpoint emits an apply-ready PVC with dataSourceRef to this VolumeSnapshot,
// so a leaf that is deleting, not readyToUse, or not yet bound must not yield a manifest.
func (r *Resolver) resolveVolumeSnapshotLeaf(ctx context.Context, namespace, vsName string) (string, error) {
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(schema.GroupVersionKind{Group: snapshot.CSISnapshotGroup, Version: snapshot.CSISnapshotVersion, Kind: snapshot.KindVolumeSnapshot})
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: vsName}, vs); err != nil {
		if errors.IsNotFound(err) {
			return "", fmt.Errorf("%w: VolumeSnapshot leaf %s/%s not found", ErrContractViolation, namespace, vsName)
		}
		return "", fmt.Errorf("failed to get VolumeSnapshot %s/%s: %w", namespace, vsName, err)
	}
	if vs.GetDeletionTimestamp() != nil {
		return "", fmt.Errorf("%w: VolumeSnapshot leaf %s/%s is being deleted", ErrContractViolation, namespace, vsName)
	}
	readyToUse, _, rerr := unstructured.NestedBool(vs.Object, "status", "readyToUse")
	if rerr != nil {
		return "", fmt.Errorf("%w: VolumeSnapshot %s/%s readyToUse unreadable", ErrContractViolation, namespace, vsName)
	}
	if !readyToUse {
		return "", fmt.Errorf("%w: VolumeSnapshot leaf %s/%s is not readyToUse", ErrNotReady, namespace, vsName)
	}
	boundVSC, _, err := unstructured.NestedString(vs.Object, "status", "boundVolumeSnapshotContentName")
	if err != nil {
		return "", fmt.Errorf("%w: VolumeSnapshot %s/%s boundVolumeSnapshotContentName unreadable", ErrContractViolation, namespace, vsName)
	}
	if boundVSC == "" {
		return "", fmt.Errorf("%w: VolumeSnapshot leaf %s/%s has empty boundVolumeSnapshotContentName", ErrNotReady, namespace, vsName)
	}
	return boundVSC, nil
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
