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

package restoreapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/transform"
)

// TestHTTPStatusForError covers every branch of the mapping, including the deliberate default: an
// unclassified failure is a server fault (500), never a client mistake. Wrapped errors are used
// throughout because that is how servers raise them — a mapping that only matched bare sentinels would
// answer 500 to every real failure.
func TestHTTPStatusForError(t *testing.T) {
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: "state-snapshotter.deckhouse.io", Resource: "snapshotcontents"}, "c",
		errors.New("bound SnapshotContent does not belong to snapshot ns/snap"),
	)
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantReason string
	}{
		{name: "bad request", err: fmt.Errorf("%w: unknown scope", transform.ErrBadRequest), wantStatus: http.StatusBadRequest, wantReason: "BadRequest"},
		{name: "not found", err: fmt.Errorf("%w: snapshot ns/snap", transform.ErrNotFound), wantStatus: http.StatusNotFound, wantReason: "NotFound"},
		{name: "apierrors not found", err: apierrors.NewNotFound(schema.GroupResource{Resource: "snapshots"}, "snap"), wantStatus: http.StatusNotFound, wantReason: "NotFound"},
		{name: "apierrors forbidden (anti-spoofing)", err: forbidden, wantStatus: http.StatusForbidden, wantReason: "Forbidden"},
		{name: "not ready", err: fmt.Errorf("%w: SnapshotContent c is not Ready", transform.ErrNotReady), wantStatus: http.StatusConflict, wantReason: "Conflict"},
		{name: "contract violation", err: fmt.Errorf("%w: duplicate object", transform.ErrContractViolation), wantStatus: http.StatusConflict, wantReason: "Conflict"},
		{name: "unclassified is a server fault", err: errors.New("connection refused"), wantStatus: http.StatusInternalServerError, wantReason: "InternalError"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, reason := HTTPStatusForError(tc.err)
			if status != tc.wantStatus || reason != tc.wantReason {
				t.Fatalf("HTTPStatusForError = %d/%q, want %d/%q", status, reason, tc.wantStatus, tc.wantReason)
			}
		})
	}
}

// TestWriteError_WritesKubernetesStatus pins the response body itself: a client of an aggregated
// subresource parses a metav1.Status, so the code inside the body must agree with the HTTP status and the
// message must survive verbatim — it is the only diagnostic the client gets.
func TestWriteError_WritesKubernetesStatus(t *testing.T) {
	rec := httptest.NewRecorder()

	WriteError(rec, fmt.Errorf("%w: snapshot ns1/snap", transform.ErrNotFound))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var status metav1.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("response body is not a Status: %v (%s)", err, rec.Body.String())
	}
	if status.Kind != "Status" || status.APIVersion != "v1" {
		t.Fatalf("body TypeMeta = %s/%s, want v1/Status", status.APIVersion, status.Kind)
	}
	if status.Status != metav1.StatusFailure {
		t.Fatalf("body status = %q, want %q", status.Status, metav1.StatusFailure)
	}
	if status.Reason != metav1.StatusReasonNotFound {
		t.Fatalf("body reason = %q, want NotFound", status.Reason)
	}
	if status.Code != int32(http.StatusNotFound) {
		t.Fatalf("body code = %d, want 404", status.Code)
	}
	if status.Message != "not found: snapshot ns1/snap" {
		t.Fatalf("body message = %q, want the wrapped error verbatim", status.Message)
	}
}
