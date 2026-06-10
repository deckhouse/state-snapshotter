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

package api //nolint:revive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

var vscGVK = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"}

// buildSnapshotTreeImpl assembles the full SnapshotContent tree and creates the root Snapshot
// idempotently. The caller (HandleImportBuild) already validated the request.
//
// Steps (leaves-first, root last):
//  1. For each node: create generic SnapshotContent, publish MCP name + dataRefs + children refs.
//  2. Patch each referenced VSC to deletionPolicy=Retain (prevents deletion when VS is removed).
//  3. Set child SnapshotContent ownerRef → parent SnapshotContent.
//  4. Create root Snapshot with spec.existingContentRef → root SnapshotContent.
//     ObjectKeeper is created by the Snapshot reconciler on first reconcile.
func buildSnapshotTreeImpl(ctx context.Context, c client.Client, req ImportBuildRequest) (*ImportBuildResult, error) {
	if len(req.Nodes) == 0 {
		return nil, fmt.Errorf("import-build: nodes list must not be empty")
	}
	if req.RootNodeID == "" {
		return nil, fmt.Errorf("import-build: rootNodeId must not be empty")
	}

	// Index nodes by nodeId for quick lookup.
	nodeByID := make(map[string]*ImportBuildNode, len(req.Nodes))
	for i := range req.Nodes {
		n := &req.Nodes[i]
		if n.NodeID == "" {
			return nil, fmt.Errorf("import-build: node at index %d has empty nodeId", i)
		}
		nodeByID[n.NodeID] = n
	}
	if _, ok := nodeByID[req.RootNodeID]; !ok {
		return nil, fmt.Errorf("import-build: rootNodeId %q not found in nodes list", req.RootNodeID)
	}

	// Determine a topological order (leaves first, root last).
	order, err := topoSort(req.RootNodeID, nodeByID)
	if err != nil {
		return nil, fmt.Errorf("import-build: %w", err)
	}

	// contentNameByNodeID maps nodeId → SnapshotContent.Name (deterministic).
	contentNameByNodeID := make(map[string]string, len(req.Nodes))
	for _, nodeID := range order {
		contentNameByNodeID[nodeID] = importContentName(req.Namespace, req.SnapshotName, nodeID)
	}

	// Patch all referenced VSCs to deletionPolicy=Retain first, so they survive VS deletion.
	for i := range req.Nodes {
		for _, dr := range req.Nodes[i].DataRefs {
			if dr.VolumeSnapshotContentName == "" {
				continue
			}
			if err := ensureVSCRetainPolicy(ctx, c, dr.VolumeSnapshotContentName); err != nil {
				return nil, fmt.Errorf("import-build: patch VSC %s to Retain: %w", dr.VolumeSnapshotContentName, err)
			}
		}
	}

	// Create / verify each SnapshotContent (leaves first).
	for _, nodeID := range order {
		node := nodeByID[nodeID]
		contentName := contentNameByNodeID[nodeID]

		if err := ensureImportSnapshotContent(ctx, c, req, node, contentName, contentNameByNodeID); err != nil {
			return nil, fmt.Errorf("import-build: ensure SnapshotContent for node %s: %w", nodeID, err)
		}
	}

	// Set parent→child ownerRefs (parent SnapshotContent owns child SnapshotContent).
	for _, nodeID := range order {
		node := nodeByID[nodeID]
		if len(node.ChildNodeIDs) == 0 {
			continue
		}
		parentName := contentNameByNodeID[nodeID]
		parentContent := &storagev1alpha1.SnapshotContent{}
		if err := c.Get(ctx, client.ObjectKey{Name: parentName}, parentContent); err != nil {
			return nil, fmt.Errorf("import-build: get parent SnapshotContent %s: %w", parentName, err)
		}
		for _, childID := range node.ChildNodeIDs {
			childName := contentNameByNodeID[childID]
			childContent := &storagev1alpha1.SnapshotContent{}
			if err := c.Get(ctx, client.ObjectKey{Name: childName}, childContent); err != nil {
				return nil, fmt.Errorf("import-build: get child SnapshotContent %s: %w", childName, err)
			}
			if _, err := controllercommon.EnsureLifecycleOwnerRef(
				ctx, c, childContent,
				controllercommon.SnapshotContentOwnerReference(parentContent),
			); err != nil {
				return nil, fmt.Errorf("import-build: ownerRef child %s → parent %s: %w", childName, parentName, err)
			}
		}
	}

	// Create the root Snapshot with spec.existingContentRef.
	rootContentName := contentNameByNodeID[req.RootNodeID]
	snapshotName := req.SnapshotName

	snap := &storagev1alpha1.Snapshot{}
	err = c.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: snapshotName}, snap)
	switch {
	case apierrors.IsNotFound(err):
		snap = &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:  req.Namespace,
				Name:       snapshotName,
				Finalizers: []string{snapshotpkg.FinalizerSnapshot},
			},
			Spec: storagev1alpha1.SnapshotSpec{
				ExistingContentRef: &storagev1alpha1.SnapshotExistingContentRef{
					Name: rootContentName,
				},
			},
		}
		if cerr := c.Create(ctx, snap); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return nil, fmt.Errorf("import-build: create Snapshot %s/%s: %w", req.Namespace, snapshotName, cerr)
		}
	case err != nil:
		return nil, fmt.Errorf("import-build: get Snapshot %s/%s: %w", req.Namespace, snapshotName, err)
	default:
		// Already exists - verify it still points at the same content (idempotency).
		if snap.Spec.ExistingContentRef == nil || snap.Spec.ExistingContentRef.Name != rootContentName {
			return nil, fmt.Errorf("import-build: Snapshot %s/%s already exists with a different existingContentRef", req.Namespace, snapshotName)
		}
	}

	return &ImportBuildResult{
		SnapshotName:            snapshotName,
		RootSnapshotContentName: rootContentName,
	}, nil
}

