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

// E6: generic NamespaceSnapshot parent readiness aggregation from child NamespaceSnapshot refs
// (typed storage API only — no domain CRD imports).

package usecase

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// NamespaceSnapshotChildTerminalReadyReasons lists child NamespaceSnapshot Ready=False reasons treated as
// terminal capture failure for parent aggregation (E6). Extend only with N2a terminal paths.
var NamespaceSnapshotChildTerminalReadyReasons = map[string]struct{}{
	"ListFailed":               {},
	"NoCaptureTargets":         {},
	"CapturePlanDrift":         {},
	"ManifestCheckpointFailed": {},
	"ContentRefMismatch":       {},
	"NamespaceNotFound":        {},
}

// NamespaceSnapshotChildReadyClass is the E6 classification of one child NamespaceSnapshot.
type NamespaceSnapshotChildReadyClass int

const (
	// NamespaceSnapshotChildReadyClassCompleted — child bound and Ready=True.
	NamespaceSnapshotChildReadyClassCompleted NamespaceSnapshotChildReadyClass = iota
	// NamespaceSnapshotChildReadyClassPending — child not bound, no Ready, Ready=False non-terminal, or Unknown.
	NamespaceSnapshotChildReadyClassPending
	// NamespaceSnapshotChildReadyClassFailed — child missing, or Ready=False with terminal reason.
	NamespaceSnapshotChildReadyClassFailed
)

// IsNamespaceSnapshotChildTerminalReadyFailure reports whether a child Ready=False reason is terminal for parent aggregation.
func IsNamespaceSnapshotChildTerminalReadyFailure(reason string) bool {
	_, ok := NamespaceSnapshotChildTerminalReadyReasons[reason]
	return ok
}

// ClassifyNamespaceSnapshotChildReady maps one child NamespaceSnapshot to E6 class and a human message (parent-facing detail).
func ClassifyNamespaceSnapshotChildReady(ch *storagev1alpha1.NamespaceSnapshot) (NamespaceSnapshotChildReadyClass, string) {
	childKey := fmt.Sprintf("%s/%s", ch.Namespace, ch.Name)
	if ch.Status.BoundSnapshotContentName == "" {
		return NamespaceSnapshotChildReadyClassPending,
			fmt.Sprintf("waiting for child NamespaceSnapshot %s to bind NamespaceSnapshotContent", childKey)
	}
	rc := meta.FindStatusCondition(ch.Status.Conditions, snapshot.ConditionReady)
	if rc == nil {
		return NamespaceSnapshotChildReadyClassPending,
			fmt.Sprintf("waiting for child NamespaceSnapshot %s Ready condition", childKey)
	}
	switch rc.Status {
	case metav1.ConditionTrue:
		return NamespaceSnapshotChildReadyClassCompleted, ""
	case metav1.ConditionFalse:
		if IsNamespaceSnapshotChildTerminalReadyFailure(rc.Reason) {
			return NamespaceSnapshotChildReadyClassFailed,
				fmt.Sprintf("child NamespaceSnapshot %s failed: reason=%s message=%s", childKey, rc.Reason, rc.Message)
		}
		if rc.Message != "" {
			return NamespaceSnapshotChildReadyClassPending,
				fmt.Sprintf("waiting for child NamespaceSnapshot %s Ready=True: child reason=%s, message=%s", childKey, rc.Reason, rc.Message)
		}
		return NamespaceSnapshotChildReadyClassPending,
			fmt.Sprintf("waiting for child NamespaceSnapshot %s Ready=True: child reason=%s", childKey, rc.Reason)
	default:
		msg := fmt.Sprintf("waiting for child NamespaceSnapshot %s Ready (child Ready status Unknown)", childKey)
		if rc.Message != "" {
			msg = fmt.Sprintf("%s: child message=%s", msg, rc.Message)
		}
		return NamespaceSnapshotChildReadyClassPending, msg
	}
}

// NamespaceSnapshotChildrenRefsSummary aggregates E6 state across status.childrenSnapshotRefs (each ref is a NamespaceSnapshot).
type NamespaceSnapshotChildrenRefsSummary struct {
	HasFailed      bool
	FailedMessages []string
	HasPending     bool
	PendingParts   []string
	AllCompleted   bool
}

// SummarizeNamespaceSnapshotChildrenRefsWithDefaultNamespace resolves empty ref.Namespace as parentSnapshotNamespace.
func SummarizeNamespaceSnapshotChildrenRefsWithDefaultNamespace(ctx context.Context, c client.Reader, refs []storagev1alpha1.NamespaceSnapshotChildRef, parentSnapshotNamespace string) (*NamespaceSnapshotChildrenRefsSummary, error) {
	if len(refs) == 0 {
		return &NamespaceSnapshotChildrenRefsSummary{AllCompleted: true}, nil
	}
	var sum NamespaceSnapshotChildrenRefsSummary
	for _, ref := range refs {
		ns := ref.Namespace
		if ns == "" {
			ns = parentSnapshotNamespace
		}
		key := client.ObjectKey{Namespace: ns, Name: ref.Name}
		child := &storagev1alpha1.NamespaceSnapshot{}
		if err := c.Get(ctx, key, child); err != nil {
			if apierrors.IsNotFound(err) {
				// Ref can be merged before the child NamespaceSnapshot object exists (e.g. domain wiring); treat as pending.
				sum.HasPending = true
				sum.PendingParts = append(sum.PendingParts,
					fmt.Sprintf("child NamespaceSnapshot %s/%s not found yet (waiting for object)", ns, ref.Name))
				continue
			}
			return nil, fmt.Errorf("get child NamespaceSnapshot %s/%s: %w", ns, ref.Name, err)
		}
		cls, msg := ClassifyNamespaceSnapshotChildReady(child)
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
			msg = "one or more child NamespaceSnapshots failed"
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
			msg = "waiting for child NamespaceSnapshots"
		}
		return E6ParentReadyPickOutput{
			Ready: false, Reason: snapshot.ReasonChildSnapshotPending, Message: msg,
		}
	}
	if in.SelfCaptureComplete {
		return E6ParentReadyPickOutput{
			Ready: true, Reason: snapshot.ReasonCompleted, Message: "all required child NamespaceSnapshots are ready",
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
