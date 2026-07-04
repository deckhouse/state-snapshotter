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

package api //nolint:revive // package name matches internal/api directory

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

// Shared HTTP test helpers for the manifest subresources (used by the per-CR manifests-download tests
// and the subresources.snapshot.storage.k8s.io connector tests). encodeTestChunkData lives in
// archive_handler_test.go.

// createReadyMCPForAPI seeds a Ready ManifestCheckpoint (single chunk) carrying the given objects, so a
// manifests-download handler can decode a node's own manifests off it.
func createReadyMCPForAPI(t *testing.T, cl client.Client, mcpName string, objects []map[string]interface{}) {
	t.Helper()
	data, checksum := encodeTestChunkData(objects)
	chunk := &ssv1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: "chunk-" + mcpName},
		Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: mcpName,
			Index:          0,
			Data:           data,
			Checksum:       checksum,
			ObjectsCount:   len(objects),
		},
	}
	if err := cl.Create(context.Background(), chunk); err != nil {
		t.Fatal(err)
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: mcpName, UID: types.UID("uid-" + mcpName)},
		Spec:       ssv1alpha1.ManifestCheckpointSpec{SourceNamespace: "ns1"},
		Status: ssv1alpha1.ManifestCheckpointStatus{
			Chunks:       []ssv1alpha1.ChunkInfo{{Name: chunk.Name, Index: 0, Checksum: checksum}},
			TotalObjects: len(objects),
		},
	}
	meta.SetStatusCondition(&mcp.Status.Conditions, metav1.Condition{
		Type:   ssv1alpha1.ManifestCheckpointConditionTypeReady,
		Status: metav1.ConditionTrue,
		Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
	})
	if err := cl.Create(context.Background(), mcp); err != nil {
		t.Fatal(err)
	}
}

func getAggregatedObjects(t *testing.T, url string, wantStatus int) []map[string]interface{} {
	t.Helper()
	body := getRawResponse(t, url, wantStatus)
	var arr []map[string]interface{}
	if err := json.Unmarshal(body, &arr); err != nil {
		t.Fatal(err)
	}
	return arr
}

func getRawResponse(t *testing.T, url string, wantStatus int) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("status %d, want %d: %s", resp.StatusCode, wantStatus, string(body))
	}
	return body
}

func containsKindName(objects []map[string]interface{}, kind, name string) bool {
	for _, obj := range objects {
		if obj["kind"] != kind {
			continue
		}
		metaObj, ok := obj["metadata"].(map[string]interface{})
		if ok && metaObj["name"] == name {
			return true
		}
	}
	return false
}
