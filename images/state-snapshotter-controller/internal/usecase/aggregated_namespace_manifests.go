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
	"errors"
	"fmt"
	"net/http"
	"strings"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
)

// AggregatedStatusError carries HTTP status for Snapshot aggregated manifests (see spec doc linked on BuildAggregatedJSON).
type AggregatedStatusError struct {
	HTTPStatus int
	Reason     string
	Message    string
}

func (e *AggregatedStatusError) Error() string { return e.Message }

// NewAggregatedStatusError builds a typed error for the HTTP layer.
func NewAggregatedStatusError(httpStatus int, reason, message string) *AggregatedStatusError {
	return &AggregatedStatusError{HTTPStatus: httpStatus, Reason: reason, Message: message}
}

// AggregatedNamespaceManifests builds a single JSON array of manifest objects for a Snapshot subtree.
type AggregatedNamespaceManifests struct {
	client    client.Client
	archive   *ArchiveService
	graphLive snapshotgraphregistry.LiveReader
}

// NewAggregatedNamespaceManifests creates an aggregated-manifests service for the manifests subresource.
func NewAggregatedNamespaceManifests(c client.Client, a *ArchiveService, graphLive snapshotgraphregistry.LiveReader) *AggregatedNamespaceManifests {
	return &AggregatedNamespaceManifests{client: c, archive: a, graphLive: graphLive}
}

// BuildAggregatedJSON returns a JSON array of objects (fail-whole). SSOT: docs/.../snapshot-aggregated-manifests-pr4.md
func (s *AggregatedNamespaceManifests) BuildAggregatedJSON(ctx context.Context, namespace, snapshotName string) ([]byte, error) {
	nsSnap := &storagev1alpha1.Snapshot{}
	err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: snapshotName}, nsSnap)
	if err == nil {
		bound := nsSnap.Status.BoundSnapshotContentName
		if bound == "" {
			return nil, NewAggregatedStatusError(http.StatusConflict, "Conflict", "boundSnapshotContentName is empty")
		}
		return s.marshalAggregatedFromRootContent(ctx, bound)
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get Snapshot: %w", err)
	}
	if bound, retainedErr := s.retainedRootContentForSnapshot(ctx, namespace, snapshotName); retainedErr == nil {
		return s.marshalAggregatedFromRootContent(ctx, bound)
	} else if !apierrors.IsNotFound(retainedErr) {
		return nil, retainedErr
	}
	return nil, NewAggregatedStatusError(http.StatusNotFound, "NotFound",
		fmt.Sprintf("Snapshot %s/%s not found", namespace, snapshotName))
}

func (s *AggregatedNamespaceManifests) retainedRootContentForSnapshot(ctx context.Context, namespace, snapshotName string) (string, error) {
	okName := namespacemanifest.SnapshotRootObjectKeeperName(namespace, snapshotName)
	ok := &deckhousev1alpha1.ObjectKeeper{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: okName}, ok); err != nil {
		return "", err
	}
	if ok.Spec.FollowObjectRef == nil ||
		ok.Spec.FollowObjectRef.APIVersion != storagev1alpha1.SchemeGroupVersion.String() ||
		ok.Spec.FollowObjectRef.Kind != "Snapshot" ||
		ok.Spec.FollowObjectRef.Namespace != namespace ||
		ok.Spec.FollowObjectRef.Name != snapshotName {
		return "", NewAggregatedStatusError(http.StatusConflict, "Conflict",
			fmt.Sprintf("ObjectKeeper %q does not follow Snapshot %s/%s", okName, namespace, snapshotName))
	}

	contents := &storagev1alpha1.SnapshotContentList{}
	if err := s.client.List(ctx, contents); err != nil {
		return "", fmt.Errorf("list SnapshotContent for retained Snapshot %s/%s: %w", namespace, snapshotName, err)
	}
	var matches []string
	for i := range contents.Items {
		content := &contents.Items[i]
		for _, ref := range content.OwnerReferences {
			if ref.APIVersion == "deckhouse.io/v1alpha1" &&
				ref.Kind == "ObjectKeeper" &&
				ref.Name == ok.Name &&
				ref.UID == ok.UID &&
				ref.Controller != nil &&
				*ref.Controller {
				matches = append(matches, content.Name)
				break
			}
		}
	}
	switch len(matches) {
	case 0:
		return "", NewAggregatedStatusError(http.StatusNotFound, "NotFound",
			fmt.Sprintf("retained SnapshotContent for Snapshot %s/%s not found via ObjectKeeper %q", namespace, snapshotName, okName))
	case 1:
		return matches[0], nil
	default:
		return "", NewAggregatedStatusError(http.StatusConflict, "Conflict",
			fmt.Sprintf("multiple retained SnapshotContents for Snapshot %s/%s found via ObjectKeeper %q", namespace, snapshotName, okName))
	}
}

