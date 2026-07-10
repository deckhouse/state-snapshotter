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

package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

// BuildSubtreeManifestIdentities walks the SnapshotContent subtree rooted at contentName (its own node
// ManifestCheckpoint plus every descendant reached through status.childrenSnapshotContentRefs) and returns
// the flat, de-duplicated set of captured object identities (apiVersion/kind/namespace/name/uid) as the
// JSON body of the snapshotcontents/<name>/subtree-manifest-identities subresource. It exists so an
// aggregator (the namespace-root Snapshot, or any domain aggregator) can compute its manifest exclude set
// (base − subtree) via the SDK without reading cluster-scoped SnapshotContent/ManifestCheckpoint itself.
//
// FAIL-CLOSED: it returns 409 (never a partial list) if any node in the subtree has no persisted MCP yet,
// if any MCP is not Ready, or if an object is captured by more than one node (a double-capture the wave
// barrier is meant to prevent). Only a complete, consistent subtree yields a 200 identity set.
func (s *AggregatedNamespaceManifests) BuildSubtreeManifestIdentities(ctx context.Context, contentName string) ([]byte, error) {
	if contentName == "" {
		return nil, NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", "content name is required")
	}
	objects := make([]map[string]interface{}, 0)
	seenKeys := make(map[string]struct{})
	visited := make(map[string]struct{})
	if err := s.collectSubtreeObjects(ctx, contentName, &objects, seenKeys, visited); err != nil {
		return nil, err
	}
	identities := make([]snapshotsdk.SubtreeManifestIdentity, 0, len(objects))
	for _, obj := range objects {
		id, err := subtreeManifestIdentityFrom(obj)
		if err != nil {
			return nil, NewAggregatedStatusError(http.StatusInternalServerError, "InternalError", err.Error())
		}
		identities = append(identities, id)
	}
	out, err := json.Marshal(snapshotsdk.SubtreeManifestIdentitiesResponse{Identities: identities})
	if err != nil {
		return nil, NewAggregatedStatusError(http.StatusInternalServerError, "InternalError", fmt.Sprintf("marshal subtree identities: %v", err))
	}
	return out, nil
}

// collectSubtreeObjects recurses one SnapshotContent node: it appends the node's own MCP objects (reusing
// appendObjectsFromManifestCheckpoint, which fails closed with 409 on a not-Ready MCP and on cross-node
// duplicates via the shared seenKeys set), then descends into every declared child content. An empty
// manifestCheckpointName is a 409 (a subtree node with no persisted manifest means the subtree is not
// fully persisted). visited guards against a malformed (cyclic) content graph.
func (s *AggregatedNamespaceManifests) collectSubtreeObjects(
	ctx context.Context,
	contentName string,
	objects *[]map[string]interface{},
	seenKeys map[string]struct{},
	visited map[string]struct{},
) error {
	if contentName == "" {
		return NewAggregatedStatusError(http.StatusConflict, "Conflict", "empty child SnapshotContent name in subtree")
	}
	if _, seen := visited[contentName]; seen {
		return nil
	}
	visited[contentName] = struct{}{}

	content := &storagev1alpha1.SnapshotContent{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			return NewAggregatedStatusError(http.StatusNotFound, "NotFound", fmt.Sprintf("SnapshotContent %q not found", contentName))
		}
		return fmt.Errorf("get SnapshotContent %q: %w", contentName, err)
	}

	mcpName := content.Status.ManifestCheckpointName
	if mcpName == "" {
		return NewAggregatedStatusError(http.StatusConflict, "Conflict",
			fmt.Sprintf("SnapshotContent %q has empty manifestCheckpointName (subtree not fully persisted)", contentName))
	}
	if err := s.appendObjectsFromManifestCheckpoint(ctx, mcpName, objects, seenKeys); err != nil {
		return err
	}
	for _, childRef := range content.Status.ChildrenSnapshotContentRefs {
		if err := s.collectSubtreeObjects(ctx, childRef.Name, objects, seenKeys, visited); err != nil {
			return err
		}
	}
	return nil
}

// subtreeManifestIdentityFrom projects one decoded manifest object down to its identity fields. It mirrors
// aggregatedObjectIdentityKey's required-field checks (apiVersion/kind/name) but keeps namespace and uid.
func subtreeManifestIdentityFrom(obj map[string]interface{}) (snapshotsdk.SubtreeManifestIdentity, error) {
	apiVersion, _ := obj["apiVersion"].(string)
	kind, _ := obj["kind"].(string)
	metaObj, ok := obj["metadata"].(map[string]interface{})
	if !ok {
		return snapshotsdk.SubtreeManifestIdentity{}, fmt.Errorf("object missing metadata")
	}
	name, _ := metaObj["name"].(string)
	namespace, _ := metaObj["namespace"].(string)
	uid, _ := metaObj["uid"].(string)
	if apiVersion == "" || kind == "" || name == "" {
		return snapshotsdk.SubtreeManifestIdentity{}, fmt.Errorf("object missing apiVersion, kind, or metadata.name")
	}
	return snapshotsdk.SubtreeManifestIdentity{
		APIVersion: apiVersion,
		Kind:       kind,
		Namespace:  namespace,
		Name:       name,
		UID:        uid,
	}, nil
}
