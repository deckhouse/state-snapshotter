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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// ReasonImportContentNotBound is the canonical status.reason of the 409 the NAMESPACED upload
// (manifests-and-children-refs-upload) returns while the addressed import CR has no
// status.boundSnapshotContentName yet (bind-first, ADR "HTTP / subresource API"). It is a transport
// "wait for the binder" signal, NOT a Ready-condition reason (d8 waits/retries on it). Code, ADR, and
// clients (d8, domain facade) MUST match this string verbatim. The cluster-scoped
// snapshotcontents/<name>/manifests-upload layer has NO bind-gate: a missing content is a 404 addressing
// error, never ImportContentNotBound.
const ReasonImportContentNotBound = "ImportContentNotBound"

// ManifestsAndChildrenUpload is the NAMESPACED per-CR import payload (POST
// manifests-and-children-refs-upload): one node's own manifests plus the list of its DIRECT children
// (refs, not the child objects). There is NO volume descriptor — volume parameters are read from the
// manifests in MCP on import. The upload is a single atomic request per node: not resumable, no
// ?node=/?finalize and no completeness marker.
type ManifestsAndChildrenUpload struct {
	// Manifests is a JSON array of raw objects (this node's own manifests, the same shape returned by
	// manifests-download). Stored verbatim in the reconstructed ManifestCheckpoint.
	Manifests json.RawMessage `json:"manifests"`

	// ChildRefs lists the direct children of this node as namespaced snapshot refs (apiVersion/kind/name;
	// child namespace is implicit = this CR's namespace). May be empty for a leaf.
	ChildRefs []UploadChildRef `json:"childRefs"`
}

// ManifestsUpload is the CLUSTER-SCOPED content-addressed import payload (POST
// snapshotcontents/<name>/manifests-upload): only the node's own manifests. childRefs are a
// namespaced-layer attribute (written on the owning CR's status) and are deliberately absent here — the
// domain upload facade writes them on its own CR and forwards ONLY manifests to this layer.
type ManifestsUpload struct {
	// Manifests is a JSON array of raw objects (same shape as the namespaced layer's manifests).
	Manifests json.RawMessage `json:"manifests"`
}

// UploadChildRef is one direct-child reference in an import payload (Kubernetes-style ref, name-only
// namespace-implicit), mirroring storagev1alpha1.SnapshotChildRef.
type UploadChildRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// ImportUploadService persists import uploads across two layers (content-single-writer design;
// ADR "HTTP / subresource API"):
//
//   - NAMESPACED, user-facing (Upload): validates the target CR (import mode), records the DIRECT children
//     on status.childrenSnapshotRefs, then enforces bind-first — an unbound CR (empty
//     status.boundSnapshotContentName) is refused with 409 ImportContentNotBound. Once bound it resolves
//     the SnapshotContent, checks the anti-spoofing back-reference, and forwards ONLY the manifests to the
//     content-addressed layer.
//   - CLUSTER-SCOPED, internal (UploadToContent): given a SnapshotContent (which exists by definition —
//     cluster-scoped addressing) it reconstructs the node's raw ManifestCheckpoint + chunks owned by that
//     content FROM BIRTH (bind-first removed the ownerless-MCP window, so there is no more ObjectKeeper
//     backstop). It is manifests-only; childRefs never reach this layer.
//
// The reconstructed MCP name stays deterministic from the owning snapshot UID
// (ReconstructedManifestCheckpointName), so the aggregator's status.manifestCheckpointName projection is
// unchanged. Errors are AggregatedStatusError so the HTTP layer maps them to Kubernetes Status responses.
type ImportUploadService struct {
	client client.Client
}

// NewImportUploadService creates an ImportUploadService backed by a writing client.
func NewImportUploadService(c client.Client) *ImportUploadService {
	return &ImportUploadService{client: c}
}

