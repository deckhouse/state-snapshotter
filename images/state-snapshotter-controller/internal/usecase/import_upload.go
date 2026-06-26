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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

// ManifestsAndChildrenUpload is the per-CR import payload (POST manifests-and-children-refs-upload):
// one node's own manifests plus the list of its DIRECT children (refs, not the child objects). There is
// NO volume descriptor — volume parameters are read from the manifests in MCP on import. The upload is a
// single atomic request per node: not resumable, no ?node=/?finalize and no completeness marker.
type ManifestsAndChildrenUpload struct {
	// Manifests is a JSON array of raw objects (this node's own manifests, the same shape returned by
	// manifests-download). Stored verbatim in the reconstructed ManifestCheckpoint.
	Manifests json.RawMessage `json:"manifests"`

	// ChildRefs lists the direct children of this node as namespaced snapshot refs (apiVersion/kind/name;
	// child namespace is implicit = this CR's namespace). May be empty for a leaf.
	ChildRefs []UploadChildRef `json:"childRefs"`
}

// UploadChildRef is one direct-child reference in an import payload (Kubernetes-style ref, name-only
// namespace-implicit), mirroring storagev1alpha1.SnapshotChildRef.
type UploadChildRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// ImportUploadService persists per-CR import uploads. It is the transport/persistence half of the import
// flow (C3): it validates the payload and the target CR (must be in import mode), reconstructs the node's
// raw ManifestCheckpoint from the uploaded manifests, and records the direct children on
// status.childrenSnapshotRefs. The import orchestrator (C5) consumes these — it materializes the
// SnapshotContent (manifestCheckpointName + childrenSnapshotContentRefs), resolves the data artifact
// (DataImport), attaches ownerRefs/GC, and writes the binding.
type ImportUploadService struct {
	client client.Client
}

// NewImportUploadService creates an ImportUploadService backed by a writing client.
func NewImportUploadService(c client.Client) *ImportUploadService {
	return &ImportUploadService{client: c}
}

// Upload persists one node's manifests + direct-children refs for the snapshot CR identified by
// (snapshotGVK, namespace, name). It returns the reconstructed ManifestCheckpoint name. Errors are
// AggregatedStatusError so the HTTP layer maps them to Kubernetes Status responses.
func (s *ImportUploadService) Upload(ctx context.Context, snapshotGVK schema.GroupVersionKind, namespace, name string, body []byte) (string, error) {
	if snapshotGVK.Empty() || namespace == "" || name == "" {
		return "", NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", "snapshot GVK, namespace, and name are required")
	}

	var payload ManifestsAndChildrenUpload
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", fmt.Sprintf("invalid upload payload: %v", err))
	}
	manifests, err := validateUploadManifests(payload.Manifests)
	if err != nil {
		return "", err
	}
	if err := validateUploadChildRefs(payload.ChildRefs); err != nil {
		return "", err
	}

	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(snapshotGVK)
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cr); err != nil {
		if apierrors.IsNotFound(err) {
			return "", NewAggregatedStatusError(http.StatusNotFound, "NotFound", fmt.Sprintf("%s %s/%s not found", snapshotGVK.String(), namespace, name))
		}
		return "", fmt.Errorf("get %s %s/%s: %w", snapshotGVK.String(), namespace, name, err)
	}
	if !uploadTargetIsImportMode(cr) {
		return "", NewAggregatedStatusError(http.StatusConflict, "Conflict",
			fmt.Sprintf("%s %s/%s is not in import mode (no spec.source.import / spec.source.dataImportName / spec.dataSource): refusing manifests upload", snapshotGVK.String(), namespace, name))
	}
	uid := cr.GetUID()
	if uid == "" {
		return "", NewAggregatedStatusError(http.StatusConflict, "Conflict", "target snapshot has no UID yet")
	}

	// Reconstruct the node's raw ManifestCheckpoint (idempotent, deterministic name keyed to the CR UID).
	// ownerRefs are intentionally nil here: the ManifestCheckpoint is cluster-scoped and cannot be owned
	// by the namespaced snapshot CR; the import orchestrator (C5) attaches it to the SnapshotContent it
	// materializes, which is the durable GC owner. captureRef is a synthetic back-reference to the CR
	// (ManifestCheckpointSpec requires one); it is not a real ManifestCaptureRequest on the import path.
	checkpointName := ReconstructedManifestCheckpointName(uid, "")
	captureRef := &ssv1alpha1.ObjectReference{Name: name, Namespace: namespace, UID: string(uid)}
	if err := ReconstructManifestCheckpoint(ctx, s.client, checkpointName, namespace, captureRef, nil, manifests); err != nil {
		return "", classifyReconstructError(err)
	}

	if err := s.writeChildrenSnapshotRefs(ctx, snapshotGVK, namespace, name, payload.ChildRefs); err != nil {
		return "", err
	}
	return checkpointName, nil
}

