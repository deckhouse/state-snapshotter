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

package snapshot

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

// NamespaceSnapshotAdapter maps the namespace-root Snapshot to the generic capture protocol
// (pkg/snapshotsdk.SnapshotAdapter), so the root reconciler can drive capture through the SAME SDK as
// external/demo domains ("dogfooding", wave5 docs/wave5-namespace-domain-design.md).
//
// Shape: the root is an aggregator domain — a namespace manifest leg plus children (domain subtrees and
// orphan VolumeSnapshot leaves), with NO single-PVC data leg of its own (SourceRef is a Namespace, not a
// PVC; EnsureVolumeCapture is never called for the root). It therefore mirrors the
// DemoVirtualMachineSnapshot adapter, but is typed (the root is a first-class API type) rather than
// unstructured.
//
// Writer discipline (mirrors demo/snapshot_adapter.go): the SDK writes ONLY
// status.captureState.domainSpecificController (via Get/SetDomainCaptureState), the top-level
// status.childrenSnapshotRefs (via the same), and status.snapshotSource (via Get/SetSnapshotSource). It
// NEVER writes the Ready condition and NEVER writes the core-owned captureState.commonController — it
// only reads them (CoreCaptureState, ReadyReason/ReadyMessage). On the root, commonController.manifestCaptured
// is stamped by the core RBAC latch (ready_patch.go stampRootManifestCaptured), which the SDK only reads.
type NamespaceSnapshotAdapter struct {
	snap *storagev1alpha1.Snapshot
}

// NewNamespaceSnapshotAdapter wraps a root Snapshot as a SnapshotAdapter. The adapter mutates the passed
// object through its pointer, so callers hand the SDK the same *Snapshot they later persist.
func NewNamespaceSnapshotAdapter(snap *storagev1alpha1.Snapshot) NamespaceSnapshotAdapter {
	return NamespaceSnapshotAdapter{snap: snap}
}

// Compile-time proof the root adapter satisfies the SDK seam.
var _ snapshotsdk.SnapshotAdapter = NamespaceSnapshotAdapter{}

func (a NamespaceSnapshotAdapter) Object() client.Object { return a.snap }

// SourceRef returns the root's logical source identity: the captured Namespace. It is the lightweight
// SourceRef type (distinct from the full status.snapshotSource ref published via PublishSnapshotSource),
// and is unused on the root — the root has no single-PVC data leg, so EnsureVolumeCapture is never called.
func (a NamespaceSnapshotAdapter) SourceRef() snapshotsdk.SourceRef {
	return snapshotsdk.SourceRef{
		APIVersion: "v1",
		Kind:       "Namespace",
		Name:       a.snap.Namespace,
	}
}

func (a NamespaceSnapshotAdapter) GetDomainCaptureState() snapshotsdk.DomainCaptureState {
	st := snapshotsdk.DomainCaptureState{ChildrenSnapshotRefs: a.snap.Status.ChildrenSnapshotRefs}
	if cs := a.snap.Status.CaptureState; cs != nil && cs.DomainSpecificController != nil {
		d := cs.DomainSpecificController
		st.ManifestCaptureRequestName = d.ManifestCaptureRequestName
		st.VolumeCaptureRequestName = d.VolumeCaptureRequestName
		st.ExcludedRefs = d.ExcludedRefs
		st.Phase = d.Phase
		st.Reason = d.Reason
		st.Message = d.Message
	}
	return st
}

func (a NamespaceSnapshotAdapter) SetDomainCaptureState(st snapshotsdk.DomainCaptureState) {
	a.snap.Status.ChildrenSnapshotRefs = st.ChildrenSnapshotRefs
	ensureDomainSpecificController(&a.snap.Status.CaptureState)
	d := a.snap.Status.CaptureState.DomainSpecificController
	d.ManifestCaptureRequestName = st.ManifestCaptureRequestName
	// The root has no data leg of its own (aggregator): a namespace VCR never exists, so
	// volumeCaptureRequestName stays empty regardless of the SDK-provided value.
	d.VolumeCaptureRequestName = st.VolumeCaptureRequestName
	d.ExcludedRefs = nonNilExcludedRefs(st.ExcludedRefs)
	d.Phase = st.Phase
	d.Reason = st.Reason
	d.Message = st.Message
}

func (a NamespaceSnapshotAdapter) GetSnapshotSource() *snapshotsdk.SnapshotSource {
	return a.snap.Status.SnapshotSource
}

func (a NamespaceSnapshotAdapter) SetSnapshotSource(src *snapshotsdk.SnapshotSource) {
	a.snap.Status.SnapshotSource = src
}

func (a NamespaceSnapshotAdapter) CoreCaptureState() snapshotsdk.CoreCaptureState {
	return coreCaptureStateFrom(a.snap.Status.CaptureState)
}

func (a NamespaceSnapshotAdapter) ReadyReason() string {
	return readyConditionReason(a.snap.Status.Conditions)
}

func (a NamespaceSnapshotAdapter) ReadyMessage() string {
	return readyConditionMessage(a.snap.Status.Conditions)
}

// nonNilExcludedRefs guarantees a non-nil slice. captureState.domainSpecificController.excludedRefs is
// written WITHOUT omitempty, so a nil slice would marshal to JSON null and be rejected by the
// non-nullable CRD array. An empty [] is the correct "domain planned, nothing excluded" wire value.
func nonNilExcludedRefs(refs []storagev1alpha1.ExcludedObjectRef) []storagev1alpha1.ExcludedObjectRef {
	if refs == nil {
		return []storagev1alpha1.ExcludedObjectRef{}
	}
	return refs
}

// ensureDomainSpecificController lazily allocates captureState + its domain-written half so the adapter
// can stamp the domain planning refs/phase. The core-written commonController half is never touched here.
func ensureDomainSpecificController(cs **storagev1alpha1.CaptureStateStatus) {
	if *cs == nil {
		*cs = &storagev1alpha1.CaptureStateStatus{}
	}
	if (*cs).DomainSpecificController == nil {
		(*cs).DomainSpecificController = &storagev1alpha1.DomainSpecificControllerCaptureState{}
	}
}

// coreCaptureStateFrom reads the core-written capture-leg latches (captureState.commonController) into the
// SDK's read-only view. Absent sub-structure => both legs nil (no leg declared yet). On the root only the
// manifest leg is ever declared (no data leg).
func coreCaptureStateFrom(cs *storagev1alpha1.CaptureStateStatus) snapshotsdk.CoreCaptureState {
	if cs == nil || cs.CommonController == nil {
		return snapshotsdk.CoreCaptureState{}
	}
	return snapshotsdk.CoreCaptureState{
		ManifestCaptured: cs.CommonController.ManifestCaptured,
		DataCaptured:     cs.CommonController.DataCaptured,
	}
}

func readyConditionReason(conditions []metav1.Condition) string {
	if c := meta.FindStatusCondition(conditions, storagev1alpha1.ConditionReady); c != nil {
		return c.Reason
	}
	return ""
}

func readyConditionMessage(conditions []metav1.Condition) string {
	if c := meta.FindStatusCondition(conditions, storagev1alpha1.ConditionReady); c != nil {
		return c.Message
	}
	return ""
}
