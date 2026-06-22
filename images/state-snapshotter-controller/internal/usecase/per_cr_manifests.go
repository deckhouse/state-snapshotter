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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// BuildSingleNodeJSON returns the manifests of EXACTLY ONE snapshot node — the objects in its own
// ManifestCheckpoint — as a JSON array, WITHOUT walking children. This is the single manifest-read
// primitive after C9: per-CR manifests-download (C3) is its only consumer pattern — export walks the
// tree client-side and fetches each node's own manifests, DataImport fetches the original PVC manifest
// (storageClass/volumeMode/status.capacity) of a single leaf, and the recursive per-CR restore (C9)
// uses it as each node's base. There is no whole-subtree server-side aggregation anymore.
//
// It decodes a node's manifests as raw objects with status preserved, namespace made relative, and
// intra-node dedup.
func (s *AggregatedNamespaceManifests) BuildSingleNodeJSON(ctx context.Context, contentName string) ([]byte, error) {
	if contentName == "" {
		return nil, NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", "content name is required")
	}
	content := &storagev1alpha1.SnapshotContent{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, NewAggregatedStatusError(http.StatusNotFound, "NotFound", fmt.Sprintf("SnapshotContent %q not found", contentName))
		}
		return nil, fmt.Errorf("get SnapshotContent %q: %w", contentName, err)
	}
	mcpName := content.Status.ManifestCheckpointName
	if mcpName == "" {
		return nil, NewAggregatedStatusError(http.StatusConflict, "Conflict",
			fmt.Sprintf("SnapshotContent %q has empty manifestCheckpointName", contentName))
	}

	seenKeys := make(map[string]struct{})
	objects := make([]map[string]interface{}, 0)
	if err := s.appendObjectsFromManifestCheckpoint(ctx, mcpName, &objects, seenKeys); err != nil {
		return nil, err
	}
	out, err := json.Marshal(objects)
	if err != nil {
		return nil, NewAggregatedStatusError(http.StatusInternalServerError, "InternalError", fmt.Sprintf("marshal single-node manifests: %v", err))
	}
	return out, nil
}

// BuildSingleNodeJSONForRootSnapshot returns the own-node manifests of a core Snapshot root (no subtree),
// resolving its SnapshotContent from live status, then from retained content via the root ObjectKeeper.
func (s *AggregatedNamespaceManifests) BuildSingleNodeJSONForRootSnapshot(ctx context.Context, namespace, snapshotName string) ([]byte, error) {
	rootContent, err := s.resolveRootContentName(ctx, namespace, snapshotName)
	if err != nil {
		return nil, err
	}
	return s.BuildSingleNodeJSON(ctx, rootContent)
}

// BuildSingleNodeJSONFromSnapshot returns the own-node manifests of any namespaced snapshot-like CR
// (by GVK; e.g. a domain leaf or a generic-PVC node), resolving its bound SnapshotContent via
// status.boundSnapshotContentName. No subtree recursion.
func (s *AggregatedNamespaceManifests) BuildSingleNodeJSONFromSnapshot(ctx context.Context, snapshotGVK schema.GroupVersionKind, namespace, snapshotName string) ([]byte, error) {
	contentName, err := s.resolveContentNameFromSnapshot(ctx, snapshotGVK, namespace, snapshotName)
	if err != nil {
		return nil, err
	}
	return s.BuildSingleNodeJSON(ctx, contentName)
}

// BuildSingleNodeJSONFromContent returns the own-node manifests for a cluster-scoped SnapshotContent
// addressed directly by name. This backs manifests-download on snapshotcontents/<name>, used by
// DataImport on the import path to read the original manifest before any namespaced snapshot CR binds.
func (s *AggregatedNamespaceManifests) BuildSingleNodeJSONFromContent(ctx context.Context, contentName string) ([]byte, error) {
	return s.BuildSingleNodeJSON(ctx, contentName)
}
