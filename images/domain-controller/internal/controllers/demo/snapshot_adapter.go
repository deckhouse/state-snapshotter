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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

// demoVirtualDiskSnapshotAdapter maps a DemoVirtualDiskSnapshot to the generic capture protocol. The disk
// snapshot is a leaf (no children); its data leg is a single optional PVC.
type demoVirtualDiskSnapshotAdapter struct {
	snap *demov1alpha1.DemoVirtualDiskSnapshot
}

func (a demoVirtualDiskSnapshotAdapter) Object() client.Object { return a.snap }

func (a demoVirtualDiskSnapshotAdapter) SourceRef() snapshotsdk.SourceRef {
	return snapshotsdk.SourceRef{
		APIVersion: a.snap.Spec.SourceRef.APIVersion,
		Kind:       a.snap.Spec.SourceRef.Kind,
		Name:       a.snap.Spec.SourceRef.Name,
	}
}

func (a demoVirtualDiskSnapshotAdapter) GetConditions() []metav1.Condition {
	return a.snap.Status.Conditions
}
func (a demoVirtualDiskSnapshotAdapter) SetConditions(c []metav1.Condition) {
	a.snap.Status.Conditions = c
}

func (a demoVirtualDiskSnapshotAdapter) GetDomainCaptureState() snapshotsdk.DomainCaptureState {
	return snapshotsdk.DomainCaptureState{
		ManifestCaptureRequestName: a.snap.Status.ManifestCaptureRequestName,
		VolumeCaptureRequestName:   a.snap.Status.VolumeCaptureRequestName,
	}
}

func (a demoVirtualDiskSnapshotAdapter) SetDomainCaptureState(st snapshotsdk.DomainCaptureState) {
	a.snap.Status.ManifestCaptureRequestName = st.ManifestCaptureRequestName
	a.snap.Status.VolumeCaptureRequestName = st.VolumeCaptureRequestName
}

func (a demoVirtualDiskSnapshotAdapter) CoreCaptureState() snapshotsdk.CoreCaptureState {
	return snapshotsdk.CoreCaptureState{
		ManifestCaptured: a.snap.Status.ManifestCaptured,
		DataCaptured:     a.snap.Status.DataCaptured,
	}
}

// demoVirtualMachineSnapshotAdapter maps a DemoVirtualMachineSnapshot to the generic capture protocol. The
// VM snapshot is manifest-only (no data leg) and owns a set of child disk snapshots.
type demoVirtualMachineSnapshotAdapter struct {
	snap *demov1alpha1.DemoVirtualMachineSnapshot
}

func (a demoVirtualMachineSnapshotAdapter) Object() client.Object { return a.snap }

func (a demoVirtualMachineSnapshotAdapter) SourceRef() snapshotsdk.SourceRef {
	return snapshotsdk.SourceRef{
		APIVersion: a.snap.Spec.SourceRef.APIVersion,
		Kind:       a.snap.Spec.SourceRef.Kind,
		Name:       a.snap.Spec.SourceRef.Name,
	}
}

func (a demoVirtualMachineSnapshotAdapter) GetConditions() []metav1.Condition {
	return a.snap.Status.Conditions
}
func (a demoVirtualMachineSnapshotAdapter) SetConditions(c []metav1.Condition) {
	a.snap.Status.Conditions = c
}

func (a demoVirtualMachineSnapshotAdapter) GetDomainCaptureState() snapshotsdk.DomainCaptureState {
	return snapshotsdk.DomainCaptureState{
		ManifestCaptureRequestName: a.snap.Status.ManifestCaptureRequestName,
		ChildrenSnapshotRefs:       a.snap.Status.ChildrenSnapshotRefs,
	}
}

func (a demoVirtualMachineSnapshotAdapter) SetDomainCaptureState(st snapshotsdk.DomainCaptureState) {
	a.snap.Status.ManifestCaptureRequestName = st.ManifestCaptureRequestName
	a.snap.Status.ChildrenSnapshotRefs = st.ChildrenSnapshotRefs
}

func (a demoVirtualMachineSnapshotAdapter) CoreCaptureState() snapshotsdk.CoreCaptureState {
	return snapshotsdk.CoreCaptureState{ManifestCaptured: a.snap.Status.ManifestCaptured}
}