func (s *AggregatedNamespaceManifests) marshalAggregatedFromRootContent(ctx context.Context, rootContent string) ([]byte, error) {
	visited := make(map[string]struct{})
	seenKeys := make(map[string]struct{})
	objects := make([]map[string]interface{}, 0)
	if err := s.walkContent(ctx, rootContent, visited, &objects, seenKeys); err != nil {
		return nil, err
	}
	out, err := json.Marshal(objects)
	if err != nil {
		return nil, NewAggregatedStatusError(http.StatusInternalServerError, "InternalError", fmt.Sprintf("marshal aggregated manifests: %v", err))
	}
	return out, nil
}

// BuildAggregatedJSONFromContent returns aggregated manifests starting from any registered content node.
func (s *AggregatedNamespaceManifests) BuildAggregatedJSONFromContent(ctx context.Context, contentGVK schema.GroupVersionKind, contentName string) ([]byte, error) {
	if contentName == "" || contentGVK.Empty() {
		return nil, NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", "content GVK and name are required")
	}
	if contentGVK != SnapshotContentGVK() {
		return nil, NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", fmt.Sprintf("unsupported content resource %s", contentGVK.String()))
	}

	visited := make(map[string]struct{})
	seenKeys := make(map[string]struct{})
	objects := make([]map[string]interface{}, 0)
	if err := s.walkContent(ctx, contentName, visited, &objects, seenKeys); err != nil {
		return nil, err
	}
	out, err := json.Marshal(objects)
	if err != nil {
		return nil, NewAggregatedStatusError(http.StatusInternalServerError, "InternalError", fmt.Sprintf("marshal aggregated manifests: %v", err))
	}
	return out, nil
}

// BuildAggregatedJSONFromSnapshot resolves a namespaced Snapshot-like object to its
// registered SnapshotContent and returns aggregated manifests for that content subtree.
func (s *AggregatedNamespaceManifests) BuildAggregatedJSONFromSnapshot(ctx context.Context, snapshotGVK schema.GroupVersionKind, namespace, snapshotName string) ([]byte, error) {
	if snapshotName == "" || namespace == "" || snapshotGVK.Empty() {
		return nil, NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", "snapshot GVK, namespace, and name are required")
	}
	snap := &unstructured.Unstructured{}
	snap.SetGroupVersionKind(snapshotGVK)
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: snapshotName}, snap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, NewAggregatedStatusError(http.StatusNotFound, "NotFound", fmt.Sprintf("%s %s/%s not found", snapshotGVK.String(), namespace, snapshotName))
		}
		return nil, fmt.Errorf("get %s %s/%s: %w", snapshotGVK.String(), namespace, snapshotName, err)
	}
	contentName, _, err := unstructured.NestedString(snap.Object, "status", "boundSnapshotContentName")
	if err != nil {
		return nil, NewAggregatedStatusError(http.StatusInternalServerError, "InternalError", fmt.Sprintf("%s %s/%s has invalid status.boundSnapshotContentName: %v", snapshotGVK.String(), namespace, snapshotName, err))
	}
	if contentName == "" {
		return nil, NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", "boundSnapshotContentName is empty")
	}
	return s.BuildAggregatedJSONFromContent(ctx, SnapshotContentGVK(), contentName)
}

// IsRegisteredSnapshotGVK checks the live graph registry for an exact Snapshot GVK match.
func (s *AggregatedNamespaceManifests) IsRegisteredSnapshotGVK(ctx context.Context, snapshotGVK schema.GroupVersionKind) (bool, error) {
	if snapshotGVK.Empty() {
		return false, nil
	}
	reg, err := s.currentAggregatedRegistry(ctx)
	if err != nil {
		return false, err
	}
	return reg.HasSnapshotGVK(snapshotGVK), nil
}

func (s *AggregatedNamespaceManifests) currentAggregatedRegistry(ctx context.Context) (*snapshot.GVKRegistry, error) {
	if s.graphLive == nil {
		return nil, NewAggregatedStatusError(http.StatusServiceUnavailable, "RegistryNotReady", "heterogeneous content traversal requires snapshot graph registry")
	}
	reg := s.graphLive.Current()
	if reg == nil {
		if err := s.graphLive.TryRefresh(ctx); err != nil && !errors.Is(err, snapshotgraphregistry.ErrRefreshNotConfigured) {
			return nil, err
		}
		reg = s.graphLive.Current()
	}
	if reg == nil {
		return nil, NewAggregatedStatusError(http.StatusServiceUnavailable, "RegistryNotReady", snapshotgraphregistry.ErrGraphRegistryNotReady.Error())
	}
	return reg, nil
}

