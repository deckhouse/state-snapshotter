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

package unifiedbootstrap

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// TestIsOutOfProcessDomainSnapshotKind_csdOrigin guards the CSD-origin restore-delegation rule and its
// regression: the predicate delegates a kind iff it is registered in the live GVK registry AND is not a
// built-in in-process pair. The namespace-root "Snapshot" is a domain-CAPTURE kind, but its restore is
// served in-process by the core apiserver — treating it as out-of-process would delegate the root back to
// core's own endpoint (self-recursion / HTTP 500). The built-in CSI "VolumeSnapshot" restores in-process
// too. A registered non-built-in kind can only have come from an eligible CustomSnapshotDefinition, so it
// is an out-of-process domain (the core carries no domain-specific kind names).
func TestIsOutOfProcessDomainSnapshotKind_csdOrigin(t *testing.T) {
	rootKind := DefaultSnapshotPair().Snapshot.Kind
	vsKind := BuiltInVolumeSnapshotPair().Snapshot.Kind

	// A live registry as it looks once an external domain's CustomSnapshotDefinition is registered: the two
	// built-in pairs plus one CSD-derived domain kind (a synthetic stand-in for the PoC demo or a real
	// virtualization domain — deliberately NOT a demo-specific name).
	const domainKind = "WidgetSnapshot"
	reg := snapshot.NewGVKRegistry()
	if err := reg.RegisterSnapshotGVK(rootKind, "state-snapshotter.deckhouse.io/v1alpha1"); err != nil {
		t.Fatalf("register root: %v", err)
	}
	if err := reg.RegisterSnapshotGVK(vsKind, "snapshot.storage.k8s.io/v1"); err != nil {
		t.Fatalf("register VolumeSnapshot: %v", err)
	}
	if err := reg.RegisterSnapshotGVK(domainKind, "domain.example.com/v1alpha1"); err != nil {
		t.Fatalf("register domain kind: %v", err)
	}

	// Built-in kinds restore in-process — never delegated.
	if IsOutOfProcessDomainSnapshotKind(reg, rootKind) {
		t.Errorf("root %q must NOT be an out-of-process domain restore kind (compiled in-process)", rootKind)
	}
	if IsOutOfProcessDomainSnapshotKind(reg, vsKind) {
		t.Errorf("built-in %q must NOT be an out-of-process domain restore kind (restored in-process)", vsKind)
	}
	// A registered, non-built-in (CSD-derived) kind is an out-of-process domain — delegated.
	if !IsOutOfProcessDomainSnapshotKind(reg, domainKind) {
		t.Errorf("CSD-derived kind %q must be an out-of-process domain restore kind (delegated to its apiserver)", domainKind)
	}
	// Unregistered / nil-registry never delegate (unknown kind).
	if IsOutOfProcessDomainSnapshotKind(reg, "ConfigMap") {
		t.Errorf("an unregistered kind must not be an out-of-process domain restore kind")
	}
	if IsOutOfProcessDomainSnapshotKind(nil, domainKind) {
		t.Errorf("a nil registry must not classify any kind as out-of-process")
	}

	// Sanity: the root is still a domain-CAPTURE kind — the capture and restore predicates diverge on it.
	if !IsDomainCaptureSnapshotKind(rootKind) {
		t.Fatalf("root %q is expected to remain a domain-capture kind (capture planned via the SDK)", rootKind)
	}
}

// TestBuiltInVolumeSnapshotPair_invariants pins the contract the boot-wiring relies on: the built-in
// VolumeSnapshot pair must be data-bearing, use the common SnapshotContent, and — critically — the kind
// must stay OUT of the dedicated/domain-capture lists so FilterGenericSnapshotGVKPairs keeps it (the
// generic binder watches it at boot) and the boot MARK (StartupBuiltInVolumeSnapshotPair) is what defers
// its content shell until the out-of-process domain claim.
func TestBuiltInVolumeSnapshotPair_invariants(t *testing.T) {
	vs := BuiltInVolumeSnapshotPair()
	if vs.Snapshot.Group != "snapshot.storage.k8s.io" || vs.Snapshot.Kind != "VolumeSnapshot" {
		t.Fatalf("built-in VolumeSnapshot pair has wrong GVK: %v", vs.Snapshot)
	}
	if !vs.RequiresDataArtifact {
		t.Fatalf("built-in VolumeSnapshot must be data-bearing (RequiresDataArtifact=true)")
	}
	if vs.SnapshotContent != CommonSnapshotContentGVK() {
		t.Fatalf("built-in VolumeSnapshot must use the common SnapshotContent, got %v", vs.SnapshotContent)
	}

	// Must NOT be dedicated: else FilterGenericSnapshotGVKPairs would strip it and the generic binder would
	// never watch it at boot (the boot mark alone adds no watch for VolumeSnapshot).
	if IsDedicatedSnapshotControllerKind(vs.Snapshot.Kind) {
		t.Fatalf("VolumeSnapshot must not be a dedicated kind (it has no in-process reconciler)")
	}
	// Must NOT be in DomainCaptureSnapshotKinds: that list is a strict subset of the dedicated kinds; its
	// domain-capture status is asserted at boot + on Sync, not via this list.
	if IsDomainCaptureSnapshotKind(vs.Snapshot.Kind) {
		t.Fatalf("VolumeSnapshot must not be in DomainCaptureSnapshotKinds (strict subset of dedicated kinds)")
	}
}

// TestStartupBuiltInVolumeSnapshotPair resolves the VolumeSnapshot pair from parallel slices only when the
// CSI VolumeSnapshot GVK is present (i.e. the CRD is installed), mirroring the root startup helper.
func TestStartupBuiltInVolumeSnapshotPair(t *testing.T) {
	vsSnap := BuiltInVolumeSnapshotPair().Snapshot
	content := CommonSnapshotContentGVK()
	rootSnap := DefaultSnapshotPair().Snapshot

	// Present: VS resolves.
	snap, gotContent, ok := StartupBuiltInVolumeSnapshotPair(
		[]schema.GroupVersionKind{rootSnap, vsSnap},
		[]schema.GroupVersionKind{content, content},
	)
	if !ok || snap != vsSnap || gotContent != content {
		t.Fatalf("expected VolumeSnapshot pair resolved, got snap=%v content=%v ok=%v", snap, gotContent, ok)
	}

	// Absent (CSI CRD not installed): ok=false.
	if _, _, ok := StartupBuiltInVolumeSnapshotPair(
		[]schema.GroupVersionKind{rootSnap},
		[]schema.GroupVersionKind{content},
	); ok {
		t.Fatalf("expected ok=false when VolumeSnapshot is absent from the resolved set")
	}

	// Mismatched slice lengths: ok=false (defensive).
	if _, _, ok := StartupBuiltInVolumeSnapshotPair(
		[]schema.GroupVersionKind{vsSnap},
		nil,
	); ok {
		t.Fatalf("expected ok=false on mismatched slice lengths")
	}
}
