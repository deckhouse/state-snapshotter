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

package snapshotcontent

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// Swap B (controller.go): the two ManifestCheckpoint readiness reads — resolveManifestCheckpointReady
// (mcp.status.conditions[Ready]) and firstMissingManifestCheckpointChunk (mcp.status.chunks[]) — now read
// the MCP from the cached r.Client. The chunk-existence GET inside firstMissingManifestCheckpointChunk
// deliberately stays on the uncached APIReader (a cached chunk Get would start a chunk informer, which
// Phase 2a avoids). One buildCommonSnapshotContentStatusPlan pass exercises both MCP reads and the chunk
// read, so the split counters prove the MCP read moved to the cache while the chunk read did not.
func TestManifestCheckpointReadFromCacheChunkStaysOnAPIReader(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := aggScheme(t)

	mcp := manifestCheckpointWithReady("mcp-x", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	mcp.Status.Chunks = []ssv1alpha1.ChunkInfo{{Name: "chunk-x", Index: 0}}
	chunk := &ssv1alpha1.ManifestCheckpointContentChunk{ObjectMeta: metav1.ObjectMeta{Name: "chunk-x"}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, chunk).Build()

	counters := newSplitCounters()
	cache, apiReader := counters.splitClients(base, nil)
	r := &SnapshotContentController{Client: cache, APIReader: apiReader, Scheme: scheme, GVKRegistry: snapshot.NewGVKRegistry()}

	content := commonContentWithStatus("c", "mcp-x")
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, content)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	// Behaviour parity: MCP Ready + the only chunk present, no children -> Ready=True (same as before swap).
	if plan.readyStatus != metav1.ConditionTrue {
		t.Fatalf("ready=%s, want True (MCP ready, chunk present, no children)", plan.readyStatus)
	}

	mcpGVK := schema.GroupVersionKind{Group: ssv1alpha1.SchemeGroupVersion.Group, Version: ssv1alpha1.SchemeGroupVersion.Version, Kind: "ManifestCheckpoint"}
	chunkGVK := schema.GroupVersionKind{Group: ssv1alpha1.SchemeGroupVersion.Group, Version: ssv1alpha1.SchemeGroupVersion.Version, Kind: "ManifestCheckpointContentChunk"}

	// Routing (the point of swap B): the MCP is read via the cache, never via the APIReader.
	if n := counters.getCount(roleClient, mcpGVK); n < 1 {
		t.Fatalf("ManifestCheckpoint GET via Client = %d, want >=1 after swap", n)
	}
	if n := counters.getCount(roleAPIReader, mcpGVK); n != 0 {
		t.Fatalf("ManifestCheckpoint GET via APIReader = %d, want 0 after swap", n)
	}
	// The chunk existence check stays uncached on purpose.
	if n := counters.getCount(roleAPIReader, chunkGVK); n < 1 {
		t.Fatalf("chunk GET via APIReader = %d, want >=1 (chunk stays uncached)", n)
	}
	if n := counters.getCount(roleClient, chunkGVK); n != 0 {
		t.Fatalf("chunk GET via Client = %d, want 0 (chunk must not be cached)", n)
	}
}
