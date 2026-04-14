/*
Copyright 2025 Flant JSC

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
	"math"
	"net/http"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

// AggregatedStatusError carries HTTP status for NamespaceSnapshot aggregated manifests (see spec doc linked on BuildAggregatedJSON).
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

// AggregatedNamespaceManifests builds a single JSON array of manifest objects for a NamespaceSnapshot subtree.
type AggregatedNamespaceManifests struct {
	client  client.Client
	archive *ArchiveService
}

// NewAggregatedNamespaceManifests creates an aggregated-manifests service for the manifests subresource.
func NewAggregatedNamespaceManifests(c client.Client, a *ArchiveService) *AggregatedNamespaceManifests {
	return &AggregatedNamespaceManifests{client: c, archive: a}
}

// BuildAggregatedJSON returns a JSON array of objects (fail-whole). SSOT: docs/.../namespace-snapshot-aggregated-manifests-pr4.md
// When the NamespaceSnapshot is gone but retained NamespaceSnapshotContent still exists (same spec ref name/namespace), resolves content by listing.
func (s *AggregatedNamespaceManifests) BuildAggregatedJSON(ctx context.Context, namespace, snapshotName string) ([]byte, error) {
	nsSnap := &storagev1alpha1.NamespaceSnapshot{}
	err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: snapshotName}, nsSnap)
	if err == nil {
		bound := nsSnap.Status.BoundSnapshotContentName
		if bound == "" {
			return nil, NewAggregatedStatusError(http.StatusConflict, "Conflict", "boundSnapshotContentName is empty")
		}
		return s.marshalAggregatedFromRootNSC(ctx, bound)
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get NamespaceSnapshot: %w", err)
	}
	bound, ferr := s.findRetainedRootNSCName(ctx, namespace, snapshotName)
	if ferr != nil {
		return nil, ferr
	}
	if bound == "" {
		return nil, NewAggregatedStatusError(http.StatusNotFound, "NotFound",
			fmt.Sprintf("NamespaceSnapshot %s/%s not found", namespace, snapshotName))
	}
	return s.marshalAggregatedFromRootNSC(ctx, bound)
}

func (s *AggregatedNamespaceManifests) marshalAggregatedFromRootNSC(ctx context.Context, rootNSC string) ([]byte, error) {
	visited := make(map[string]struct{})
	seenKeys := make(map[string]struct{})
	var objects []map[string]interface{}
	if err := s.walkNSC(ctx, rootNSC, visited, &objects, seenKeys); err != nil {
		return nil, err
	}
	out, err := json.Marshal(objects)
	if err != nil {
		return nil, NewAggregatedStatusError(http.StatusInternalServerError, "InternalError", fmt.Sprintf("marshal aggregated manifests: %v", err))
	}
	return out, nil
}

func (s *AggregatedNamespaceManifests) findRetainedRootNSCName(ctx context.Context, namespace, snapshotName string) (string, error) {
	var list storagev1alpha1.NamespaceSnapshotContentList
	if err := s.client.List(ctx, &list); err != nil {
		return "", fmt.Errorf("list NamespaceSnapshotContent: %w", err)
	}
	var best string
	// Use MinInt64 so clients without CreationTimestamp (e.g. envtest/fake) still pick a retained root;
	// zero metav1.Time encodes as a large negative UnixNano(), which is still > MinInt64.
	var bestTs int64 = math.MinInt64
	for i := range list.Items {
		ref := list.Items[i].Spec.NamespaceSnapshotRef
		if ref.Namespace != namespace || ref.Name != snapshotName {
			continue
		}
		ts := list.Items[i].CreationTimestamp.UnixNano()
		if ts >= bestTs {
			bestTs = ts
			best = list.Items[i].Name
		}
	}
	return best, nil
}

func (s *AggregatedNamespaceManifests) walkNSC(ctx context.Context, nscName string, visited map[string]struct{}, objects *[]map[string]interface{}, seenKeys map[string]struct{}) error {
	if _, ok := visited[nscName]; ok {
		return NewAggregatedStatusError(http.StatusInternalServerError, "InternalError",
			fmt.Sprintf("cycle detected at NamespaceSnapshotContent %q", nscName))
	}
	visited[nscName] = struct{}{}

	nsc := &storagev1alpha1.NamespaceSnapshotContent{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: nscName}, nsc); err != nil {
		if apierrors.IsNotFound(err) {
			return NewAggregatedStatusError(http.StatusNotFound, "NotFound",
				fmt.Sprintf("NamespaceSnapshotContent %q not found", nscName))
		}
		return fmt.Errorf("get NamespaceSnapshotContent %q: %w", nscName, err)
	}

	mcpName := nsc.Status.ManifestCheckpointName
	if mcpName == "" {
		return NewAggregatedStatusError(http.StatusInternalServerError, "InternalError",
			fmt.Sprintf("manifestCheckpointName is empty for NamespaceSnapshotContent %q", nscName))
	}

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
			return NewAggregatedStatusError(http.StatusInternalServerError, "InternalError",
				fmt.Sprintf("duplicate object %q", key))
		}
		seenKeys[key] = struct{}{}
		*objects = append(*objects, obj)
	}

	children := append([]storagev1alpha1.NamespaceSnapshotContentChildRef(nil), nsc.Status.ChildrenSnapshotContentRefs...)
	sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })
	for _, ch := range children {
		if err := s.walkNSC(ctx, ch.Name, visited, objects, seenKeys); err != nil {
			return err
		}
	}
	return nil
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
