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
)

// ImportBuildRequest is the body for POST .../snapshots/{name}/import-build.
// It carries the snapshot tree topology, data volume references, and metadata
// needed to assemble the SnapshotContent tree and root Snapshot.
type ImportBuildRequest struct {
	// Namespace is injected by the HTTP handler from the URL path.
	Namespace string `json:"-"`
	// SnapshotName is injected by the HTTP handler from the URL path.
	SnapshotName string `json:"-"`

	// Nodes describes the snapshot content tree topology.
	// Each node maps to one generic SnapshotContent.
	Nodes []ImportBuildNode `json:"nodes"`

	// RootNodeID is the nodeId of the root node in the Nodes list.
	RootNodeID string `json:"rootNodeId"`

	// TTL is an optional retention TTL hint for the ObjectKeeper (e.g. "168h").
	TTL string `json:"ttl,omitempty"`
}

// ImportBuildNode describes one node in the snapshot content tree.
type ImportBuildNode struct {
	// NodeID is a unique identifier within the archive (from nodes.jsonl).
	NodeID string `json:"nodeId"`

	// ManifestCheckpointName is the name of the already-created import-mode MCP.
	// Empty if this node has no manifest data.
	ManifestCheckpointName string `json:"manifestCheckpointName,omitempty"`

	// ChildNodeIDs lists the nodeIds of child nodes.
	ChildNodeIDs []string `json:"childNodeIds,omitempty"`

	// DataRefs lists the VolumeSnapshotContent references for this node's data artifacts.
	DataRefs []ImportBuildDataRef `json:"dataRefs,omitempty"`
}

// ImportBuildDataRef associates a PVC target UID with a VolumeSnapshotContent artifact.
type ImportBuildDataRef struct {
	// TargetUID is the original PVC UID (key for dataRefs map in SnapshotContent.status).
	TargetUID string `json:"targetUID"`

	// OriginalAPIVersion / OriginalKind / OriginalName describe the original target resource.
	OriginalAPIVersion string `json:"originalAPIVersion,omitempty"`
	OriginalKind       string `json:"originalKind,omitempty"`
	OriginalName       string `json:"originalName,omitempty"`
	OriginalNamespace  string `json:"originalNamespace,omitempty"`

	// VolumeSnapshotContentName is the name of the VSC artifact.
	VolumeSnapshotContentName string `json:"volumeSnapshotContentName"`
}

// ImportBuildResult is returned by the build endpoint.
type ImportBuildResult struct {
	// SnapshotName is the name of the created Snapshot.
	SnapshotName string `json:"snapshotName"`
	// RootSnapshotContentName is the name of the root SnapshotContent.
	RootSnapshotContentName string `json:"rootSnapshotContentName"`
}

// buildSnapshotTree assembles the SnapshotContent tree and creates the root Snapshot.
// Implemented in task build-endpoint-reuse.
func (h *ImportHandler) buildSnapshotTree(ctx context.Context, req ImportBuildRequest) (*ImportBuildResult, error) {
	return buildSnapshotTreeImpl(ctx, h.client, req)
}
