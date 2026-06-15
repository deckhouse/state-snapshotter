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

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func newUploadHandler(t *testing.T, importName string) *ImportUploadHandler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add ss scheme: %v", err)
	}
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotImport{}).
		Build()
	if importName != "" {
		imp := &storagev1alpha1.SnapshotImport{ObjectMeta: metav1.ObjectMeta{Name: importName, Namespace: "ns"}}
		if err := cl.Create(context.Background(), imp); err != nil {
			t.Fatalf("create import: %v", err)
		}
	}
	log, _ := logger.NewLogger("error")
	return NewImportUploadHandler(cl, usecase.NewImportBlobStore(cl, log), log)
}

func doUpload(t *testing.T, h *ImportUploadHandler, method, url, sub string, offset int64, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, url, rdr)
	if offset >= 0 {
		req.Header.Set("X-Offset", strconv.FormatInt(offset, 10))
	}
	rec := httptest.NewRecorder()
	h.Handle(rec, req, "ns", "imp", sub)
	return rec
}

func TestImportUpload_ResumableHappyPath(t *testing.T) {
	h := newUploadHandler(t, "imp")

	// HEAD on a fresh blob reports offset 0.
	rec := doUpload(t, h, http.MethodHead, "http://x/", "index", -1, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD: expected 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-Next-Offset"); got != "0" {
		t.Fatalf("HEAD: expected X-Next-Offset 0, got %q", got)
	}

	// POST first chunk.
	rec = doUpload(t, h, http.MethodPost, "http://x/", "index", 0, []byte("abc"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST: expected 202, got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Next-Offset"); got != "3" {
		t.Fatalf("POST: expected X-Next-Offset 3, got %q", got)
	}

	// PUT next chunk at the resume offset.
	rec = doUpload(t, h, http.MethodPut, "http://x/", "index", 3, []byte("def"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("PUT: expected 202, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-Next-Offset"); got != "6" {
		t.Fatalf("PUT: expected X-Next-Offset 6, got %q", got)
	}

	// HEAD reflects the new offset.
	rec = doUpload(t, h, http.MethodHead, "http://x/", "index", -1, nil)
	if got := rec.Header().Get("X-Next-Offset"); got != "6" {
		t.Fatalf("HEAD after writes: expected 6, got %q", got)
	}
}

func TestImportUpload_RetransmitAndConflict(t *testing.T) {
	h := newUploadHandler(t, "imp")
	_ = doUpload(t, h, http.MethodPost, "http://x/", "index", 0, []byte("abcdef"))

	// Retransmit a stored prefix: no-op, 202, offset unchanged.
	rec := doUpload(t, h, http.MethodPut, "http://x/", "index", 0, []byte("abc"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("retransmit: expected 202, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-Next-Offset"); got != "6" {
		t.Fatalf("retransmit: expected 6, got %q", got)
	}

	// Gap: 409 with the resume offset surfaced.
	rec = doUpload(t, h, http.MethodPut, "http://x/", "index", 100, []byte("zzz"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("gap: expected 409, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-Next-Offset"); got != "6" {
		t.Fatalf("gap: expected X-Next-Offset 6, got %q", got)
	}
}

func TestImportUpload_InvalidOffsetAndTooLarge(t *testing.T) {
	h := newUploadHandler(t, "imp")

	// Non-numeric X-Offset.
	req := httptest.NewRequest(http.MethodPost, "http://x/", bytes.NewReader([]byte("x")))
	req.Header.Set("X-Offset", "not-a-number")
	rec := httptest.NewRecorder()
	h.Handle(rec, req, "ns", "imp", "index")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid offset: expected 400, got %d", rec.Code)
	}

	// Body over the chunk limit.
	big := bytes.Repeat([]byte("a"), maxImportUploadChunkBytes+1)
	rec = doUpload(t, h, http.MethodPost, "http://x/", "index", 0, big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large: expected 413, got %d", rec.Code)
	}
}

func TestImportUpload_MethodAndSubresourceGuards(t *testing.T) {
	h := newUploadHandler(t, "imp")

	rec := doUpload(t, h, http.MethodGet, "http://x/", "index", -1, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: expected 405, got %d", rec.Code)
	}

	rec = doUpload(t, h, http.MethodHead, "http://x/", "bogus", -1, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bogus sub: expected 404, got %d", rec.Code)
	}
}

func TestImportUpload_MissingImport(t *testing.T) {
	h := newUploadHandler(t, "") // no SnapshotImport created
	rec := doUpload(t, h, http.MethodPost, "http://x/", "index", 0, []byte("abc"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing import: expected 404, got %d", rec.Code)
	}
}

func TestImportUpload_IndexFinalize(t *testing.T) {
	h := newUploadHandler(t, "imp")
	idx := restore.Index{
		Version:      restore.IndexVersion,
		RootSnapshot: restore.IndexSnapshotID{ID: "Snapshot--ns--snap"},
		Snapshots:    []restore.IndexSnapshot{{ID: "Snapshot--ns--snap"}},
	}
	raw, _ := json.Marshal(idx)

	rec := doUpload(t, h, http.MethodPost, "http://x/?finalize=true", "index", 0, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("index finalize: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	assertCondition(t, h, storagev1alpha1.SnapshotImportConditionIndexReceived, true)
}

func TestImportUpload_IndexFinalizeRejectsBadJSON(t *testing.T) {
	h := newUploadHandler(t, "imp")
	rec := doUpload(t, h, http.MethodPost, "http://x/?finalize=true", "index", 0, []byte("not json"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad index finalize: expected 400, got %d", rec.Code)
	}
	assertCondition(t, h, storagev1alpha1.SnapshotImportConditionIndexReceived, false)
}

func TestImportUpload_ManifestsPerNodeVsWholeTree(t *testing.T) {
	h := newUploadHandler(t, "imp")

	// Per-node finalize validates the array but does NOT flip the global condition.
	rec := doUpload(t, h, http.MethodPost, "http://x/?node=Snapshot--ns--snap&finalize=true", "manifests", 0, []byte("[]"))
	if rec.Code != http.StatusOK {
		t.Fatalf("per-node finalize: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	assertCondition(t, h, storagev1alpha1.SnapshotImportConditionManifestsReceived, false)

	// Whole-tree finalize (no node) flips ManifestsReceived.
	rec = doUpload(t, h, http.MethodPost, "http://x/?finalize=true", "manifests", 0, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("whole-tree finalize: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	assertCondition(t, h, storagev1alpha1.SnapshotImportConditionManifestsReceived, true)
}

func TestImportUpload_PerNodeFinalizeRejectsNonArray(t *testing.T) {
	h := newUploadHandler(t, "imp")
	rec := doUpload(t, h, http.MethodPost, "http://x/?node=n1&finalize=true", "manifests", 0, []byte(`{"x":1}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("per-node bad finalize: expected 400, got %d", rec.Code)
	}
}

func assertCondition(t *testing.T, h *ImportUploadHandler, condType string, want bool) {
	t.Helper()
	imp := &storagev1alpha1.SnapshotImport{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "imp"}, imp); err != nil {
		t.Fatalf("get import: %v", err)
	}
	got := meta.IsStatusConditionTrue(imp.Status.Conditions, condType)
	if got != want {
		t.Fatalf("condition %s: want %v, got %v (conditions=%v)", condType, want, got, imp.Status.Conditions)
	}
}