// ensureImportSnapshotContent creates or verifies one SnapshotContent node.
func ensureImportSnapshotContent(
	ctx context.Context,
	c client.Client,
	req ImportBuildRequest,
	node *ImportBuildNode,
	contentName string,
	contentNameByNodeID map[string]string,
) error {
	content := &storagev1alpha1.SnapshotContent{}
	err := c.Get(ctx, client.ObjectKey{Name: contentName}, content)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get SnapshotContent %s: %w", contentName, err)
	}
	if apierrors.IsNotFound(err) {
		content = &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{
				Name: contentName,
				Labels: map[string]string{
					labelImportMode: labelValueTrue,
					labelSourceSnap: req.SnapshotName,
					labelSourceNS:   req.Namespace,
				},
				Finalizers: []string{snapshotpkg.FinalizerParentProtect},
			},
			Spec: storagev1alpha1.SnapshotContentSpec{
				DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
			},
		}
		if cerr := c.Create(ctx, content); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return fmt.Errorf("create SnapshotContent %s: %w", contentName, cerr)
		}
		if gerr := c.Get(ctx, client.ObjectKey{Name: contentName}, content); gerr != nil {
			return fmt.Errorf("get SnapshotContent %s after create: %w", contentName, gerr)
		}
	}

	// Publish manifestCheckpointName via the canonical publish helper.
	if node.ManifestCheckpointName != "" {
		if err := snapshotcontent.PublishSnapshotContentManifestCheckpointName(ctx, c, contentName, node.ManifestCheckpointName); err != nil {
			return fmt.Errorf("publish MCP name on %s: %w", contentName, err)
		}
	}

	// Publish dataRefs via the canonical publish helper.
	if len(node.DataRefs) > 0 {
		bindings, err := importDataRefBindings(node.DataRefs)
		if err != nil {
			return fmt.Errorf("build dataRefs for %s: %w", contentName, err)
		}
		if err := snapshotcontent.PublishSnapshotContentDataRefs(ctx, c, contentName, bindings); err != nil {
			return fmt.Errorf("publish dataRefs on %s: %w", contentName, err)
		}
	}

	// Publish children refs via the canonical publish helper.
	childRefs := make([]storagev1alpha1.SnapshotContentChildRef, 0, len(node.ChildNodeIDs))
	for _, childID := range node.ChildNodeIDs {
		childRefs = append(childRefs, storagev1alpha1.SnapshotContentChildRef{Name: contentNameByNodeID[childID]})
	}
	if err := snapshotcontent.PublishSnapshotContentChildrenRefs(ctx, c, contentName, childRefs); err != nil {
		return fmt.Errorf("publish children refs on %s: %w", contentName, err)
	}

	return nil
}

