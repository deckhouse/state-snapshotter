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
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

func boolPtr(b bool) *bool { return &b }

func newRootSnapshot() *storagev1alpha1.Snapshot {
	return &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app-snap", Namespace: "my-app", UID: "root-uid"},
	}
}

// SourceRef is the captured Namespace identity (lightweight ref; unused on the root data leg).
func TestNamespaceSnapshotAdapter_SourceRef(t *testing.T) {
	a := NewNamespaceSnapshotAdapter(newRootSnapshot())
	got := a.SourceRef()
	want := snapshotsdk.SourceRef{APIVersion: "v1", Kind: "Namespace", Name: "my-app"}
	if got != want {
		t.Fatalf("SourceRef = %+v, want %+v", got, want)
	}
}

// SetDomainCaptureState writes the domain half + top-level childrenSnapshotRefs; GetDomainCaptureState
// round-trips it back.
func TestNamespaceSnapshotAdapter_DomainCaptureStateRoundTrip(t *testing.T) {
	snap := newRootSnapshot()
	a := NewNamespaceSnapshotAdapter(snap)

	children := []snapshotsdk.SnapshotChildRef{{
		APIVersion: "demo.state-snapshotter.deckhouse.io/v1alpha1",
		Kind:       "DemoVirtualMachineSnapshot",
		Name:       "nss-snap-child",
	}}
	in := snapshotsdk.DomainCaptureState{
		ManifestCaptureRequestName: "nss-mcr-root",
		ChildrenSnapshotRefs:       children,
		Phase:                      snapshotsdk.PhaseFinished,
		Message:                    "namespace snapshot planned",
	}
	a.SetDomainCaptureState(in)

	if snap.Status.CaptureState == nil || snap.Status.CaptureState.DomainSpecificController == nil {
		t.Fatalf("SetDomainCaptureState did not allocate domainSpecificController")
	}
	d := snap.Status.CaptureState.DomainSpecificController
	if d.ManifestCaptureRequestName != "nss-mcr-root" {
		t.Errorf("manifestCaptureRequestName = %q, want nss-mcr-root", d.ManifestCaptureRequestName)
	}
	if d.Phase != snapshotsdk.PhaseFinished {
		t.Errorf("phase = %q, want Finished", d.Phase)
	}
	if len(snap.Status.ChildrenSnapshotRefs) != 1 || snap.Status.ChildrenSnapshotRefs[0].Name != "nss-snap-child" {
		t.Errorf("childrenSnapshotRefs = %+v, want the single child ref", snap.Status.ChildrenSnapshotRefs)
	}

	out := a.GetDomainCaptureState()
	if out.ManifestCaptureRequestName != in.ManifestCaptureRequestName || out.Phase != in.Phase || out.Message != in.Message {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
	if len(out.ChildrenSnapshotRefs) != 1 || out.ChildrenSnapshotRefs[0].Name != "nss-snap-child" {
		t.Errorf("round-trip children mismatch: %+v", out.ChildrenSnapshotRefs)
	}
}

// Write discipline: SetDomainCaptureState must not touch the core-owned commonController or the Ready
// condition — those stay exactly as the core wrote them.
func TestNamespaceSnapshotAdapter_SetDomainCaptureState_LeavesCoreUntouched(t *testing.T) {
	snap := newRootSnapshot()
	snap.Status.CaptureState = &storagev1alpha1.CaptureStateStatus{
		CommonController: &storagev1alpha1.CommonControllerCaptureState{ManifestCaptured: boolPtr(true)},
	}
	meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{
		Type: storagev1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed",
	})

	a := NewNamespaceSnapshotAdapter(snap)
	a.SetDomainCaptureState(snapshotsdk.DomainCaptureState{Phase: snapshotsdk.PhasePlanned})

	cc := snap.Status.CaptureState.CommonController
	if cc == nil || cc.ManifestCaptured == nil || !*cc.ManifestCaptured {
		t.Errorf("commonController.manifestCaptured was mutated: %+v", cc)
	}
	if cc != nil && cc.DataCaptured != nil {
		t.Errorf("root must have no dataCaptured leg, got %v", *cc.DataCaptured)
	}
	if c := meta.FindStatusCondition(snap.Status.Conditions, storagev1alpha1.ConditionReady); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Completed" {
		t.Errorf("Ready condition was mutated: %+v", c)
	}
}

// excludedRefs is written without omitempty, so a nil input must be persisted as an empty [] (never nil).
func TestNamespaceSnapshotAdapter_ExcludedRefsNonNil(t *testing.T) {
	snap := newRootSnapshot()
	a := NewNamespaceSnapshotAdapter(snap)
	a.SetDomainCaptureState(snapshotsdk.DomainCaptureState{Phase: snapshotsdk.PhasePlanned, ExcludedRefs: nil})

	got := snap.Status.CaptureState.DomainSpecificController.ExcludedRefs
	if got == nil {
		t.Fatalf("excludedRefs must be non-nil ([]), got nil")
	}
	if len(got) != 0 {
		t.Errorf("excludedRefs = %+v, want empty", got)
	}
}

func TestNamespaceSnapshotAdapter_SnapshotSourceRoundTrip(t *testing.T) {
	snap := newRootSnapshot()
	a := NewNamespaceSnapshotAdapter(snap)
	if a.GetSnapshotSource() != nil {
		t.Fatalf("expected nil snapshotSource initially")
	}
	src := &snapshotsdk.SnapshotSource{APIVersion: "v1", Kind: "Namespace", Name: "my-app", UID: "ns-uid"}
	a.SetSnapshotSource(src)
	got := a.GetSnapshotSource()
	if got == nil || *got != *src {
		t.Errorf("snapshotSource round-trip = %+v, want %+v", got, src)
	}
}

func TestNamespaceSnapshotAdapter_CoreCaptureStateReadsCommon(t *testing.T) {
	snap := newRootSnapshot()
	snap.Status.CaptureState = &storagev1alpha1.CaptureStateStatus{
		CommonController: &storagev1alpha1.CommonControllerCaptureState{ManifestCaptured: boolPtr(true)},
	}
	a := NewNamespaceSnapshotAdapter(snap)
	cs := a.CoreCaptureState()
	if cs.ManifestCaptured == nil || !*cs.ManifestCaptured {
		t.Errorf("manifestCaptured = %v, want true", cs.ManifestCaptured)
	}
	if cs.DataCaptured != nil {
		t.Errorf("dataCaptured = %v, want nil (root has no data leg)", *cs.DataCaptured)
	}

	// Absent captureState => both legs nil.
	empty := NewNamespaceSnapshotAdapter(newRootSnapshot()).CoreCaptureState()
	if empty.ManifestCaptured != nil || empty.DataCaptured != nil {
		t.Errorf("empty CoreCaptureState = %+v, want both nil", empty)
	}
}

func TestNamespaceSnapshotAdapter_ReadyReasonMessage(t *testing.T) {
	snap := newRootSnapshot()
	a := NewNamespaceSnapshotAdapter(snap)
	if a.ReadyReason() != "" || a.ReadyMessage() != "" {
		t.Fatalf("expected empty reason/message with no Ready condition")
	}
	meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{
		Type: storagev1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "Capturing", Message: "still capturing",
	})
	if a.ReadyReason() != "Capturing" {
		t.Errorf("ReadyReason = %q, want Capturing", a.ReadyReason())
	}
	if a.ReadyMessage() != "still capturing" {
		t.Errorf("ReadyMessage = %q, want 'still capturing'", a.ReadyMessage())
	}
}
