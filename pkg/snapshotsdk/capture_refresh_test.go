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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

// These tests pin the capture-request suppression contract that keeps the domain's in-flight poll
// (Reconcile RequeueAfter, firing every few hundred ms while capture runs) off the API server: the SDK
// consults the informer cache first and only pays an uncached read (apiReader) when the request is absent
// and the state is therefore ambiguous ("not created yet" vs "captured, then deleted by the binder"). The
// counting reader stands in for the manager's uncached apiReader so the tests can assert exactly how many
// uncached reads each phase performs, while the fake client is the informer cache.

func refreshBoolPtr(b bool) *bool { return &b }

// refreshTestAdapter is a minimal SnapshotAdapter over a typed Snapshot object. The core leg latches live
// in an in-memory field (as the real adapter derives them from status.captureState.commonController); the
// counting reader flips them to model an authoritative read revealing a completed leg.
type refreshTestAdapter struct {
	obj    *storagev1alpha1.Snapshot
	core   CoreCaptureState
	domain DomainCaptureState
}

func (a *refreshTestAdapter) Object() client.Object                      { return a.obj }
func (a *refreshTestAdapter) SourceRef() SourceRef                       { return SourceRef{} }
func (a *refreshTestAdapter) GetDomainCaptureState() DomainCaptureState  { return a.domain }
func (a *refreshTestAdapter) SetDomainCaptureState(s DomainCaptureState) { a.domain = s }
func (a *refreshTestAdapter) GetSnapshotSource() *SnapshotSource         { return nil }
func (a *refreshTestAdapter) SetSnapshotSource(*SnapshotSource)          {}
func (a *refreshTestAdapter) CoreCaptureState() CoreCaptureState         { return a.core }
func (a *refreshTestAdapter) ReadyStatus() metav1.ConditionStatus        { return "" }
func (a *refreshTestAdapter) ReadyReason() string                        { return "" }
func (a *refreshTestAdapter) ReadyMessage() string                       { return "" }

// countingReader is a client.Reader spy standing in for the uncached apiReader. It counts Get calls and,
// via onGet, models the authoritative read observing a freshly flipped leg latch.
type countingReader struct {
	gets  int
	onGet func()
}

func (r *countingReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	r.gets++
	if r.onGet != nil {
		r.onGet()
	}
	return nil
}

func (r *countingReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return nil
}

// fakeVolumeProvider is an in-memory VolumeCaptureProvider. It models VCR presence (OwnedPVCTarget returns
// non-nil while a request exists) and counts create transitions so a test can assert re-creation never
// happens after the leg is captured.
type fakeVolumeProvider struct {
	name    string
	target  *Target
	creates int
}

func (p *fakeVolumeProvider) VCRName(types.UID) string { return p.name }

func (p *fakeVolumeProvider) EnsureVCR(_ context.Context, _, _ string, _ metav1.OwnerReference, dataRef Target) error {
	if p.target == nil {
		p.creates++
	}
	t := dataRef
	p.target = &t
	return nil
}

func (p *fakeVolumeProvider) OwnedPVCTarget(_ context.Context, _, _ string) (*Target, error) {
	return p.target, nil
}

func newRefreshTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add ss scheme: %v", err)
	}
	return scheme
}

func countMCRs(t *testing.T, ctx context.Context, cl client.Client) int {
	t.Helper()
	list := &ssv1alpha1.ManifestCaptureRequestList{}
	if err := cl.List(ctx, list); err != nil {
		t.Fatalf("list MCRs: %v", err)
	}
	return len(list.Items)
}

