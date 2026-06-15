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
	"encoding/json"
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
)

// BuildNodeManifests returns the own-manifests JSON array for a single snapshot node identified by
// nodeID (the index's stable "<kind>--<namespace>--<name>" id) within the tree rooted at the
// namespaced Snapshot snapshotName. The bytes are exactly that node's ManifestCheckpoint objects, so
// the SnapshotImport upload->reconstruct path recreates a byte-faithful per-node ManifestCheckpoint.
//
// It is the export-side counterpart of the per-node import upload (?node=<id>): the whole-tree
// /manifests endpoint is deduped and flattened, so it cannot be split back per node. Per-node
// retrieval is therefore required to drive the per-node reconstruction (plan decision
// "per-node manifests"). Object namespaces are left intact; the restore compiler re-sets the target
// namespace at restore time (sanitizeForRestore), so the stored namespace is not authoritative.
func (s *Service) BuildNodeManifests(ctx context.Context, namespace, snapshotName, nodeID string) ([]byte, error) {
	if nodeID == "" {
		return nil, usecase.NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", "node selector is required")
	}
	root, err := s.resolver.ResolveRestoreTree(ctx, namespace, snapshotName)
	if err != nil {
		return nil, err
	}
	node := findNodeByID(root, nodeID)
	if node == nil {
		return nil, usecase.NewAggregatedStatusError(http.StatusNotFound, "NotFound",
			fmt.Sprintf("snapshot node %q not found in tree %s/%s", nodeID, namespace, snapshotName))
	}
	objs, err := s.loader.LoadManifests(ctx, node.ManifestCheckpointName)
	if err != nil {
		return nil, err
	}
	if objs == nil {
		objs = []unstructured.Unstructured{}
	}
	out, err := json.Marshal(objs)
	if err != nil {
		return nil, usecase.NewAggregatedStatusError(http.StatusInternalServerError, "InternalError", "internal error")
	}
	return out, nil
}

// findNodeByID returns the node in the run tree whose index id matches, or nil. The id scheme is the
// same indexNodeID used to build the index, so callers iterate index.Snapshots[].ID to retrieve each
// node's own manifests. The resolver guarantees an acyclic tree (the /manifests path detects
// duplicates with a 409), so this plain DFS needs no visited-set.
func findNodeByID(node *RestoreNode, id string) *RestoreNode {
	if node == nil {
		return nil
	}
	if indexNodeID(node) == id {
		return node
	}
	for _, c := range node.Children {
		if found := findNodeByID(c, id); found != nil {
			return found
		}
	}
	return nil
}
