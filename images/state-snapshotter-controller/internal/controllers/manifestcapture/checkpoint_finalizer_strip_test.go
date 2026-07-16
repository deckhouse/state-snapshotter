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

package manifestcapture

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	deckhousev1alpha1 "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/deckhouseio/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// decodeChunkObjects base64-decodes + gunzips a chunk's Spec.Data back into the captured object list,
// so the test asserts against exactly what was archived (not the in-memory input).
func decodeChunkObjects(t *testing.T, data string) []map[string]interface{} {
	t.Helper()
	gzipBytes, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatalf("base64 decode chunk: %v", err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(gzipBytes))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gr.Close()
	jsonBytes, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("gunzip chunk: %v", err)
	}
	var objs []map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &objs); err != nil {
		t.Fatalf("unmarshal chunk objects: %v", err)
	}
	return objs
}

// TestCreateChunks_StripsTransientCaptureFinalizerVerbatimRest proves the ONE field-level exception to
// verbatim capture: the self-induced transient pvc-as-source-protection finalizer is dropped, while every
// other finalizer and all other fields (status, managedFields, uid) are archived verbatim.
func TestCreateChunks_StripsTransientCaptureFinalizerVerbatimRest(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := deckhousev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	testLogger, err := logger.NewLogger("info")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Options{DefaultTTL: 10 * time.Minute, DefaultTTLStr: "10m", MaxChunkSizeBytes: 800000}

	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctrl, err := NewManifestCheckpointController(cl, cl, scheme, testLogger, cfg)
	if err != nil {
		t.Fatal(err)
	}

	pvc := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":          "bk-pvc",
			"namespace":     "app",
			"uid":           "pvc-uid-123",
			"managedFields": []interface{}{map[string]interface{}{"manager": "kube-controller-manager"}},
			"finalizers": []interface{}{
				"kubernetes.io/pvc-protection",
				"snapshot.storage.kubernetes.io/pvc-as-source-protection",
				"custom.io/keep",
			},
		},
		"spec":   map[string]interface{}{"accessModes": []interface{}{"ReadWriteOnce"}},
		"status": map[string]interface{}{"phase": "Bound"},
	}}

	if _, err := ctrl.createChunks(context.Background(), "cp-test", "cp-uid", []unstructured.Unstructured{pvc}); err != nil {
		t.Fatalf("createChunks: %v", err)
	}

	chunks := &storagev1alpha1.ManifestCheckpointContentChunkList{}
	if err := cl.List(context.Background(), chunks); err != nil {
		t.Fatalf("list chunks: %v", err)
	}
	if len(chunks.Items) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks.Items))
	}

	objs := decodeChunkObjects(t, chunks.Items[0].Spec.Data)
	if len(objs) != 1 {
		t.Fatalf("expected 1 archived object, got %d", len(objs))
	}
	got := objs[0]

	meta, _ := got["metadata"].(map[string]interface{})
	if meta == nil {
		t.Fatal("archived object has no metadata")
	}

	// Transient self-induced finalizer stripped; the two legitimate finalizers kept, in order.
	finalizers, _ := meta["finalizers"].([]interface{})
	want := []string{"kubernetes.io/pvc-protection", "custom.io/keep"}
	if len(finalizers) != len(want) {
		t.Fatalf("finalizers = %v, want %v", finalizers, want)
	}
	for i, w := range want {
		if finalizers[i] != w {
			t.Fatalf("finalizers[%d] = %v, want %q", i, finalizers[i], w)
		}
	}

	// Everything else is verbatim: uid, managedFields, status must survive untouched.
	if meta["uid"] != "pvc-uid-123" {
		t.Fatalf("metadata.uid = %v, want verbatim pvc-uid-123", meta["uid"])
	}
	if _, ok := meta["managedFields"]; !ok {
		t.Fatal("metadata.managedFields must be captured verbatim")
	}
	if _, ok := got["status"]; !ok {
		t.Fatal("status must be captured verbatim")
	}
}

// TestStripTransientCaptureFinalizers_DropsEmptyList proves the finalizers key is removed entirely when
// the only entry was the denylisted one, so the archived object matches a live object with no finalizers.
func TestStripTransientCaptureFinalizers_DropsEmptyList(t *testing.T) {
	m := map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":       "only-transient",
			"finalizers": []interface{}{"snapshot.storage.kubernetes.io/pvc-as-source-protection"},
		},
	}
	stripTransientCaptureFinalizers(m)
	meta, _ := m["metadata"].(map[string]interface{})
	if _, ok := meta["finalizers"]; ok {
		t.Fatal("finalizers key must be removed when the list becomes empty")
	}
}

// TestStripTransientCaptureFinalizers_NoFinalizersIsNoop proves the helper is safe on objects that carry
// no finalizers at all.
func TestStripTransientCaptureFinalizers_NoFinalizersIsNoop(t *testing.T) {
	m := map[string]interface{}{"metadata": map[string]interface{}{"name": "no-finalizers"}}
	stripTransientCaptureFinalizers(m)
	meta, _ := m["metadata"].(map[string]interface{})
	if _, ok := meta["finalizers"]; ok {
		t.Fatal("no finalizers key should have been added")
	}
}