func TestEnsureManifestCapture_InFlightSkipsUncachedRead(t *testing.T) {
	ctx := context.Background()
	scheme := newRefreshTestScheme(t)

	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "snap-a", UID: types.UID("snap-a-uid")},
	}
	snap.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}).
		WithObjects(snap).
		Build()

	reader := &countingReader{}
	provider := &fakeVolumeProvider{name: "vcr-a"}
	adapter := &refreshTestAdapter{obj: snap, core: CoreCaptureState{ManifestCaptured: refreshBoolPtr(false)}}

	sdk := New(cl, reader, provider)
	spec := ManifestCaptureSpec{Targets: []ManifestTarget{{APIVersion: "demo/v1alpha1", Kind: "DemoVirtualMachine", Name: "vm-a"}}}

	// Phase 1 — first reconcile: MCR absent in cache, so the SDK must consult the latch once (uncached)
	// before creating, then create the MCR and publish its name.
	if err := sdk.EnsureManifestCapture(ctx, adapter, spec); err != nil {
		t.Fatalf("phase1: %v", err)
	}
	if reader.gets != 1 {
		t.Fatalf("phase1: uncached reads = %d, want 1 (absent MCR must consult the latch before creating)", reader.gets)
	}
	if adapter.domain.ManifestCaptureRequestName == "" {
		t.Fatal("phase1: manifest capture request name was not published")
	}
	if n := countMCRs(t, ctx, cl); n != 1 {
		t.Fatalf("phase1: MCR count = %d, want 1", n)
	}

	// Phase 2 — in-flight polls: MCR present in cache, so no further uncached reads and no duplicate MCR.
	for i := 0; i < 3; i++ {
		if err := sdk.EnsureManifestCapture(ctx, adapter, spec); err != nil {
			t.Fatalf("phase2[%d]: %v", i, err)
		}
	}
	if reader.gets != 1 {
		t.Fatalf("phase2: in-flight polls performed %d uncached reads, want to stay at 1 (cache hit must skip refresh)", reader.gets)
	}
	if n := countMCRs(t, ctx, cl); n != 1 {
		t.Fatalf("phase2: MCR count = %d, want 1 (no duplicate creation)", n)
	}

	// Phase 3 — capture completes: the binder flips the latch and deletes the MCR. A stale poll that still
	// sees the MCR absent must consult the latch once and then suppress (never re-create the captured MCR).
	mcr := &ssv1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: adapter.domain.ManifestCaptureRequestName},
	}
	if err := cl.Delete(ctx, mcr); err != nil {
		t.Fatalf("phase3: delete MCR: %v", err)
	}
	reader.onGet = func() { adapter.core.ManifestCaptured = refreshBoolPtr(true) }

	if err := sdk.EnsureManifestCapture(ctx, adapter, spec); err != nil {
		t.Fatalf("phase3: %v", err)
	}
	if reader.gets != 2 {
		t.Fatalf("phase3: uncached reads = %d, want 2 (absent MCR must consult the latch once)", reader.gets)
	}
	if n := countMCRs(t, ctx, cl); n != 0 {
		t.Fatalf("phase3: MCR count = %d, want 0 (must not re-create a captured request)", n)
	}
}

func TestEnsureVolumeCapture_InFlightSkipsUncachedRead(t *testing.T) {
	ctx := context.Background()
	scheme := newRefreshTestScheme(t)

	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "snap-b", UID: types.UID("snap-b-uid")},
	}
	snap.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}).
		WithObjects(snap).
		Build()

	reader := &countingReader{}
	provider := &fakeVolumeProvider{name: "vcr-b"}
	adapter := &refreshTestAdapter{obj: snap, core: CoreCaptureState{DataCaptured: refreshBoolPtr(false)}}

	sdk := New(cl, reader, provider)
	pvc := Target{UID: "pvc-uid", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-b", Namespace: "ns1"}
	spec := VolumeCaptureSpec{DataRef: &pvc}

	// Phase 1 — first reconcile: VCR absent, so the SDK consults the latch once (uncached) then creates.
	if err := sdk.EnsureVolumeCapture(ctx, adapter, spec); err != nil {
		t.Fatalf("phase1: %v", err)
	}
	if reader.gets != 1 {
		t.Fatalf("phase1: uncached reads = %d, want 1 (absent VCR must consult the latch before creating)", reader.gets)
	}
	if provider.creates != 1 {
		t.Fatalf("phase1: VCR creates = %d, want 1", provider.creates)
	}
	if adapter.domain.VolumeCaptureRequestName != "vcr-b" {
		t.Fatalf("phase1: published VCR name = %q, want %q", adapter.domain.VolumeCaptureRequestName, "vcr-b")
	}

	// Phase 2 — in-flight polls: VCR present, so no further uncached reads and no re-create.
	for i := 0; i < 3; i++ {
		if err := sdk.EnsureVolumeCapture(ctx, adapter, spec); err != nil {
			t.Fatalf("phase2[%d]: %v", i, err)
		}
	}
	if reader.gets != 1 {
		t.Fatalf("phase2: in-flight polls performed %d uncached reads, want to stay at 1 (cache hit must skip refresh)", reader.gets)
	}
	if provider.creates != 1 {
		t.Fatalf("phase2: VCR creates = %d, want 1 (no duplicate creation)", provider.creates)
	}

	// Phase 3 — capture completes: the binder flips the latch and deletes the VCR. A stale poll must
	// consult the latch once and then suppress (never re-create the captured VCR).
	provider.target = nil
	reader.onGet = func() { adapter.core.DataCaptured = refreshBoolPtr(true) }

	if err := sdk.EnsureVolumeCapture(ctx, adapter, spec); err != nil {
		t.Fatalf("phase3: %v", err)
	}
	if reader.gets != 2 {
		t.Fatalf("phase3: uncached reads = %d, want 2 (absent VCR must consult the latch once)", reader.gets)
	}
	if provider.creates != 1 {
		t.Fatalf("phase3: VCR creates = %d, want 1 (must not re-create a captured request)", provider.creates)
	}
}
