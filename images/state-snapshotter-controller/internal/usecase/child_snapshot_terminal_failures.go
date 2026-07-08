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

// Child Snapshot terminal-failure classification from status.childrenSnapshotRefs.
// Each ref carries explicit apiVersion/kind/name; the child object is loaded with a single Get (no registry scan).
//
// This is NOT a Ready aggregator. Final readiness is owned by SnapshotContent
// (Ready = ManifestsReady && VolumeReady && ChildrenReady) and Snapshot.Ready mirrors the bound SnapshotContent.Ready.
// The helpers here serve two narrow purposes:
//   - the priority-wave barrier (parent_graph.go), which must detect terminal child failures;
//   - the single Snapshot.Ready bridge exception for child-Snapshot capture failures that no
//     SnapshotContent can yet represent (SummarizeChildSnapshotTerminalFailures).

package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// ChildSnapshotTerminalReadyReasons lists child snapshot Ready=False reasons treated as terminal capture
// failure. Extend only with N2a-equivalent terminal paths shared across snapshot kinds.
//
// This set MUST stay in sync with the exhaustive terminal capture-failure reasons documented in
// ready_patch.go (the pre-bind/pre-publish failCapture writers). In particular the volume-capture
// terminal reason (VolumeCaptureFailed) and DuplicateCoveredPVCUID are carried by a child Snapshot's
// Ready=False before any child SnapshotContent can represent them, so the child-Snapshot
// terminal-failure bridge (SummarizeChildSnapshotTerminalFailures) must classify them as Failed —
// otherwise a domain child whose volume capture failed is misclassified as Pending and the parent
// holds a stale Ready=True over lost data (INV-FAIL-PROP).
var ChildSnapshotTerminalReadyReasons = map[string]struct{}{
	"ListFailed":                       {},
	"ManifestCheckpointFailed":         {},
	"NamespaceNotFound":                {},
	snapshot.ReasonVolumeCaptureFailed: {}, // "VolumeCaptureFailed"
	"DuplicateCoveredPVCUID":           {},
}

// SnapshotChildReadyClass is the classification of one resolved child snapshot object.
type SnapshotChildReadyClass int

const (
	// SnapshotChildReadyClassCompleted — child bound and Ready=True.
	SnapshotChildReadyClassCompleted SnapshotChildReadyClass = iota
	// SnapshotChildReadyClassPending — not bound, no Ready, Ready=False non-terminal, or Unknown.
	SnapshotChildReadyClassPending
	// SnapshotChildReadyClassFailed — Ready=False with terminal reason, or invalid ref fields.
	SnapshotChildReadyClassFailed
)

// IsSnapshotChildTerminalReadyFailure reports whether a child Ready=False reason is terminal for parent aggregation.
func IsSnapshotChildTerminalReadyFailure(reason string) bool {
	_, ok := ChildSnapshotTerminalReadyReasons[reason]
	return ok
}

