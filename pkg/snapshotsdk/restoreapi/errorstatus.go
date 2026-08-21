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
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/transform"
)

// HTTPStatusForError maps a restore failure to the HTTP status and Kubernetes reason a client sees.
// An error that matches nothing here is an internal fault (500) — the deliberate default, because a
// failure nobody classified must not be reported as a client mistake.
//
// The mapping is the reason the restore vocabulary is shared: a client distinguishes "not there" (404)
// from "not ready yet, retry" (409) from "you asked wrongly" (400) by status alone, and it must get the
// same answer whichever aggregated group compiled the node.
func HTTPStatusForError(err error) (status int, reason string) {
	switch {
	case errors.Is(err, transform.ErrBadRequest):
		return http.StatusBadRequest, "BadRequest"
	case errors.Is(err, transform.ErrNotFound) || apierrors.IsNotFound(err):
		return http.StatusNotFound, "NotFound"
	case apierrors.IsForbidden(err):
		// A resolver returns apierrors.NewForbidden when a bound SnapshotContent's spec.snapshotRef
		// does not point back at the addressed snapshot subject (anti-spoofing).
		return http.StatusForbidden, "Forbidden"
	case errors.Is(err, transform.ErrNotReady) || errors.Is(err, transform.ErrContractViolation):
		return http.StatusConflict, "Conflict"
	default:
		return http.StatusInternalServerError, "InternalError"
	}
}

// WriteError answers a failed manifests-with-data-restoration request with the Kubernetes Status object
// the mapping selects. The error's own message is passed through verbatim: it names the subject that
// failed, which is the only diagnostic a client of an aggregated subresource gets.
func WriteError(w http.ResponseWriter, err error) {
	status, reason := HTTPStatusForError(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(metav1.Status{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Status",
			APIVersion: "v1",
		},
		Status:  metav1.StatusFailure,
		Message: err.Error(),
		Reason:  metav1.StatusReason(reason),
		Code:    int32(status),
	})
}
