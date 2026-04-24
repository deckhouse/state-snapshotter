/*
Copyright 2025 Flant JSC

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

// E6: generic NamespaceSnapshot parent readiness aggregation from status.childrenSnapshotRefs.
// Each ref carries explicit apiVersion/kind/name; the child object is loaded with a single Get (no registry scan).

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
// failure for parent aggregation (E6). Extend only with N2a-equivalent terminal paths shared across snapshot kinds.
var ChildSnapshotTerminalReadyReasons = map[string]struct{}{
	"ListFailed":               {},
	"NoCaptureTargets":         {},
	"CapturePlanDrift":         {},
	"ManifestCheckpointFailed": {},
	"ContentRefMismatch":       {},
	"NamespaceNotFound":        {},
}

// NamespaceSnapshotChildReadyClass is the E6 classification of one resolved child snapshot object.
type NamespaceSnapshotChildReadyClass int

const (
	// NamespaceSnapshotChildReadyClassCompleted — child bound and Ready=True.
	NamespaceSnapshotChildReadyClassCompleted NamespaceSnapshotChildReadyClass = iota
	// NamespaceSnapshotChildReadyClassPending — not bound, no Ready, Ready=False non-terminal, or Unknown.
	NamespaceSnapshotChildReadyClassPending
	// NamespaceSnapshotChildReadyClassFailed — Ready=False with terminal reason, or invalid ref fields.
	NamespaceSnapshotChildReadyClassFailed
)

// IsNamespaceSnapshotChildTerminalReadyFailure reports whether a child Ready=False reason is terminal for parent aggregation.
func IsNamespaceSnapshotChildTerminalReadyFailure(reason string) bool {
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

// ClassifyGenericChildSnapshotReady classifies one resolved child snapshot (unstructured + GVK) for parent E6 aggregation.
func ClassifyGenericChildSnapshotReady(u *unstructured.Unstructured, gvk schema.GroupVersionKind, childNS, childName string) (NamespaceSnapshotChildReadyClass, string) {
	childKey := fmt.Sprintf("%s/%s/%s", gvk.String(), childNS, childName)
	bound, foundBound, err := unstructured.NestedString(u.Object, "status", "boundSnapshotContentName")
	if err != nil || !foundBound || bound == "" {
		return NamespaceSnapshotChildReadyClassPending,
			fmt.Sprintf("waiting for child snapshot %s to bind snapshot content", childKey)
	}
	rc := readyConditionFromSnapshotUnstructured(u)
	if rc == nil {
		return NamespaceSnapshotChildReadyClassPending,
			fmt.Sprintf("waiting for child snapshot %s Ready condition", childKey)
	}
	switch rc.Status {
	case metav1.ConditionTrue:
		return NamespaceSnapshotChildReadyClassCompleted, ""
	case metav1.ConditionFalse:
		if IsNamespaceSnapshotChildTerminalReadyFailure(rc.Reason) {
			return NamespaceSnapshotChildReadyClassFailed,
				fmt.Sprintf("child snapshot %s failed: reason=%s message=%s", childKey, rc.Reason, rc.Message)
		}
		if rc.Message != "" {
			return NamespaceSnapshotChildReadyClassPending,
				fmt.Sprintf("waiting for child snapshot %s Ready=True: child reason=%s, message=%s", childKey, rc.Reason, rc.Message)
		}
		return NamespaceSnapshotChildReadyClassPending,
			fmt.Sprintf("waiting for child snapshot %s Ready=True: child reason=%s", childKey, rc.Reason)
	default:
		msg := fmt.Sprintf("waiting for child snapshot %s Ready (child Ready status Unknown)", childKey)
		if rc.Message != "" {
			msg = fmt.Sprintf("%s: child message=%s", msg, rc.Message)
		}
		return NamespaceSnapshotChildReadyClassPending, msg
	}
}

// ClassifyNamespaceSnapshotChildReady maps a typed NamespaceSnapshot to the same E6 class as generic resolution
// (typed storage NamespaceSnapshot path; same status shape as other snapshot kinds).
func ClassifyNamespaceSnapshotChildReady(ch *storagev1alpha1.NamespaceSnapshot) (NamespaceSnapshotChildReadyClass, string) {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ch)
	if err != nil {
		return NamespaceSnapshotChildReadyClassFailed, fmt.Sprintf("internal: convert NamespaceSnapshot: %v", err)
	}
	u := &unstructured.Unstructured{Object: raw}
	gvk := storagev1alpha1.SchemeGroupVersion.WithKind("NamespaceSnapshot")
	return ClassifyGenericChildSnapshotReady(u, gvk, ch.Namespace, ch.Name)
}

// NamespaceSnapshotChildrenRefsSummary aggregates E6 state across status.childrenSnapshotRefs.
type NamespaceSnapshotChildrenRefsSummary struct {
	HasFailed      bool
	FailedMessages []string
	HasPending     bool
	PendingParts   []string
	AllCompleted   bool
}

// SummarizeChildrenSnapshotRefsForParentReadyE6 aggregates parent child readiness from strict refs (apiVersion/kind/name).
func SummarizeChildrenSnapshotRefsForParentReadyE6(ctx context.Context, c client.Reader, refs []storagev1alpha1.NamespaceSnapshotChildRef, parentSnapshotNamespace string) (*NamespaceSnapshotChildrenRefsSummary, error) {
	if len(refs) == 0 {
		return &NamespaceSnapshotChildrenRefsSummary{AllCompleted: true}, nil
	}
	var sum NamespaceSnapshotChildrenRefsSummary
	for _, ref := range refs {
		if _, err := RefGVK(ref); err != nil {
			sum.HasFailed = true
			sum.FailedMessages = append(sum.FailedMessages, err.Error())
			continue
		}
		u, gvk, resErr := GetChildSnapshot(ctx, c, ref, parentSnapshotNamespace)
		if resErr != nil {
			if errors.Is(resErr, ErrRunGraphChildSnapshotNotFound) {
				sum.HasPending = true
				sum.PendingParts = append(sum.PendingParts,
					fmt.Sprintf("child snapshot %s/%s/%s not found yet", ref.APIVersion, ref.Kind, parentSnapshotNamespace+"/"+ref.Name))
				continue
			}
			return nil, resErr
		}
		cls, msg := ClassifyGenericChildSnapshotReady(u, gvk, parentSnapshotNamespace, ref.Name)
		switch cls {
		case NamespaceSnapshotChildReadyClassFailed:
			sum.HasFailed = true
			if msg != "" {
				sum.FailedMessages = append(sum.FailedMessages, msg)
			}
		case NamespaceSnapshotChildReadyClassPending:
			sum.HasPending = true
			if msg != "" {
				sum.PendingParts = append(sum.PendingParts, msg)
			}
		case NamespaceSnapshotChildReadyClassCompleted:
			// ok
		}
	}
	if sum.HasFailed {
		sum.AllCompleted = false
		return &sum, nil
	}
	if sum.HasPending {
		sum.AllCompleted = false
		return &sum, nil
	}
	sum.AllCompleted = true
	return &sum, nil
}

// E6ParentReadyPickInput is the generic parent Ready decision for NamespaceSnapshot (priority matrix).
type E6ParentReadyPickInput struct {
	HasChildFailed                bool
	ChildFailedMessage            string
	SubtreeManifestCapturePending bool
	SubtreeMessage                string
	HasChildPending               bool
	ChildPendingMessage           string
	SelfCaptureComplete           bool
}

// E6ParentReadyPickOutput is the parent Ready condition after applying E6 priority.
type E6ParentReadyPickOutput struct {
	Ready   bool
	Reason  string
	Message string
}

// PickParentReadyReasonE6 applies strict priority:
// ChildSnapshotFailed > SubtreeManifestCapturePending > ChildSnapshotPending > Completed
// (Completed only if SelfCaptureComplete and no higher-priority issue).
func PickParentReadyReasonE6(in E6ParentReadyPickInput) E6ParentReadyPickOutput {
	if in.HasChildFailed {
		msg := in.ChildFailedMessage
		if msg == "" {
			msg = "one or more child snapshots failed"
		}
		return E6ParentReadyPickOutput{
			Ready: false, Reason: snapshot.ReasonChildSnapshotFailed, Message: msg,
		}
	}
	if in.SubtreeManifestCapturePending {
		msg := in.SubtreeMessage
		if msg == "" {
			msg = "subtree manifest capture pending"
		}
		return E6ParentReadyPickOutput{
			Ready: false, Reason: snapshot.ReasonSubtreeManifestCapturePending, Message: msg,
		}
	}
	if in.HasChildPending {
		msg := in.ChildPendingMessage
		if msg == "" {
			msg = "waiting for child snapshots"
		}
		return E6ParentReadyPickOutput{
			Ready: false, Reason: snapshot.ReasonChildSnapshotPending, Message: msg,
		}
	}
	if in.SelfCaptureComplete {
		return E6ParentReadyPickOutput{
			Ready: true, Reason: snapshot.ReasonCompleted, Message: "all required child snapshots are ready",
		}
	}
	return E6ParentReadyPickOutput{
		Ready: false, Reason: snapshot.ReasonChildSnapshotPending, Message: "waiting for root manifest capture to complete",
	}
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
