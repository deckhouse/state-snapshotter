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

package snapshotsdk

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SnapshotAdapter is the single seam between a concrete domain snapshot and the generic capture protocol.
// A domain package implements it next to its snapshot type; the SDK never references the domain type
// directly. It is the only place client.Object crosses the boundary — the domain's Reconcile body uses
// the intent verbs (Ensure*/Mark*/Confirm*/Fail) and never these mapping methods.
//
// Implementations are thin: each method maps one generic concept to a concrete status field. They must be
// free of side effects (no API calls); the SDK owns all reads, writes, and patching.
//
// Writer discipline: the SDK writes ONLY status.captureState.domainSpecificController (via
// Get/SetDomainCaptureState), status.childrenSnapshotRefs (via the same), and status.sourceRef (via
// Get/SetSnapshotSource). It NEVER writes the Ready condition and NEVER writes the core-owned
// captureState.commonController — it only reads them (CoreCaptureState, ReadyReason/ReadyMessage).
type SnapshotAdapter interface {
	// Object returns the live snapshot object. The SDK refreshes it via the API reader and patches it; it
	// must be the same pointer the other accessors read from and write to.
	Object() client.Object

	// SourceRef returns the snapshot's source identity (spec.sourceRef).
	SourceRef() SourceRef

	// GetDomainCaptureState / SetDomainCaptureState bridge the domain-written status fields the SDK
	// publishes: status.captureState.domainSpecificController (MCR/VCR names, phase, reason, message) plus
	// the top-level status.childrenSnapshotRefs.
	GetDomainCaptureState() DomainCaptureState
	SetDomainCaptureState(DomainCaptureState)

	// GetSnapshotSource / SetSnapshotSource bridge the top-level status.sourceRef (full ref to the
	// captured live source object). nil means unset. The SDK publishes it via PublishSnapshotSource.
	GetSnapshotSource() *SnapshotSource
	SetSnapshotSource(*SnapshotSource)

	// CoreCaptureState returns the read-only core handoff (captureState.commonController leg latches) the
	// SDK consults for suppression and for computing CoreCaptureOutcome. The SDK never writes it.
	CoreCaptureState() CoreCaptureState

	// ReadyStatus / ReadyReason / ReadyMessage return the status/reason/message of the core-written
	// status.conditions[Ready]. They are the domain's READ-ONLY view of its own capture outcome (the core
	// is the sole writer of the terminal Ready): a terminal Ready reason (IsReasonTerminal) drives
	// CoreCaptureOutcome=Failed, and ReadyStatus lets a domain observe True/False/Unknown directly when it
	// builds its own Finished/wait/stop logic. All three are empty (ReadyStatus "") when no Ready condition
	// is present yet.
	ReadyStatus() metav1.ConditionStatus
	ReadyReason() string
	ReadyMessage() string
}
