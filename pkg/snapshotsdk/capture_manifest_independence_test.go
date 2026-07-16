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
)

// These tests pin the leg-independence contract: the manifest leg is built SOLELY from the domain's
// declared ManifestCaptureSpec.Targets, and the data leg SOLELY from VolumeCaptureSpec.DataRef. The SDK
// never reads the data-leg VCR to derive/inject manifest targets, so the two Ensure* calls are two
// independent declarations whose call order does not affect the resulting MCR.

// soleMCRTargets runs the given capture sequence against a fresh snapshot/client/SDK and returns the spec
// targets of the single MCR it produced.
func soleMCRTargets(t *testing.T, run func(ctx context.Context, sdk CaptureSDK, adapter *refreshTestAdapter)) []ssv1alpha1.ManifestTarget {
	t.Helper()
	ctx := context.Background()
	scheme := newRefreshTestScheme(t)

	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "snap", UID: types.UID("snap-uid")},
	}
	snap.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}).
		WithObjects(snap).
		Build()

	sdk := New(cl, &countingReader{}, &fakeVolumeProvider{name: "vcr-a"})
	adapter := &refreshTestAdapter{obj: snap, core: CoreCaptureState{
		ManifestCaptured: refreshBoolPtr(false),
		DataCaptured:     refreshBoolPtr(false),
	}}

	run(ctx, sdk, adapter)

	list := &ssv1alpha1.ManifestCaptureRequestList{}
	if err := cl.List(ctx, list); err != nil {
		t.Fatalf("list MCRs: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("want exactly 1 MCR, got %d", len(list.Items))
	}
	return list.Items[0].Spec.Targets
}

func targetsEqual(a, b []ssv1alpha1.ManifestTarget) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var (
	independenceDisk = ManifestTarget{APIVersion: "demo/v1alpha1", Kind: "DemoVirtualDisk", Name: "disk-a"}
	independencePVCT = ManifestTarget{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a"}
	independencePVC  = Target{UID: "pvc-uid", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: "ns1"}
)

// The domain declares BOTH its object and the PVC in the manifest set and captures the PVC data. The
// resulting MCR must be identical whether EnsureManifestCapture runs before or after EnsureVolumeCapture.
func TestManifestCaptureOrderIndependent(t *testing.T) {
	manifestSpec := ManifestCaptureSpec{Targets: []ManifestTarget{independenceDisk, independencePVCT}}
	volumeSpec := VolumeCaptureSpec{DataRef: &independencePVC}
	// deterministic sort: "demo/v1alpha1" < "v1", so the disk sorts before the PVC.
	want := []ssv1alpha1.ManifestTarget{independenceDisk, independencePVCT}

	manifestFirst := soleMCRTargets(t, func(ctx context.Context, sdk CaptureSDK, adapter *refreshTestAdapter) {
		if err := sdk.EnsureManifestCapture(ctx, adapter, manifestSpec); err != nil {
			t.Fatalf("manifest-first: EnsureManifestCapture: %v", err)
		}
		if err := sdk.EnsureVolumeCapture(ctx, adapter, volumeSpec); err != nil {
			t.Fatalf("manifest-first: EnsureVolumeCapture: %v", err)
		}
	})

	volumeFirst := soleMCRTargets(t, func(ctx context.Context, sdk CaptureSDK, adapter *refreshTestAdapter) {
		if err := sdk.EnsureVolumeCapture(ctx, adapter, volumeSpec); err != nil {
			t.Fatalf("volume-first: EnsureVolumeCapture: %v", err)
		}
		if err := sdk.EnsureManifestCapture(ctx, adapter, manifestSpec); err != nil {
			t.Fatalf("volume-first: EnsureManifestCapture: %v", err)
		}
	})

	if !targetsEqual(manifestFirst, want) {
		t.Fatalf("manifest-first MCR targets = %#v, want %#v", manifestFirst, want)
	}
	if !targetsEqual(volumeFirst, want) {
		t.Fatalf("volume-first MCR targets = %#v, want %#v", volumeFirst, want)
	}
	if !targetsEqual(manifestFirst, volumeFirst) {
		t.Fatalf("MCR targets differ by call order: manifest-first=%#v volume-first=%#v", manifestFirst, volumeFirst)
	}
}

// Anti-injection: a data-leg VCR carrying a PVC target must NOT leak into the MCR. The domain declared only
// its own object, so the MCR contains exactly that — the SDK does not derive manifest targets from the VCR.
func TestManifestCaptureIgnoresVolumeCaptureTarget(t *testing.T) {
	manifestSpec := ManifestCaptureSpec{Targets: []ManifestTarget{independenceDisk}}
	volumeSpec := VolumeCaptureSpec{DataRef: &independencePVC}
	want := []ssv1alpha1.ManifestTarget{independenceDisk}

	got := soleMCRTargets(t, func(ctx context.Context, sdk CaptureSDK, adapter *refreshTestAdapter) {
		// Volume capture first, so a VCR with a PVC target definitely exists when the manifest leg runs.
		if err := sdk.EnsureVolumeCapture(ctx, adapter, volumeSpec); err != nil {
			t.Fatalf("EnsureVolumeCapture: %v", err)
		}
		if err := sdk.EnsureManifestCapture(ctx, adapter, manifestSpec); err != nil {
			t.Fatalf("EnsureManifestCapture: %v", err)
		}
	})

	if !targetsEqual(got, want) {
		t.Fatalf("MCR targets = %#v, want %#v (VCR PVC target must not be injected)", got, want)
	}
}

// A manifest-only snapshot (nil DataRef) captures exactly its declared targets and creates no VCR.
func TestManifestCaptureManifestOnly(t *testing.T) {
	manifestSpec := ManifestCaptureSpec{Targets: []ManifestTarget{independenceDisk}}
	want := []ssv1alpha1.ManifestTarget{independenceDisk}

	ctx := context.Background()
	scheme := newRefreshTestScheme(t)
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "snap", UID: types.UID("snap-uid")},
	}
	snap.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}).
		WithObjects(snap).
		Build()
	provider := &fakeVolumeProvider{name: "vcr-a"}
	sdk := New(cl, &countingReader{}, provider)
	adapter := &refreshTestAdapter{obj: snap, core: CoreCaptureState{
		ManifestCaptured: refreshBoolPtr(false),
		DataCaptured:     refreshBoolPtr(false),
	}}

	if err := sdk.EnsureVolumeCapture(ctx, adapter, VolumeCaptureSpec{DataRef: nil}); err != nil {
		t.Fatalf("EnsureVolumeCapture (manifest-only): %v", err)
	}
	if provider.creates != 0 {
		t.Fatalf("manifest-only must create no VCR, got %d creates", provider.creates)
	}
	if err := sdk.EnsureManifestCapture(ctx, adapter, manifestSpec); err != nil {
		t.Fatalf("EnsureManifestCapture (manifest-only): %v", err)
	}

	list := &ssv1alpha1.ManifestCaptureRequestList{}
	if err := cl.List(ctx, list); err != nil {
		t.Fatalf("list MCRs: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("want 1 MCR, got %d", len(list.Items))
	}
	if got := list.Items[0].Spec.Targets; !targetsEqual(got, want) {
		t.Fatalf("manifest-only MCR targets = %#v, want %#v", got, want)
	}
}
