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
	"context"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// DomainCaptureStatusWriter is the single public API for publishing the domain-owned capture triple
// (phase, reason, message) into status.captureState.domainSpecificController.
//
// Use it for every lifecycle write: waiting (Planning), barrier 1 (Planned), barrier 2 (Finished), and
// terminal failure (Failed). Apply always goes through the SDK status-write path (SnapshotAdapter +
// monotonic setPhase rules). It never writes status.conditions.
type DomainCaptureStatusWriter interface {
	Phase(Phase) DomainCaptureStatusWriter
	Reason(Reason) DomainCaptureStatusWriter
	Message(string) DomainCaptureStatusWriter
	Apply(ctx context.Context) error
}

// domainCaptureStatus accumulates a full desired triple and applies it via the bound SDK.
type domainCaptureStatus struct {
	s       *sdk
	adapter SnapshotAdapter
	phase   Phase
	reason  Reason
	message string
}

// DomainCaptureStatus returns a fluent writer bound to this SDK instance and adapter. Prefer a fresh
// writer (or a full Phase/Reason/Message rewrite) before each Apply so stale reason/message cannot leak
// across reconcile branches.
func (s *sdk) DomainCaptureStatus(t SnapshotAdapter) DomainCaptureStatusWriter {
	return &domainCaptureStatus{s: s, adapter: t}
}

func (b *domainCaptureStatus) Phase(phase Phase) DomainCaptureStatusWriter {
	b.phase = phase
	return b
}

func (b *domainCaptureStatus) Reason(reason Reason) DomainCaptureStatusWriter {
	b.reason = reason
	return b
}

func (b *domainCaptureStatus) Message(message string) DomainCaptureStatusWriter {
	b.message = message
	return b
}

// Apply publishes the accumulated triple through the existing setPhase path (monotonic transitions,
// Failed sink, optimistic status patch). Entering Planned or Finished (phase change) force-clears
// reason and message — the barrier assert. A same-phase re-apply keeps the builder's reason/message
// so a post-barrier diagnostic (or an explicit clear with Message("")) can update the domain message
// without regressing phase. Failed carries whatever reason/message were set (no extra validation policy).
func (b *domainCaptureStatus) Apply(ctx context.Context) error {
	phase := b.phase
	reason := string(b.reason)
	message := b.message
	cur := b.adapter.GetDomainCaptureState()
	switch phase {
	case storagev1alpha1.SnapshotCapturePhasePlanned, storagev1alpha1.SnapshotCapturePhaseFinished:
		if cur.Phase != phase {
			reason = ""
			message = ""
		}
	}
	return b.s.setPhase(ctx, b.adapter, phase, reason, message)
}
