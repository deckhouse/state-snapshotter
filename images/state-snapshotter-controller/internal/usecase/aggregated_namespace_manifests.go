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
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
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
	client   client.Client
	archive  *ArchiveService
	graphReg *snapshot.GVKRegistry
}

// NewAggregatedNamespaceManifests creates an aggregated-manifests service for the manifests subresource.
// graphReg lists DSC/bootstrap snapshot↔content pairs so heterogeneous childrenSnapshotContentRefs can be traversed without domain imports.
func NewAggregatedNamespaceManifests(c client.Client, a *ArchiveService, graphReg *snapshot.GVKRegistry) *AggregatedNamespaceManifests {
	return &AggregatedNamespaceManifests{client: c, archive: a, graphReg: graphReg}
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

// findRetainedRootNSCName is a retained-read helper when the NamespaceSnapshot object is already deleted.
// It lists cluster NamespaceSnapshotContent and picks the newest (by CreationTimestamp) whose
// spec.namespaceSnapshotRef matches (namespace, snapshotName). If several retained contents exist for the same
// name (recreated snapshots), this is a best-effort policy for the aggregated manifests subresource only;
// it is not a strong multi-version product contract.
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

// walkNSC visits NamespaceSnapshotContent nodes for aggregated manifests (N2b PR4).
// Traversal uses only status.childrenSnapshotContentRefs on each node (see
// docs/state-snapshotter-rework/spec/namespace-snapshot-aggregated-manifests-pr4.md §2.2).
// It does not list NamespaceSnapshotContent or NamespaceSnapshot to discover children,
// and does not follow status.childrenSnapshotRefs on NamespaceSnapshot — consistent with
// system-spec §3.4 (INV-REF-C1): empty or absent content refs mean no further descent from that node.
//
// Graph DFS is shared with WalkNamespaceSnapshotContentSubtree / WalkNamespaceSnapshotContentSubtreeWithRegistry
// (namespacesnapshot_content_graph.go) so domain code and aggregation use the same ref-only walk (§3-E4).
// Dedicated snapshot content nodes under childrenSnapshotContentRefs have no MCP on this aggregated path;
// see namespacesnapshot_content_graph.go.
func (s *AggregatedNamespaceManifests) walkNSC(ctx context.Context, nscName string, visited map[string]struct{}, objects *[]map[string]interface{}, seenKeys map[string]struct{}) error {
	visit := func(ctx context.Context, nsc *storagev1alpha1.NamespaceSnapshotContent) error {
		mcpName := nsc.Status.ManifestCheckpointName
		if mcpName == "" {
			return NewAggregatedStatusError(http.StatusInternalServerError, "InternalError",
				fmt.Sprintf("manifestCheckpointName is empty for NamespaceSnapshotContent %q", nsc.Name))
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
		return nil
	}
	var err error
	if s.graphReg != nil {
		err = walkNamespaceSnapshotContentSubtree(ctx, s.client, nscName, visited, visit, s.graphReg, nil)
	} else {
		err = WalkNamespaceSnapshotContentSubtree(ctx, s.client, nscName, visit)
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNamespaceSnapshotContentCycle) {
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
