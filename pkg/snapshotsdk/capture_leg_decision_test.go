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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/manifest"
)

// The leg latch is three-valued and the SDK must keep it that way. nil is not "not captured yet": it is
// "the core has not declared this leg", which is also what a snapshot looks like right after a writer
// without extension fields replaced its status. A two-valued reading answers that state with "create a
// capture request", and a second request captures manifests (or a volume) from a cluster that has moved on
// since the artifact this snapshot already owns was taken.

func decisionTestAdapter(latch *bool, mcrName string) *refreshTestAdapter {
	return &refreshTestAdapter{
		obj:    &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "snap-d", UID: types.UID("snap-d-uid")}},
		core:   CoreCaptureState{ManifestCaptured: latch},
		domain: DomainCaptureState{ManifestCaptureRequestName: mcrName},
	}
}

func TestManifestCaptureDecisionIsThreeValued(t *testing.T) {
	for _, tc := range []struct {
		name     string
		latch    *bool
		mcrName  string
		want     CaptureLegDecision
		wantNeed bool
	}{
		{name: "no latch: the core has not declared the leg", latch: nil, want: CaptureLegWaitDeclaration},
		{name: "declared and open", latch: refreshBoolPtr(false), want: CaptureLegPlan, wantNeed: true},
		{name: "captured", latch: refreshBoolPtr(true), want: CaptureLegSuppressed},
		{name: "planned: the frozen plan wins over an open latch", latch: refreshBoolPtr(false), mcrName: "nss-mcr-frozen", want: CaptureLegSuppressed},
		{name: "planned with a wiped latch is still planned", latch: nil, mcrName: "nss-mcr-frozen", want: CaptureLegSuppressed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := decisionTestAdapter(tc.latch, tc.mcrName)
			if got := ManifestCaptureDecision(adapter); got != tc.want {
				t.Fatalf("ManifestCaptureDecision = %v, want %v", got, tc.want)
			}
			if got := ManifestCaptureNeeded(adapter); got != tc.wantNeed {
				t.Fatalf("ManifestCaptureNeeded = %v, want %v (true only for CaptureLegPlan)", got, tc.wantNeed)
			}
		})
	}
}

func TestVolumeCaptureDecisionIsThreeValued(t *testing.T) {
	for _, tc := range []struct {
		name    string
		latch   *bool
		vcrName string
		want    CaptureLegDecision
	}{
		{name: "no data line on this node, or not declared yet", latch: nil, want: CaptureLegWaitDeclaration},
		{name: "declared and open", latch: refreshBoolPtr(false), want: CaptureLegPlan},
		{name: "captured", latch: refreshBoolPtr(true), want: CaptureLegSuppressed},
		{name: "request published", latch: refreshBoolPtr(false), vcrName: "nss-vcr-1", want: CaptureLegSuppressed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &refreshTestAdapter{
				obj:    &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "snap-d", UID: types.UID("snap-d-uid")}},
				core:   CoreCaptureState{DataCaptured: tc.latch},
				domain: DomainCaptureState{VolumeCaptureRequestName: tc.vcrName},
			}
			if got := VolumeCaptureDecision(adapter); got != tc.want {
				t.Fatalf("VolumeCaptureDecision = %v, want %v", got, tc.want)
			}
		})
	}
}

