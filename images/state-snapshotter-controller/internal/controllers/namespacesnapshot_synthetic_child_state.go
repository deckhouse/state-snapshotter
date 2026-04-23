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

// Aggregates parent NamespaceSnapshot Ready from one required synthetic child in the temporary N2b tree scaffold.
// Not a general multi-child framework — explicit allowlist of child terminal failures only.

package controllers

import (
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

type syntheticChildAggregatePhase int

const (
	syntheticChildAggregatePending syntheticChildAggregatePhase = iota
	syntheticChildAggregateReady
	syntheticChildAggregateFailed
)

// syntheticChildAggregateResult drives parent NamespaceSnapshot Ready for the single synthetic child case.
type syntheticChildAggregateResult struct {
	Phase syntheticChildAggregatePhase
	// Reason is the parent Ready condition reason when Phase is Pending or Failed (snapshot.ReasonChildSnapshot*).
	Reason string
	// Message is the parent Ready condition message for that phase.
	Message string
}

// evaluateSyntheticRequiredChildState maps one synthetic child's status to parent aggregate state.
//
// Preconditions (enforced by call site, not this function): the parent must already have completed N2a
// manifest capture with a persisted ManifestCheckpoint on the parent NamespaceSnapshotContent — i.e.
// reconcileSyntheticChildTree runs only after that stage. Do not call this helper earlier or the parent
// Ready semantics will be wrong.
func evaluateSyntheticRequiredChildState(child *storagev1alpha1.NamespaceSnapshot) syntheticChildAggregateResult {
	c, msg := usecase.ClassifyNamespaceSnapshotChildReady(child)
	switch c {
	case usecase.NamespaceSnapshotChildReadyClassCompleted:
		return syntheticChildAggregateResult{Phase: syntheticChildAggregateReady}
	case usecase.NamespaceSnapshotChildReadyClassFailed:
		return syntheticChildAggregateResult{
			Phase:   syntheticChildAggregateFailed,
			Reason:  snapshot.ReasonChildSnapshotFailed,
			Message: msg,
		}
	default:
		return syntheticChildAggregateResult{
			Phase:   syntheticChildAggregatePending,
			Reason:  snapshot.ReasonChildSnapshotPending,
			Message: msg,
		}
	}
}