// Upload is the NAMESPACED layer: it persists one node's direct-children refs for the snapshot CR
// identified by (snapshotGVK, namespace, name) and, once the CR is bound, forwards the manifests to the
// content-addressed layer. It returns the reconstructed ManifestCheckpoint name.
//
// leaf marks a leaf-only connector (the CSI VolumeSnapshot connector): a leaf declares no children, so a
// non-empty childRefs payload is rejected (400). Core Snapshots pass leaf=false.
//
// Ordering (mirrors the domain upload facade): validate -> write childRefs (idempotent, a snapshot-layer
// attribute the CR owner writes regardless of bind) -> bind-first gate (empty boundSnapshotContentName ->
// 409 ImportContentNotBound) -> resolve content + anti-spoofing back-ref (403 on mismatch/missing) ->
// forward manifests to UploadToContent.
func (s *ImportUploadService) Upload(ctx context.Context, snapshotGVK schema.GroupVersionKind, namespace, name string, body []byte, leaf bool) (string, error) {
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
	if leaf && len(payload.ChildRefs) > 0 {
		return "", NewAggregatedStatusError(http.StatusBadRequest, "BadRequest",
			fmt.Sprintf("%s %s/%s is a leaf: childRefs must be empty", snapshotGVK.String(), namespace, name))
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
			fmt.Sprintf("%s %s/%s is not in import mode (spec.mode is not Import): refusing manifests upload", snapshotGVK.String(), namespace, name))
	}
	uid := cr.GetUID()
	if uid == "" {
		return "", NewAggregatedStatusError(http.StatusConflict, "Conflict", "target snapshot has no UID yet")
	}

	// childRefs is a snapshot-layer attribute: the CR owner records it on its own status regardless of bind
	// (idempotent, skip-if-equal). A leaf has no children, so this is a no-op there.
	if err := s.writeChildrenSnapshotRefs(ctx, snapshotGVK, namespace, name, payload.ChildRefs); err != nil {
		return "", err
	}

	// Bind-first (ADR): the binder creates + binds the SnapshotContent independently of upload. Until it
	// has, refuse the manifests write with the canonical 409 so the client waits/retries. Content-slug
	// resolution and the MCP write are content-addressed (below); nothing is written pre-bind.
	boundContentName, _, berr := unstructured.NestedString(cr.Object, "status", "boundSnapshotContentName")
	if berr != nil {
		return "", NewAggregatedStatusError(http.StatusInternalServerError, "InternalError",
			fmt.Sprintf("%s %s/%s has invalid status.boundSnapshotContentName: %v", snapshotGVK.String(), namespace, name, berr))
	}
	if boundContentName == "" {
		return "", NewAggregatedStatusError(http.StatusConflict, ReasonImportContentNotBound,
			fmt.Sprintf("%s %s/%s is not bound to a SnapshotContent yet (status.boundSnapshotContentName is empty): waiting for the binder", snapshotGVK.String(), namespace, name))
	}

	// Anti-spoofing: status.boundSnapshotContentName is user-writable, so verify the resolved content points
	// back at THIS CR (spec.snapshotRef) before forwarding manifests into it (fail-closed 403). The
	// content-addressed layer has no addressed CR, so the back-ref lives here, not there.
	content := &storagev1alpha1.SnapshotContent{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: boundContentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			return "", NewAggregatedStatusError(http.StatusNotFound, "NotFound", fmt.Sprintf("SnapshotContent %q not found", boundContentName))
		}
		return "", fmt.Errorf("get SnapshotContent %q: %w", boundContentName, err)
	}
	if err := verifyContentSnapshotRef(content, snapshotGVK.GroupVersion().String(), snapshotGVK.Kind, namespace, name, string(uid)); err != nil {
		return "", err
	}

	return s.UploadToContent(ctx, content, manifests)
}

// UploadToContent is the CLUSTER-SCOPED, content-addressed layer: given a SnapshotContent (which exists by
// definition) it reconstructs the node's raw ManifestCheckpoint + chunks owned by that content from birth
// and returns the checkpoint name. manifests is a JSON array (this node's own manifests, no childRefs).
//
// The checkpoint name is derived from the content's spec.snapshotRef.uid so it matches the aggregator's
// ReconstructedManifestCheckpointName(owner.UID) projection. It is idempotent: an already-Ready checkpoint
// is left untouched.
func (s *ImportUploadService) UploadToContent(ctx context.Context, content *storagev1alpha1.SnapshotContent, manifests []byte) (string, error) {
	if content == nil {
		return "", NewAggregatedStatusError(http.StatusInternalServerError, "InternalError", "nil SnapshotContent")
	}
	validated, err := validateUploadManifests(manifests)
	if err != nil {
		return "", err
	}
	if content.Spec.SnapshotRef == nil || content.Spec.SnapshotRef.UID == "" {
		return "", NewAggregatedStatusError(http.StatusConflict, "Conflict",
			fmt.Sprintf("SnapshotContent %q has no spec.snapshotRef.uid to key the reconstructed ManifestCheckpoint", content.Name))
	}
	snapshotUID := content.Spec.SnapshotRef.UID

	// The MCP is born owned by the SnapshotContent (bind-first guarantees the content exists), so it is
	// GC-safe immediately — no ObjectKeeper backstop and no aggregator re-parent needed.
	controllerTrue := true
	contentOwnerRef := metav1.OwnerReference{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "SnapshotContent",
		Name:       content.Name,
		UID:        content.UID,
		Controller: &controllerTrue,
	}
	checkpointName := ReconstructedManifestCheckpointName(snapshotUID, "")
	if err := ReconstructManifestCheckpoint(ctx, s.client, checkpointName, []metav1.OwnerReference{contentOwnerRef}, validated); err != nil {
		return "", classifyReconstructError(err)
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

// uploadTargetIsImportMode reports whether a snapshot CR is an import target. Every snapshot kind the
// upload endpoint sees — our snapshot CRDs (core/structural nodes, domain data leaves) AND the extended
// CSI VolumeSnapshot fork — carries the same enum spec.mode: Import (the former spec.source.import: {}
// fork marker was replaced by spec.mode on the fork CRD). This keeps the upload endpoint from clobbering
// a live-capture snapshot.
func uploadTargetIsImportMode(obj *unstructured.Unstructured) bool {
	return IsUnstructuredImportMode(obj)
}

// IsUnstructuredImportMode reports whether an unstructured snapshot object is in import mode: the enum
// spec.mode: Import, uniform across ALL snapshot kinds (root Snapshot, domain XxxxSnapshot, and the
// extended CSI VolumeSnapshot fork, whose CRD now hosts the same top-level spec.mode). A live-capture
// snapshot carries mode: Capture (or omits it — the CRD default).
func IsUnstructuredImportMode(obj *unstructured.Unstructured) bool {
	mode, _, _ := unstructured.NestedString(obj.Object, "spec", "mode")
	return mode == string(storagev1alpha1.SnapshotModeImport)
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
