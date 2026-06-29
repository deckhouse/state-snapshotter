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
// directly. It is the only place metav1.Condition and client.Object cross the boundary — the domain's
// Reconcile body uses the intent verbs (Ensure*/Mark*) and never these mapping methods.
//
// Implementations are thin: each method maps one generic concept to a concrete status field. They must be
// free of side effects (no API calls); the SDK owns all reads, writes, and patching.
type SnapshotAdapter interface {
	// Object returns the live snapshot object. The SDK refreshes it via the API reader and patches it; it
	// must be the same pointer the other accessors read from and write to.
	Object() client.Object

	// SourceRef returns the snapshot's source identity (spec.sourceRef).
	//
	// NOTE: not consumed by the SDK in v1 — no Ensure*/Mark* path reads it. It is part of the adapter
	// contract as identity exposure (logging/diagnostics/future source-aware logic). The domain itself
	// resolves DataRef, manifest targets and children from spec.sourceRef; the SDK does not re-resolve
	// from this getter.
	SourceRef() SourceRef

	// GetConditions / SetConditions bridge the snapshot's status.conditions. The SDK merges a single
	// condition at a time (preserving co-owned conditions) and stamps observedGeneration itself.
	GetConditions() []metav1.Condition
	SetConditions([]metav1.Condition)

	// GetDomainCaptureState / SetDomainCaptureState bridge the durable planning-result status fields the
	// SDK publishes (manifest/volume capture request names, child refs).
	GetDomainCaptureState() DomainCaptureState
	SetDomainCaptureState(DomainCaptureState)
}
