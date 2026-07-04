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

package demo

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

// demoVirtualDiskSnapshotAdapter maps a DemoVirtualDiskSnapshot to the generic capture protocol. The disk
// snapshot is a leaf (no children); its data leg is a single optional PVC.
//
// Writer discipline (wave3): the SDK writes ONLY status.captureState.domainSpecificController (via
// Get/SetDomainCaptureState) and top-level status.snapshotSource (via Get/SetSnapshotSource). It never
// writes the Ready condition and never writes the core-owned captureState.commonController — it only
// reads them (CoreCaptureState, ReadyReason/ReadyMessage).
type demoVirtualDiskSnapshotAdapter struct {
	snap *demov1alpha1.DemoVirtualDiskSnapshot
}

func (a demoVirtualDiskSnapshotAdapter) Object() client.Object { return a.snap }

func (a demoVirtualDiskSnapshotAdapter) SourceRef() snapshotsdk.SourceRef {
	if a.snap.Spec.SourceRef == nil {
		return snapshotsdk.SourceRef{}
	}
	return snapshotsdk.SourceRef{
		APIVersion: a.snap.Spec.SourceRef.APIVersion,
		Kind:       a.snap.Spec.SourceRef.Kind,
		Name:       a.snap.Spec.SourceRef.Name,
	}
}

func (a demoVirtualDiskSnapshotAdapter) GetDomainCaptureState() snapshotsdk.DomainCaptureState {
	st := snapshotsdk.DomainCaptureState{ChildrenSnapshotRefs: a.snap.Status.ChildrenSnapshotRefs}
	if cs := a.snap.Status.CaptureState; cs != nil && cs.DomainSpecificController != nil {
		d := cs.DomainSpecificController
		st.ManifestCaptureRequestName = d.ManifestCaptureRequestName
		st.VolumeCaptureRequestName = d.VolumeCaptureRequestName
		st.Phase = d.Phase
		st.Reason = d.Reason
		st.Message = d.Message
	}
	return st
}

func (a demoVirtualDiskSnapshotAdapter) SetDomainCaptureState(st snapshotsdk.DomainCaptureState) {
	a.snap.Status.ChildrenSnapshotRefs = st.ChildrenSnapshotRefs
	ensureDomainSpecificController(&a.snap.Status.CaptureState)
	d := a.snap.Status.CaptureState.DomainSpecificController
	d.ManifestCaptureRequestName = st.ManifestCaptureRequestName
	d.VolumeCaptureRequestName = st.VolumeCaptureRequestName
	d.Phase = st.Phase
	d.Reason = st.Reason
	d.Message = st.Message
}

func (a demoVirtualDiskSnapshotAdapter) GetSnapshotSource() *snapshotsdk.SnapshotSource {
	return a.snap.Status.SnapshotSource
}

func (a demoVirtualDiskSnapshotAdapter) SetSnapshotSource(src *snapshotsdk.SnapshotSource) {
	a.snap.Status.SnapshotSource = src
}

func (a demoVirtualDiskSnapshotAdapter) CoreCaptureState() snapshotsdk.CoreCaptureState {
	return coreCaptureStateFrom(a.snap.Status.CaptureState)
}

func (a demoVirtualDiskSnapshotAdapter) ReadyReason() string  { return readyReason(a.snap.Status.Conditions) }
func (a demoVirtualDiskSnapshotAdapter) ReadyMessage() string { return readyMessage(a.snap.Status.Conditions) }

// demoVirtualMachineSnapshotAdapter maps a DemoVirtualMachineSnapshot to the generic capture protocol. The
// VM snapshot is manifest-only (no data leg) and owns a set of child disk snapshots.
type demoVirtualMachineSnapshotAdapter struct {
	snap *demov1alpha1.DemoVirtualMachineSnapshot
}

func (a demoVirtualMachineSnapshotAdapter) Object() client.Object { return a.snap }