// walkContent visits SnapshotContent nodes for aggregated manifests.
// Traversal uses only status.childrenSnapshotContentRefs on each node (see
// docs/state-snapshotter-rework/spec/snapshot-aggregated-manifests-pr4.md §2.2).
// It does not list SnapshotContent or Snapshot to discover children,
// and does not follow status.childrenSnapshotRefs on Snapshot — consistent with
// system-spec §3.4 (INV-REF-C1): empty or absent content refs mean no further descent from that node.
//
// Graph DFS is shared with WalkSnapshotContentSubtree so domain code and aggregation use the same ref-only walk.
func (s *AggregatedNamespaceManifests) walkContent(ctx context.Context, contentName string, _ map[string]struct{}, objects *[]map[string]interface{}, seenKeys map[string]struct{}) error {
	visit := func(ctx context.Context, content *storagev1alpha1.SnapshotContent) error {
		mcpName := content.Status.ManifestCheckpointName
		if mcpName == "" {
			return NewAggregatedStatusError(http.StatusInternalServerError, "InternalError",
				fmt.Sprintf("manifestCheckpointName is empty for SnapshotContent %q", content.Name))
		}
		return s.appendObjectsFromManifestCheckpoint(ctx, mcpName, objects, seenKeys)
	}
	err := WalkSnapshotContentSubtree(ctx, s.client, contentName, visit)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrSnapshotContentCycle) {
		return NewAggregatedStatusError(http.StatusInternalServerError, "InternalError", err.Error())
	}
	if apierrors.IsNotFound(err) {
		return NewAggregatedStatusError(http.StatusNotFound, "NotFound", err.Error())
	}
	var st *AggregatedStatusError
	if errors.As(err, &st) {
		return err
	}
	return err
}

func (s *AggregatedNamespaceManifests) appendObjectsFromManifestCheckpoint(
	ctx context.Context,
	mcpName string,
	objects *[]map[string]interface{},
	seenKeys map[string]struct{},
) error {
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		if apierrors.IsNotFound(err) {
			return NewAggregatedStatusError(http.StatusNotFound, "NotFound",
				fmt.Sprintf("ManifestCheckpoint %q not found", mcpName))
		}
		return fmt.Errorf("get ManifestCheckpoint %q: %w", mcpName, err)
	}

	req := &ArchiveRequest{
		CheckpointName:  mcpName,
		CheckpointUID:   string(mcp.UID),
		SourceNamespace: mcp.Spec.SourceNamespace,
	}
	raw, _, err := s.archive.GetArchiveFromCheckpoint(ctx, mcp, req)
	if err != nil {
		return classifyAggregatedArchiveError(err)
	}

	var arr []map[string]interface{}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return NewAggregatedStatusError(http.StatusInternalServerError, "InternalError",
			fmt.Sprintf("invalid MCP JSON for %q: %v", mcpName, err))
	}

	for _, obj := range arr {
		key, err := aggregatedObjectIdentityKey(obj)
		if err != nil {
			return NewAggregatedStatusError(http.StatusInternalServerError, "InternalError", err.Error())
		}
		if _, dup := seenKeys[key]; dup {
			return NewAggregatedStatusError(http.StatusConflict, "Conflict",
				fmt.Sprintf("duplicate object detected in snapshot tree: %s", key))
		}
		seenKeys[key] = struct{}{}
		if !makeAggregatedObjectNamespaceRelative(obj) {
			continue
		}
		*objects = append(*objects, obj)
	}
	return nil
}

func makeAggregatedObjectNamespaceRelative(obj map[string]interface{}) bool {
	meta, ok := obj["metadata"].(map[string]interface{})
	if !ok {
		return false
	}
	if ns, _ := meta["namespace"].(string); ns == "" {
		return false
	}
	delete(meta, "namespace")
	return true
}

func aggregatedObjectIdentityKey(obj map[string]interface{}) (string, error) {
	apiVersion, _ := obj["apiVersion"].(string)
	kind, _ := obj["kind"].(string)
	meta, ok := obj["metadata"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("object missing metadata")
	}
	name, _ := meta["name"].(string)
	ns, _ := meta["namespace"].(string)
	if apiVersion == "" || kind == "" || name == "" {
		return "", fmt.Errorf("object missing apiVersion, kind, or metadata.name")
	}
	if ns == "" {
		ns = "_cluster"
	}
	return fmt.Sprintf("%s|%s|%s|%s", apiVersion, kind, ns, name), nil
}

func classifyAggregatedArchiveError(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "checkpoint is not ready") {
		return NewAggregatedStatusError(http.StatusConflict, "Conflict", msg)
	}
	return NewAggregatedStatusError(http.StatusInternalServerError, "InternalError", msg)
}
