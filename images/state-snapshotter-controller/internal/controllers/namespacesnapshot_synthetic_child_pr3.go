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

// N2b PR3: aggregate parent Ready from one synthetic required child (PR2 scaffold).
// Not a general multi-child framework — explicit whitelist of child terminal failures only.

package controllers

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

type syntheticChildAggregatePhase int

const (
	syntheticChildAggregatePending syntheticChildAggregatePhase = iota
	syntheticChildAggregateReady
	syntheticChildAggregateFailed
)

// syntheticChildAggregateResult drives parent NamespaceSnapshot Ready for N2b PR2/PR3 (single child).
type syntheticChildAggregateResult struct {
	Phase syntheticChildAggregatePhase
	// Reason is the parent Ready condition reason when Phase is Pending or Failed (snapshot.ReasonChildSnapshot*).
	Reason string
	// Message is the parent Ready condition message for that phase.
	Message string
}

// n2bSyntheticChildTerminalReadyReasons is the PR3 allowlist: child NamespaceSnapshot Ready=False reasons
// that N2a treats as terminal capture failure. Include only reasons after which the child is not expected
// to return to Ready=True without external intervention (fix spec, delete MCR, recreate root, etc.); do not
// add transient or ambiguous reasons or the parent will falsely report ChildSnapshotFailed. Any other
// Ready=False on the child keeps the parent pending (in-progress / MCP pending / unknown). Extend only
// together with N2a fail paths and design §11.1.
var n2bSyntheticChildTerminalReadyReasons = map[string]struct{}{
	"ListFailed":               {},
	"NoCaptureTargets":         {},
	"CapturePlanDrift":         {},
	"ManifestCheckpointFailed": {},
	"ContentRefMismatch":       {},
	"NamespaceNotFound":        {},
}

func isN2bSyntheticChildTerminalReadyFailure(reason string) bool {
	_, ok := n2bSyntheticChildTerminalReadyReasons[reason]
	return ok
}

func formatSyntheticChildPendingUntilReadyMessage(childKey, childReason, childMsg string) string {
	if childMsg != "" {
		return fmt.Sprintf("waiting for synthetic child %s Ready=True: child reason=%s, message=%s", childKey, childReason, childMsg)
	}
	return fmt.Sprintf("waiting for synthetic child %s Ready=True: child reason=%s", childKey, childReason)
}

// evaluateSyntheticRequiredChildStateForPR2 maps one synthetic child's status to parent aggregate state.
//
// Preconditions (enforced by call site, not this function): the parent must already have completed N2a
// manifest capture with a persisted ManifestCheckpoint on the parent NamespaceSnapshotContent — i.e.
// reconcileSyntheticTreePR2 runs only after that stage. Do not call this helper earlier or the parent
// Ready semantics will be wrong.
func evaluateSyntheticRequiredChildStateForPR2(child *storagev1alpha1.NamespaceSnapshot) syntheticChildAggregateResult {
	childKey := fmt.Sprintf("%s/%s", child.Namespace, child.Name)
	if child.Status.BoundSnapshotContentName == "" {
		return syntheticChildAggregateResult{
			Phase:   syntheticChildAggregatePending,
			Reason:  snapshot.ReasonChildSnapshotPending,
			Message: fmt.Sprintf("waiting for synthetic child %s to bind NamespaceSnapshotContent", childKey),
		}
	}
	rc := meta.FindStatusCondition(child.Status.Conditions, snapshot.ConditionReady)
	if rc == nil {
		return syntheticChildAggregateResult{
			Phase:   syntheticChildAggregatePending,
			Reason:  snapshot.ReasonChildSnapshotPending,
			Message: fmt.Sprintf("waiting for synthetic child %s Ready condition", childKey),
		}
	}
	switch rc.Status {
	case metav1.ConditionTrue:
		return syntheticChildAggregateResult{Phase: syntheticChildAggregateReady}
	case metav1.ConditionFalse:
		if isN2bSyntheticChildTerminalReadyFailure(rc.Reason) {
			return syntheticChildAggregateResult{
				Phase:   syntheticChildAggregateFailed,
				Reason:  snapshot.ReasonChildSnapshotFailed,
				Message: fmt.Sprintf("synthetic child %s failed: reason=%s message=%s", childKey, rc.Reason, rc.Message),
			}
		}
		return syntheticChildAggregateResult{
			Phase:   syntheticChildAggregatePending,
			Reason:  snapshot.ReasonChildSnapshotPending,
			Message: formatSyntheticChildPendingUntilReadyMessage(childKey, rc.Reason, rc.Message),
		}
	default:
		msg := fmt.Sprintf("waiting for synthetic child %s Ready (child Ready status Unknown)", childKey)
		if rc.Message != "" {
			msg = fmt.Sprintf("%s: child message=%s", msg, rc.Message)
		}
		return syntheticChildAggregateResult{
			Phase:   syntheticChildAggregatePending,
			Reason:  snapshot.ReasonChildSnapshotPending,
			Message: msg,
		}
	}
}
