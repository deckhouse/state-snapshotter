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

// Target is the single PVC the snapshot captures as its data. The domain resolves its own PVC
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

// ChildSpec is the child-builder seam: the domain constructs the fully-formed child snapshot object
// (kind, name, spec.sourceRef, labels) and hands it to the SDK, which owns adoption (owner reference),
// create-or-validate, and SnapshotChildRef derivation. The SDK never authors domain child spec fields.
type ChildSpec struct {
	// Object is the desired child snapshot object, built by the domain. The SDK derives its
	// SnapshotChildRef from the object's GVK and name and stamps the parent owner reference on it.
	Object client.Object
}

// NotReadyStatus describes a Ready=False outcome the domain wants published (invalid source, missing
// artifact, …). It generalizes the various Ready=False paths into one verb. The SDK only publishes the
// condition; whether and when to requeue is the controller's decision, expressed through its own
// ctrl.Result (the SDK does not drive the reconcile loop).
type NotReadyStatus struct {
	// Reason is the machine-readable Ready=False reason (domain-chosen).
	Reason Reason
	// Message is an optional human-readable explanation.
	Message string
	// Cause, when set and Message is empty, is used as the human-readable condition message
	// (Cause.Error()). It is not otherwise stored or returned by the SDK.
	Cause error
}

// VolumeCaptureSpec is the domain's data-capture intent: the single PVC to capture. A snapshot node binds
// at most one data artifact (Variant A, cardinality ≤1, see api/storage/v1alpha1 SnapshotContent.dataRef):
// multiple volumes are modeled as child snapshot nodes, never as several data refs on one node. A nil
// DataRef means the snapshot is manifest-only — the SDK ensures no VolumeCaptureRequest and publishes no
// name.
//
// The domain is expected to resolve DataRef from the snapshot's immutable spec.sourceRef, so in practice it
// does not flip between data capture and manifest-only across reconciles. Per the SDK lifecycle model
// (see the Planning interface), the data capture follows per-artifact immutability: before the planning
// barrier an existing VCR targeting a different PVC fails closed, and after the barrier EnsureVolumeCapture
// is inert. A nil DataRef is always a plain no-op — the SDK ensures no VolumeCaptureRequest and, being
// delete-free, does not clear a name it published earlier (so a published VCR survives a later nil).
type VolumeCaptureSpec struct {
	// DataRef is the single PVC the snapshot captures as its data, or nil for a manifest-only snapshot.
	DataRef *Target
}

// ManifestTarget identifies one namespaced object the domain wants captured in the snapshot node's
// ManifestCaptureRequest. It is the shared API type re-exported through the SDK facade.
type ManifestTarget = ssv1alpha1.ManifestTarget

// ManifestCaptureSpec is the domain's manifest-capture intent: the complete desired set of manifest targets
// for the snapshot node. The SDK turns this set into one ManifestCaptureRequest and merges any owned-PVC
// target discovered from the data-capture VolumeCaptureRequest.
type ManifestCaptureSpec struct {
	// Targets is the complete domain-chosen manifest target set for this snapshot node.
	Targets []ManifestTarget
}