// writeChildrenSnapshotRefs sets status.childrenSnapshotRefs on the import CR from the uploaded child
// list (namespaced snapshot refs, used by export and recursive restore to walk the imported tree). The
// content-graph counterpart (SnapshotContent.status.childrenSnapshotContentRefs) is written by the
// import orchestrator (C5) once the child SnapshotContents are materialized.
func (s *ImportUploadService) writeChildrenSnapshotRefs(ctx context.Context, snapshotGVK schema.GroupVersionKind, namespace, name string, childRefs []UploadChildRef) error {
	refs := make([]interface{}, 0, len(childRefs))
	for _, ref := range childRefs {
		refs = append(refs, map[string]interface{}{
			"apiVersion": ref.APIVersion,
			"kind":       ref.Kind,
			"name":       ref.Name,
		})
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(snapshotGVK)
		if err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, fresh); err != nil {
			return err
		}
		// Skip the status write when the children are already recorded as desired. This makes the upload
		// a true no-op on retries and, for a leaf (childRefs empty), avoids touching status at all: a leaf
		// has no children to record, so a blind Status().Update only races whoever owns the CR's status.
		// The CSI VolumeSnapshot leaf is the concrete case — its forked status schema has no
		// childrenSnapshotRefs field (the write is pruned anyway), while the state-snapshotter binder
		// actively reconciles the import VolumeSnapshot's status; a blind write lost the conflict race for
		// the whole retry budget and the upload returned 409 (see genericbinder/import.go). The graph
		// consumers treat an absent list as "no children", so skipping the empty write is behavior-neutral.
		current, _, err := unstructured.NestedSlice(fresh.Object, "status", "childrenSnapshotRefs")
		if err != nil {
			return err
		}
		if childRefSlicesEqual(current, refs) {
			return nil
		}
		if err := unstructured.SetNestedSlice(fresh.Object, refs, "status", "childrenSnapshotRefs"); err != nil {
			return err
		}
		return s.client.Status().Update(ctx, fresh)
	})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return NewAggregatedStatusError(http.StatusNotFound, "NotFound", fmt.Sprintf("%s %s/%s not found", snapshotGVK.String(), namespace, name))
		}
		if apierrors.IsConflict(err) {
			return NewAggregatedStatusError(http.StatusConflict, "Conflict",
				fmt.Sprintf("conflict writing status.childrenSnapshotRefs on %s %s/%s: %v", snapshotGVK.String(), namespace, name, err))
		}
		return fmt.Errorf("write status.childrenSnapshotRefs on %s %s/%s: %w", snapshotGVK.String(), namespace, name, err)
	}
	return nil
}

// childRefSlicesEqual reports whether the recorded status.childrenSnapshotRefs already match the desired
// refs. An absent field (nil) and an empty list are treated as equal (both mean "no children"), so a leaf
// upload neither writes nor races the CR's status owner when there is nothing to record.
func childRefSlicesEqual(current, desired []interface{}) bool {
	if len(current) == 0 && len(desired) == 0 {
		return true
	}
	return reflect.DeepEqual(current, desired)
}

// uploadTargetIsImportMode reports whether a snapshot CR is an import target. It accepts any of the
// import markers across snapshot kinds:
//   - spec.source.import        — core/structural Snapshot nodes
//   - spec.source.dataImportName — generic-PVC extended VolumeSnapshot
//   - spec.dataSource           — domain data leaves
//   - spec.import               — domain aggregator import marker (manifest-only, no data leg)
//
// This keeps the upload endpoint from clobbering a live-capture snapshot.
func uploadTargetIsImportMode(obj *unstructured.Unstructured) bool {
	if _, found, _ := unstructured.NestedMap(obj.Object, "spec", "source", "import"); found {
		return true
	}
	if v, found, _ := unstructured.NestedString(obj.Object, "spec", "source", "dataImportName"); found && v != "" {
		return true
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "dataSource"); found {
		return true
	}
	// Domain aggregator import marker: presence of spec.import signals manifest-only import mode.
	if _, found, _ := unstructured.NestedMap(obj.Object, "spec", "import"); found {
		return true
	}
	return false
}

// validateUploadManifests checks that manifests is present and is a JSON array (rejecting absent/null and
// non-array values, e.g. an object), returning the original bytes for reconstruction.
func validateUploadManifests(manifests json.RawMessage) ([]byte, error) {
	trimmed := bytes.TrimSpace(manifests)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", "manifests is required")
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(trimmed, &arr); err != nil {
		return nil, NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", fmt.Sprintf("manifests must be a JSON array: %v", err))
	}
	return manifests, nil
}

// validateUploadChildRefs rejects child refs missing apiVersion/kind/name.
func validateUploadChildRefs(childRefs []UploadChildRef) error {
	for i, ref := range childRefs {
		if ref.APIVersion == "" || ref.Kind == "" || ref.Name == "" {
			return NewAggregatedStatusError(http.StatusBadRequest, "BadRequest",
				fmt.Sprintf("childRefs[%d] must set apiVersion, kind, and name", i))
		}
	}
	return nil
}

// classifyReconstructError maps a malformed-manifests reconstruction error to 400 and everything else to 500.
func classifyReconstructError(err error) error {
	if strings.Contains(err.Error(), "not a JSON array") {
		return NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", err.Error())
	}
	return NewAggregatedStatusError(http.StatusInternalServerError, "InternalError", err.Error())
}