// importDataRefBindings converts ImportBuildDataRef entries to SnapshotDataBinding entries.
func importDataRefBindings(refs []ImportBuildDataRef) ([]storagev1alpha1.SnapshotDataBinding, error) {
	bindings := make([]storagev1alpha1.SnapshotDataBinding, 0, len(refs))
	for _, ref := range refs {
		if ref.VolumeSnapshotContentName == "" {
			return nil, fmt.Errorf("dataRef with targetUID %q has empty volumeSnapshotContentName", ref.TargetUID)
		}
		if ref.TargetUID == "" {
			return nil, fmt.Errorf("dataRef for VSC %q has empty targetUID", ref.VolumeSnapshotContentName)
		}
		bindings = append(bindings, storagev1alpha1.SnapshotDataBinding{
			TargetUID: ref.TargetUID,
			Target: storagev1alpha1.SnapshotSubjectRef{
				APIVersion: ref.OriginalAPIVersion,
				Kind:       ref.OriginalKind,
				Name:       ref.OriginalName,
				Namespace:  ref.OriginalNamespace,
			},
			Artifact: storagev1alpha1.SnapshotDataArtifactRef{
				APIVersion: "snapshot.storage.k8s.io/v1",
				Kind:       "VolumeSnapshotContent",
				Name:       ref.VolumeSnapshotContentName,
			},
		})
	}
	return bindings, nil
}

// ensureVSCRetainPolicy patches the VolumeSnapshotContent.spec.deletionPolicy to "Retain".
// This prevents the external snapshot controller from deleting the VSC when the bound
// VolumeSnapshot is deleted (impedance fix for the VS→VSC lifecycle during import).
func ensureVSCRetainPolicy(ctx context.Context, c client.Client, vscName string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vsc := &unstructured.Unstructured{}
		vsc.SetGroupVersionKind(vscGVK)
		if err := c.Get(ctx, client.ObjectKey{Name: vscName}, vsc); err != nil {
			if apierrors.IsNotFound(err) {
				// VSC not yet created; caller will retry when it becomes available.
				return nil
			}
			return err
		}
		current, _, _ := unstructured.NestedString(vsc.Object, "spec", "deletionPolicy")
		if current == "Retain" {
			return nil
		}
		base := vsc.DeepCopy()
		if err := unstructured.SetNestedField(vsc.Object, "Retain", "spec", "deletionPolicy"); err != nil {
			return fmt.Errorf("set deletionPolicy on VSC %s: %w", vscName, err)
		}
		return c.Patch(ctx, vsc, client.MergeFrom(base))
	})
}

// importContentName returns a deterministic SnapshotContent name for an import node.
func importContentName(namespace, snapshotName, nodeID string) string {
	h := sha256.Sum256([]byte(namespace + "/" + snapshotName + "/content/" + nodeID))
	return "import-ct-" + hex.EncodeToString(h[:8])
}

// topoSort returns nodeIDs in topological order (leaves first, root last) via DFS post-order.
func topoSort(rootID string, nodeByID map[string]*ImportBuildNode) ([]string, error) {
	visited := make(map[string]bool, len(nodeByID))
	inStack := make(map[string]bool, len(nodeByID))
	var order []string

	var visit func(id string) error
	visit = func(id string) error {
		if inStack[id] {
			return fmt.Errorf("cycle detected at node %q", id)
		}
		if visited[id] {
			return nil
		}
		inStack[id] = true
		node, ok := nodeByID[id]
		if !ok {
			return fmt.Errorf("node %q referenced but not found in nodes list", id)
		}
		for _, childID := range node.ChildNodeIDs {
			if err := visit(childID); err != nil {
				return err
			}
		}
		inStack[id] = false
		visited[id] = true
		order = append(order, id)
		return nil
	}

	if err := visit(rootID); err != nil {
		return nil, err
	}
	return order, nil
}