// The defect this closes, in the shape it happens: the owner's captureState is gone (replaced by a typed
// writer), so the latch reads nil and the published request name is gone with it. Under a two-valued
// reading this is indistinguishable from a fresh snapshot and the SDK created a second request.
func TestEnsureCaptureCreatesNothingWhileTheLegIsUndeclared(t *testing.T) {
	ctx := context.Background()
	scheme := newRefreshTestScheme(t)

	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "snap-wiped", UID: types.UID("snap-wiped-uid")},
	}
	snap.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}).
		WithObjects(snap).
		Build()

	reader := &countingReader{}
	provider := &fakeVolumeProvider{name: "vcr-wiped"}
	// Both latches absent: the wiped state.
	adapter := &refreshTestAdapter{obj: snap, core: CoreCaptureState{}}
	sdk := New(cl, reader, provider)

	targets := ManifestCaptureSpec{Targets: []ManifestTarget{{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-1"}}}
	if err := sdk.EnsureManifestCapture(ctx, adapter, targets); err != nil {
		t.Fatalf("an undeclared manifest leg must be a no-op, not an error: %v", err)
	}
	if n := countMCRs(ctx, t, cl); n != 0 {
		t.Fatalf("MCRs created while the leg was undeclared = %d, want 0", n)
	}
	if adapter.domain.ManifestCaptureRequestName != "" {
		t.Fatalf("no request means no published name, got %q", adapter.domain.ManifestCaptureRequestName)
	}

	pvc := Target{UID: "pvc-uid", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-wiped", Namespace: "ns1"}
	if err := sdk.EnsureVolumeCapture(ctx, adapter, VolumeCaptureSpec{DataRef: &pvc}); err != nil {
		t.Fatalf("an undeclared data leg must be a no-op, not an error: %v", err)
	}
	if provider.creates != 0 {
		t.Fatalf("VCRs created while the leg was undeclared = %d, want 0", provider.creates)
	}
	if adapter.domain.VolumeCaptureRequestName != "" {
		t.Fatalf("no request means no published name, got %q", adapter.domain.VolumeCaptureRequestName)
	}

	// The wait ends where it should: once the core declares the leg open, the very same call captures.
	adapter.core = CoreCaptureState{ManifestCaptured: refreshBoolPtr(false), DataCaptured: refreshBoolPtr(false)}
	if err := sdk.EnsureManifestCapture(ctx, adapter, targets); err != nil {
		t.Fatalf("declared open manifest leg: %v", err)
	}
	if n := countMCRs(ctx, t, cl); n != 1 {
		t.Fatalf("MCRs after the core declared the leg = %d, want 1", n)
	}
	if err := sdk.EnsureVolumeCapture(ctx, adapter, VolumeCaptureSpec{DataRef: &pvc}); err != nil {
		t.Fatalf("declared open data leg: %v", err)
	}
	if provider.creates != 1 {
		t.Fatalf("VCRs after the core declared the leg = %d, want 1", provider.creates)
	}
}

// A leftover request from before the status was replaced must not be adopted into the wiped status either:
// the core reaps a request that does not match the published artifact, and re-publishing its name onto a
// status the core is still restoring would resurrect a plan that no longer has an owner.
func TestEnsureManifestCaptureDoesNotAdoptIntoAnUndeclaredLeg(t *testing.T) {
	ctx := context.Background()
	scheme := newRefreshTestScheme(t)

	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "snap-leftover", UID: types.UID("snap-leftover-uid")},
	}
	snap.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))
	leftover := &ssv1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: manifest.RequestName(snap.UID)},
		Spec: ssv1alpha1.ManifestCaptureRequestSpec{
			Targets: []ssv1alpha1.ManifestTarget{{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-1"}},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}).
		WithObjects(snap, leftover).
		Build()

	adapter := &refreshTestAdapter{obj: snap, core: CoreCaptureState{}}
	sdk := New(cl, &countingReader{}, &fakeVolumeProvider{name: "vcr-leftover"})

	if err := sdk.EnsureManifestCapture(ctx, adapter, ManifestCaptureSpec{
		Targets: []ManifestTarget{{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-1"}},
	}); err != nil {
		t.Fatalf("an undeclared leg with a leftover request must be a no-op, not an error: %v", err)
	}
	if adapter.domain.ManifestCaptureRequestName != "" {
		t.Fatalf("a leftover request must not be adopted into a wiped status, got %q", adapter.domain.ManifestCaptureRequestName)
	}
	if n := countMCRs(ctx, t, cl); n != 1 {
		t.Fatalf("the leftover must be left alone (the core reaps it), got %d MCRs", n)
	}
}

// The contract an external caller relies on: a domain that skips its manifest leg on
// !ManifestCaptureNeeded is safe under the three-valued latch, because skipping is the right answer in
// BOTH states the bool collapses — the plan is frozen, or the core has not declared the leg. What it must
// never do is conclude from the skip that the leg is DONE, and it cannot: the outcome the domain switches
// its wait loop on is derived from the latches, and neither an undeclared nor an open leg reads as
// captured. Only a captured latch does.
func TestSkippingOnNeededIsNeverTheSameAsAFinishedLeg(t *testing.T) {
	for _, tc := range []struct {
		name        string
		latch       *bool
		mcrName     string
		wantSkip    bool
		wantOutcome CaptureOutcome
	}{
		{name: "undeclared: skip, still capturing", latch: nil, wantSkip: true, wantOutcome: CaptureOutcomeCapturing},
		{name: "declared open: do not skip, still capturing", latch: refreshBoolPtr(false), wantSkip: false, wantOutcome: CaptureOutcomeCapturing},
		{name: "planned: skip, still capturing", latch: refreshBoolPtr(false), mcrName: "nss-mcr-frozen", wantSkip: true, wantOutcome: CaptureOutcomeCapturing},
		{name: "captured: skip, and only now finished", latch: refreshBoolPtr(true), wantSkip: true, wantOutcome: CaptureOutcomeCaptured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := decisionTestAdapter(tc.latch, tc.mcrName)
			if skip := !ManifestCaptureNeeded(adapter); skip != tc.wantSkip {
				t.Fatalf("skip on !ManifestCaptureNeeded = %v, want %v", skip, tc.wantSkip)
			}
			if got := CoreCaptureOutcome(adapter).Outcome; got != tc.wantOutcome {
				t.Fatalf("CoreCaptureOutcome = %v, want %v", got, tc.wantOutcome)
			}
		})
	}
}