func (a demoVirtualMachineSnapshotAdapter) SourceRef() snapshotsdk.SourceRef {
	if a.snap.Spec.SourceRef == nil {
		return snapshotsdk.SourceRef{}
	}
	return snapshotsdk.SourceRef{
		APIVersion: a.snap.Spec.SourceRef.APIVersion,
		Kind:       a.snap.Spec.SourceRef.Kind,
		Name:       a.snap.Spec.SourceRef.Name,
	}
}

func (a demoVirtualMachineSnapshotAdapter) GetDomainCaptureState() snapshotsdk.DomainCaptureState {
	st := snapshotsdk.DomainCaptureState{ChildrenSnapshotRefs: a.snap.Status.ChildrenSnapshotRefs}
	if cs := a.snap.Status.CaptureState; cs != nil && cs.DomainSpecificController != nil {
		d := cs.DomainSpecificController
		st.ManifestCaptureRequestName = d.ManifestCaptureRequestName
		st.VolumeCaptureRequestName = d.VolumeCaptureRequestName
		st.Phase = d.Phase
		st.Reason = d.Reason
		st.Message = d.Message
	}
	return st
}

func (a demoVirtualMachineSnapshotAdapter) SetDomainCaptureState(st snapshotsdk.DomainCaptureState) {
	a.snap.Status.ChildrenSnapshotRefs = st.ChildrenSnapshotRefs
	ensureDomainSpecificController(&a.snap.Status.CaptureState)
	d := a.snap.Status.CaptureState.DomainSpecificController
	d.ManifestCaptureRequestName = st.ManifestCaptureRequestName
	d.VolumeCaptureRequestName = st.VolumeCaptureRequestName
	d.Phase = st.Phase
	d.Reason = st.Reason
	d.Message = st.Message
}

func (a demoVirtualMachineSnapshotAdapter) GetSnapshotSource() *snapshotsdk.SnapshotSource {
	return a.snap.Status.SnapshotSource
}

func (a demoVirtualMachineSnapshotAdapter) SetSnapshotSource(src *snapshotsdk.SnapshotSource) {
	a.snap.Status.SnapshotSource = src
}

func (a demoVirtualMachineSnapshotAdapter) CoreCaptureState() snapshotsdk.CoreCaptureState {
	return coreCaptureStateFrom(a.snap.Status.CaptureState)
}

func (a demoVirtualMachineSnapshotAdapter) ReadyReason() string {
	return readyReason(a.snap.Status.Conditions)
}
func (a demoVirtualMachineSnapshotAdapter) ReadyMessage() string {
	return readyMessage(a.snap.Status.Conditions)
}

// ensureDomainSpecificController lazily allocates captureState + its domain-written half so the adapter can
// stamp the domain planning refs/phase. The core-written commonController half is never touched here.
func ensureDomainSpecificController(cs **storagev1alpha1.CaptureStateStatus) {
	if *cs == nil {
		*cs = &storagev1alpha1.CaptureStateStatus{}
	}
	if (*cs).DomainSpecificController == nil {
		(*cs).DomainSpecificController = &storagev1alpha1.DomainSpecificControllerCaptureState{}
	}
}

// coreCaptureStateFrom reads the core-written capture-leg latches (captureState.commonController) into the
// SDK's read-only view. Absent sub-structure => both legs nil (no leg declared yet).
func coreCaptureStateFrom(cs *storagev1alpha1.CaptureStateStatus) snapshotsdk.CoreCaptureState {
	if cs == nil || cs.CommonController == nil {
		return snapshotsdk.CoreCaptureState{}
	}
	return snapshotsdk.CoreCaptureState{
		ManifestCaptured: cs.CommonController.ManifestCaptured,
		DataCaptured:     cs.CommonController.DataCaptured,
	}
}

func readyReason(conditions []metav1.Condition) string {
	if c := meta.FindStatusCondition(conditions, storagev1alpha1.ConditionReady); c != nil {
		return c.Reason
	}
	return ""
}

func readyMessage(conditions []metav1.Condition) string {
	if c := meta.FindStatusCondition(conditions, storagev1alpha1.ConditionReady); c != nil {
		return c.Message
	}
	return ""
}
