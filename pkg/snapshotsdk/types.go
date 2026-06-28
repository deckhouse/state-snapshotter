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
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/storagefoundation"
)

// SourceRef identifies the namespace-local source object a snapshot captures. It mirrors the generic
// spec.sourceRef contract; the namespace is implicit (the snapshot's namespace).
type SourceRef struct {
	APIVersion string
	Kind       string
	Name       string
}

// SnapshotChildRef identifies one child snapshot CR in the snapshot run tree. It is the durable record the
// SDK publishes (the set of children currently attached to the snapshot graph). This is the shared api
// contract type, re-exported so domain controllers and adapters reference a single definition.
type SnapshotChildRef = storagev1alpha1.SnapshotChildRef

// Target is the single PVC capture target of a snapshot's data leg. The domain resolves its own PVC
// (including readiness/ArtifactMissing decisions) and hands the SDK the result; the SDK turns it into the
// storage-foundation VolumeCaptureRequest.
type Target = storagefoundation.Target

// Reason is a stable, machine-readable condition reason published by the SDK on behalf of the domain. The
// domain chooses the reason (for example "InvalidSourceRef", "SourceNotFound", "ArtifactMissing"); the SDK
// never invents domain semantics.
type Reason string

// DomainCaptureState is the durable, domain-owned planning result the SDK publishes into snapshot status:
// the names of the manifest- and volume-capture requests it created and the set of child snapshot refs.
// Adapters map it to and from the concrete snapshot status fields.
type DomainCaptureState struct {
	ManifestCaptureRequestName string
	VolumeCaptureRequestName   string
	ChildrenSnapshotRefs       []SnapshotChildRef
}

// CoreCaptureState is the read-only, core-owned handoff the SDK consults to suppress re-creating capture
// requests after the core controller has durably stamped a leg captured. It is never written by the SDK.
type CoreCaptureState struct {
	// ManifestCaptured is set by the core controller once the manifest leg is durably checkpointed.
	ManifestCaptured bool
	// DataCaptured is set by the core controller once the data leg's content handoff is durable.
	DataCaptured bool
}

// ChildSpec is the child-builder seam: the domain constructs the fully-formed child snapshot object
// (kind, name, spec.sourceRef, labels) and hands it to the SDK, which owns adoption (owner reference),
// create-or-validate, and SnapshotChildRef derivation. The SDK never authors domain child spec fields.
type ChildSpec struct {
	// Object is the desired child snapshot object, built by the domain. The SDK derives its
	// SnapshotChildRef from the object's GVK and name and stamps the parent owner reference on it.
	Object client.Object
}

// NotReadySpec describes a non-terminal-or-terminal Ready=False outcome the domain wants published
// (invalid source, missing artifact, …). It generalizes the various Ready=False paths into one verb.
type NotReadySpec struct {
	// Reason is the machine-readable Ready=False reason (domain-chosen).
	Reason Reason
	// Message is an optional human-readable explanation.
	Message string
	// Cause, when set, is logged/returned so the manager can surface the underlying error.
	Cause error
	// Requeue asks the caller to requeue (for example, an artifact that may appear later). When false the
	// outcome is treated as terminal-until-spec-change and the SDK returns no error and no requeue intent.
	Requeue bool
}

// VolumeCaptureSpec is the domain's data-leg intent: the single PVC to capture. A snapshot node binds at
// most one data artifact (Variant A, cardinality ≤1, see api/storage/v1alpha1 SnapshotContent.dataRef):
// multiple volumes are modeled as child snapshot nodes, never as several data refs on one node. A nil
// DataRef means the snapshot is manifest-only — the SDK ensures no VolumeCaptureRequest and publishes no
// name.
type VolumeCaptureSpec struct {
	// DataRef is the snapshot's single data-leg PVC, or nil for a manifest-only snapshot.
	DataRef *Target
}

// ManifestTarget identifies one namespaced object the domain wants captured in the snapshot node's
// ManifestCaptureRequest. It is the shared API type re-exported through the SDK facade.
type ManifestTarget = ssv1alpha1.ManifestTarget

// ManifestCaptureSpec is the domain's manifest-leg intent: the complete desired set of manifest targets for
// the snapshot node. The SDK turns this set into one ManifestCaptureRequest and merges any owned-PVC target
// discovered from the data-leg VolumeCaptureRequest.
type ManifestCaptureSpec struct {
	// Targets is the complete domain-chosen manifest target set for this snapshot node.
	Targets []ManifestTarget
}