// readyConditionFromSnapshotUnstructured reads the Ready status condition from a snapshot object's unstructured status.
// It unmarshals the whole status map via JSON so conditions survive listType=map / typed JSON shapes that do not
// always round-trip as []interface{} in unstructured.NestedSlice.
func readyConditionFromSnapshotUnstructured(u *unstructured.Unstructured) *metav1.Condition {
	if u == nil {
		return nil
	}
	st, found, err := unstructured.NestedMap(u.Object, "status")
	if !found || err != nil || len(st) == 0 {
		return nil
	}
	b, err := json.Marshal(st)
	if err != nil {
		return nil
	}
	var parsed struct {
		Conditions []metav1.Condition `json:"conditions,omitempty"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil
	}
	for i := range parsed.Conditions {
		if parsed.Conditions[i].Type == snapshot.ConditionReady {
			c := parsed.Conditions[i]
			return &c
		}
	}
	return nil
}

// CurrentReadyCondition returns the Ready status condition from a snapshot's unstructured status, or
// nil if absent. It uses the same JSON-based read as classification so listType=map / typed JSON
// shapes round-trip. Callers that need a strict (current-generation) terminal decision compare the
// returned condition's ObservedGeneration to the object's metadata.generation themselves.
func CurrentReadyCondition(u *unstructured.Unstructured) *metav1.Condition {
	return readyConditionFromSnapshotUnstructured(u)
}

// ClassifyGenericChildSnapshotReady classifies one resolved child snapshot (unstructured + GVK).
func ClassifyGenericChildSnapshotReady(u *unstructured.Unstructured, gvk schema.GroupVersionKind, childNS, childName string) (SnapshotChildReadyClass, string) {
	childKey := fmt.Sprintf("%s/%s/%s", gvk.String(), childNS, childName)
	bound, foundBound, err := unstructured.NestedString(u.Object, "status", "boundSnapshotContentName")
	if err != nil || !foundBound || bound == "" {
		return SnapshotChildReadyClassPending,
			fmt.Sprintf("waiting for child snapshot %s to bind snapshot content", childKey)
	}
	rc := readyConditionFromSnapshotUnstructured(u)
	if rc == nil {
		return SnapshotChildReadyClassPending,
			fmt.Sprintf("waiting for child snapshot %s Ready condition", childKey)
	}
	switch rc.Status {
	case metav1.ConditionTrue:
		return SnapshotChildReadyClassCompleted, ""
	case metav1.ConditionFalse:
		if IsSnapshotChildTerminalReadyFailure(rc.Reason) {
			return SnapshotChildReadyClassFailed,
				fmt.Sprintf("child snapshot %s failed: reason=%s message=%s", childKey, rc.Reason, rc.Message)
		}
		if rc.Message != "" {
			return SnapshotChildReadyClassPending,
				fmt.Sprintf("waiting for child snapshot %s Ready=True: child reason=%s, message=%s", childKey, rc.Reason, rc.Message)
		}
		return SnapshotChildReadyClassPending,
			fmt.Sprintf("waiting for child snapshot %s Ready=True: child reason=%s", childKey, rc.Reason)
	default:
		msg := fmt.Sprintf("waiting for child snapshot %s Ready (child Ready status Unknown)", childKey)
		if rc.Message != "" {
			msg = fmt.Sprintf("%s: child message=%s", msg, rc.Message)
		}
		return SnapshotChildReadyClassPending, msg
	}
}

// ClassifySnapshotChildReady maps a typed Snapshot to the same class as generic resolution
// (typed storage Snapshot path; same status shape as other snapshot kinds).
func ClassifySnapshotChildReady(ch *storagev1alpha1.Snapshot) (SnapshotChildReadyClass, string) {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ch)
	if err != nil {
		return SnapshotChildReadyClassFailed, fmt.Sprintf("internal: convert Snapshot: %v", err)
	}
	u := &unstructured.Unstructured{Object: raw}
	gvk := storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot")
	return ClassifyGenericChildSnapshotReady(u, gvk, ch.Namespace, ch.Name)
}

// ChildSnapshotTerminalFailures is the narrow input for the Snapshot.Ready child-capture-failure
// bridge: terminal child-Snapshot capture failures discovered from status.childrenSnapshotRefs.
// It carries no pending/Completed aggregation — those states are reflected through the
// SnapshotContent.Ready mirror, not here.
type ChildSnapshotTerminalFailures struct {
	HasFailed bool
	Messages  []string
}

// SummarizeChildSnapshotTerminalFailures scans status.childrenSnapshotRefs (strict
// apiVersion/kind/name refs) and reports only terminal child-Snapshot capture failures: a child
// Ready=False with a terminal reason, or an invalid ref. Pending children (including not-found-yet)
// and Completed children are ignored — they are reflected through the SnapshotContent.Ready mirror.
func SummarizeChildSnapshotTerminalFailures(ctx context.Context, c client.Reader, refs []storagev1alpha1.SnapshotChildRef, parentSnapshotNamespace string) (ChildSnapshotTerminalFailures, error) {
	var out ChildSnapshotTerminalFailures
	for _, ref := range refs {
		if _, err := RefGVK(ref); err != nil {
			out.HasFailed = true
			out.Messages = append(out.Messages, err.Error())
			continue
		}
		u, gvk, resErr := GetChildSnapshot(ctx, c, ref, parentSnapshotNamespace)
		if resErr != nil {
			if errors.Is(resErr, ErrRunGraphChildSnapshotNotFound) {
				continue // not found yet is pending, not a terminal failure
			}
			return out, resErr
		}
		if cls, msg := ClassifyGenericChildSnapshotReady(u, gvk, parentSnapshotNamespace, ref.Name); cls == SnapshotChildReadyClassFailed {
			out.HasFailed = true
			if msg != "" {
				out.Messages = append(out.Messages, msg)
			}
		}
	}
	return out, nil
}

// JoinNonEmpty joins non-empty strings with sep (helper for parent-facing messages).
func JoinNonEmpty(parts []string, sep string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}
