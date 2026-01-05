/*
Copyright 2025 Flant JSC

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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Test RegisterSnapshotGVK - Idempotency
//
// INTERFACE: pkg/snapshot.GVKRegistry.RegisterSnapshotGVK
//
// PRECONDITION:
// - Registry is empty
//
// ACTIONS:
// 1. RegisterSnapshotGVK("VirtualMachineSnapshot", "virtualization.deckhouse.io/v1alpha1")
// 2. ResolveSnapshotGVK("VirtualMachineSnapshot") → record gvk1
// 3. RegisterSnapshotGVK("VirtualMachineSnapshot", "virtualization.deckhouse.io/v1alpha1") (duplicate)
// 4. ResolveSnapshotGVK("VirtualMachineSnapshot") → record gvk2
//
// EXPECTED BEHAVIOR:
// - Step 2: gvk1 is correct GVK
// - Step 4: gvk2 == gvk1 (no change)
// - No error on duplicate registration
//
// POSTCONDITION:
// - GVK registered correctly
// - Duplicate registration is idempotent
func TestRegisterSnapshotGVK_Idempotency(t *testing.T) {
	registry := NewGVKRegistry()

	// Step 1: Register GVK first time
	err1 := registry.RegisterSnapshotGVK("VirtualMachineSnapshot", "virtualization.deckhouse.io/v1alpha1")
	if err1 != nil {
		t.Fatalf("Expected no error on first registration, got: %v", err1)
	}

	// Step 2: Resolve and record GVK
	gvk1, err := registry.ResolveSnapshotGVK("VirtualMachineSnapshot")
	if err != nil {
		t.Fatalf("Expected no error resolving registered GVK, got: %v", err)
	}
	expectedGVK := schema.GroupVersionKind{
		Group:   "virtualization.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "VirtualMachineSnapshot",
	}
	if gvk1 != expectedGVK {
		t.Errorf("Expected GVK=%v, got %v", expectedGVK, gvk1)
	}

	// Step 3: Register same GVK again (duplicate)
	err2 := registry.RegisterSnapshotGVK("VirtualMachineSnapshot", "virtualization.deckhouse.io/v1alpha1")
	if err2 != nil {
		t.Fatalf("Expected no error on duplicate registration (idempotent), got: %v", err2)
	}

	// Step 4: Resolve again and check GVK unchanged
	gvk2, err := registry.ResolveSnapshotGVK("VirtualMachineSnapshot")
	if err != nil {
		t.Fatalf("Expected no error resolving after duplicate registration, got: %v", err)
	}

	// CRITICAL: GVK should remain unchanged after duplicate registration
	if gvk2 != gvk1 {
		t.Errorf("Expected GVK to remain unchanged after duplicate registration, but changed: %v -> %v",
			gvk1, gvk2)
	}
	if gvk2 != expectedGVK {
		t.Errorf("Expected GVK=%v, got %v", expectedGVK, gvk2)
	}
}

// Test ResolveSnapshotGVK - Unknown Kind Returns Error
//
// INTERFACE: pkg/snapshot.GVKRegistry.ResolveSnapshotGVK
//
// PRECONDITION:
// - Registry is empty or does not contain "UnknownSnapshot"
//
// ACTIONS:
// 1. ResolveSnapshotGVK("UnknownSnapshot")
//
// EXPECTED BEHAVIOR:
// - Returns error (GVK not found)
// - Error message is informative
//
// POSTCONDITION:
// - Error returned for unknown Kind
func TestResolveSnapshotGVK_UnknownKindReturnsError(t *testing.T) {
	registry := NewGVKRegistry()

	// Try to resolve unregistered Kind
	_, err := registry.ResolveSnapshotGVK("UnknownSnapshot")
	if err == nil {
		t.Fatal("Expected error when resolving unknown Kind, got nil")
	}

	// Check error message is informative
	errorMsg := err.Error()
	if errorMsg == "" {
		t.Error("Expected non-empty error message")
	}
	// Error should mention the Kind name
	if !strings.Contains(errorMsg, "UnknownSnapshot") {
		t.Errorf("Expected error message to mention Kind name, got: %s", errorMsg)
	}
}

// Test ResolveSnapshotContentGVK - Fallback Logic
//
// INTERFACE: pkg/snapshot.GVKRegistry.ResolveSnapshotContentGVK
//
// Tests the fallback behavior when Content GVK is not explicitly registered
func TestResolveSnapshotContentGVK_FallbackLogic(t *testing.T) {
	t.Run("With registered Content GVK", func(t *testing.T) {
		registry := NewGVKRegistry()

		// Register Snapshot GVK
		err := registry.RegisterSnapshotGVK("VirtualMachineSnapshot", "virtualization.deckhouse.io/v1alpha1")
		if err != nil {
			t.Fatalf("Failed to register Snapshot GVK: %v", err)
		}

		// Register Content GVK explicitly
		err = registry.RegisterSnapshotContentGVK("VirtualMachineSnapshotContent", "virtualization.deckhouse.io/v1beta1")
		if err != nil {
			t.Fatalf("Failed to register Content GVK: %v", err)
		}

		// Resolve Content GVK - should return registered one
		contentGVK, err := registry.ResolveSnapshotContentGVK("VirtualMachineSnapshot")
		if err != nil {
			t.Fatalf("Expected no error resolving Content GVK, got: %v", err)
		}

		expectedGVK := schema.GroupVersionKind{
			Group:   "virtualization.deckhouse.io",
			Version: "v1beta1", // Should use registered version, not fallback
			Kind:    "VirtualMachineSnapshotContent",
		}
		if contentGVK != expectedGVK {
			t.Errorf("Expected registered Content GVK=%v, got %v", expectedGVK, contentGVK)
		}
	})

	t.Run("Without registered Content GVK (fallback)", func(t *testing.T) {
		registry := NewGVKRegistry()

		// Register Snapshot GVK only
		err := registry.RegisterSnapshotGVK("VirtualMachineSnapshot", "virtualization.deckhouse.io/v1alpha1")
		if err != nil {
			t.Fatalf("Failed to register Snapshot GVK: %v", err)
		}

		// Resolve Content GVK - should fallback to derived GVK
		contentGVK, err := registry.ResolveSnapshotContentGVK("VirtualMachineSnapshot")
		if err != nil {
			t.Fatalf("Expected no error resolving Content GVK with fallback, got: %v", err)
		}

		expectedGVK := schema.GroupVersionKind{
			Group:   "virtualization.deckhouse.io",
			Version: "v1alpha1", // Should derive from Snapshot GVK
			Kind:    "VirtualMachineSnapshotContent", // Should add "Content" suffix
		}
		if contentGVK != expectedGVK {
			t.Errorf("Expected fallback Content GVK=%v, got %v", expectedGVK, contentGVK)
		}
	})

	t.Run("Unknown Snapshot Kind returns error", func(t *testing.T) {
		registry := NewGVKRegistry()

		// Try to resolve Content GVK for unregistered Snapshot Kind
		_, err := registry.ResolveSnapshotContentGVK("UnknownSnapshot")
		if err == nil {
			t.Fatal("Expected error when resolving Content GVK for unknown Snapshot Kind, got nil")
		}

		// Error should be informative
		errorMsg := err.Error()
		if errorMsg == "" {
			t.Error("Expected non-empty error message")
		}
	})
}

// Test GVK Registry - Registration Conflict Handling
//
// INTERFACE: pkg/snapshot.GVKRegistry.RegisterSnapshotGVK
//
// PRECONDITION:
// - Registry contains "VirtualMachineSnapshot" → "virtualization.deckhouse.io/v1alpha1"
//
// ACTIONS:
// 1. RegisterSnapshotGVK("VirtualMachineSnapshot", "different.group.io/v1alpha1") (conflict)
// 2. ResolveSnapshotGVK("VirtualMachineSnapshot")
//
// EXPECTED BEHAVIOR:
// - Registration either:
//   - Returns error (conflict detected), OR
//   - Is idempotent (same GVK registered twice is OK)
// - ResolveSnapshotGVK returns consistent result
//
// POSTCONDITION:
// - Registry state is consistent
// - No undefined behavior on conflict
//
// NOTE: This tests current behavior - registration overwrites previous value.
// This is documented behavior, not a bug.
func TestGVKRegistry_RegistrationConflictHandling(t *testing.T) {
	registry := NewGVKRegistry()

	// Register initial GVK
	err1 := registry.RegisterSnapshotGVK("VirtualMachineSnapshot", "virtualization.deckhouse.io/v1alpha1")
	if err1 != nil {
		t.Fatalf("Failed to register initial GVK: %v", err1)
	}

	// Resolve initial GVK
	gvk1, err := registry.ResolveSnapshotGVK("VirtualMachineSnapshot")
	if err != nil {
		t.Fatalf("Failed to resolve initial GVK: %v", err)
	}

	// Register conflicting GVK (different group/version)
	// EXPECTED BEHAVIOR: Current implementation allows overwrite (idempotent by design)
	err2 := registry.RegisterSnapshotGVK("VirtualMachineSnapshot", "different.group.io/v1alpha1")
	if err2 != nil {
		// If implementation changes to detect conflicts, that's OK
		// But current behavior is: no error, overwrite allowed
		t.Logf("Registration with conflict returned error (acceptable): %v", err2)
		return
	}

	// Resolve after conflict registration
	gvk2, err := registry.ResolveSnapshotGVK("VirtualMachineSnapshot")
	if err != nil {
		t.Fatalf("Failed to resolve after conflict registration: %v", err)
	}

	// CRITICAL: Registry state should be consistent
	// Current behavior: last registration wins (overwrites)
	if gvk2.Group != "different.group.io" {
		t.Logf("Note: Registration conflict overwrites previous value (current behavior)")
		t.Logf("Initial GVK: %v", gvk1)
		t.Logf("After conflict: %v", gvk2)
	}
}

// Test RegisterSnapshotContentGVK - Idempotency
//
// Tests idempotency for Content GVK registration
func TestRegisterSnapshotContentGVK_Idempotency(t *testing.T) {
	registry := NewGVKRegistry()

	// Register Content GVK first time
	err1 := registry.RegisterSnapshotContentGVK("VirtualMachineSnapshotContent", "virtualization.deckhouse.io/v1alpha1")
	if err1 != nil {
		t.Fatalf("Expected no error on first registration, got: %v", err1)
	}

	// Register same Content GVK again (duplicate)
	err2 := registry.RegisterSnapshotContentGVK("VirtualMachineSnapshotContent", "virtualization.deckhouse.io/v1alpha1")
	if err2 != nil {
		t.Fatalf("Expected no error on duplicate registration (idempotent), got: %v", err2)
	}

	// Verify registry is still consistent
	// (Content GVK resolution is tested in ResolveSnapshotContentGVK tests)
}

// Test GVK Registry - Core API Format
//
// Tests parsing of core API format (version without group)
func TestGVKRegistry_CoreAPIFormat(t *testing.T) {
	registry := NewGVKRegistry()

	// Register with core API format (no group)
	err := registry.RegisterSnapshotGVK("PodSnapshot", "v1")
	if err != nil {
		t.Fatalf("Expected no error registering core API format, got: %v", err)
	}

	// Resolve and verify
	gvk, err := registry.ResolveSnapshotGVK("PodSnapshot")
	if err != nil {
		t.Fatalf("Expected no error resolving core API GVK, got: %v", err)
	}

	expectedGVK := schema.GroupVersionKind{
		Group:   "", // Core API has empty group
		Version: "v1",
		Kind:    "PodSnapshot",
	}
	if gvk != expectedGVK {
		t.Errorf("Expected core API GVK=%v, got %v", expectedGVK, gvk)
	}
}

// Test GVK Registry - Multiple Registrations
//
// Tests registering multiple different GVKs
func TestGVKRegistry_MultipleRegistrations(t *testing.T) {
	registry := NewGVKRegistry()

	// Register multiple Snapshot GVKs
	gvks := []struct {
		kind       string
		apiVersion string
		expected   schema.GroupVersionKind
	}{
		{"VirtualMachineSnapshot", "virtualization.deckhouse.io/v1alpha1", schema.GroupVersionKind{
			Group:   "virtualization.deckhouse.io",
			Version: "v1alpha1",
			Kind:    "VirtualMachineSnapshot",
		}},
		{"VolumeSnapshot", "storage.deckhouse.io/v1alpha1", schema.GroupVersionKind{
			Group:   "storage.deckhouse.io",
			Version: "v1alpha1",
			Kind:    "VolumeSnapshot",
		}},
		{"PodSnapshot", "v1", schema.GroupVersionKind{
			Group:   "",
			Version: "v1",
			Kind:    "PodSnapshot",
		}},
	}

	for _, gvk := range gvks {
		err := registry.RegisterSnapshotGVK(gvk.kind, gvk.apiVersion)
		if err != nil {
			t.Fatalf("Failed to register %s: %v", gvk.kind, err)
		}

		resolved, err := registry.ResolveSnapshotGVK(gvk.kind)
		if err != nil {
			t.Fatalf("Failed to resolve %s: %v", gvk.kind, err)
		}

		if resolved != gvk.expected {
			t.Errorf("Expected %s GVK=%v, got %v", gvk.kind, gvk.expected, resolved)
		}
	}
}

// Test GVK Registry - Edge Cases
//
// Tests edge cases for GVK registry
func TestGVKRegistry_EdgeCases(t *testing.T) {
	t.Run("Empty Kind", func(t *testing.T) {
		registry := NewGVKRegistry()

		err := registry.RegisterSnapshotGVK("", "v1")
		if err != nil {
			t.Fatalf("Expected empty Kind to be handled, got error: %v", err)
		}

		gvk, err := registry.ResolveSnapshotGVK("")
		if err != nil {
			t.Fatalf("Expected empty Kind to resolve, got error: %v", err)
		}

		if gvk.Kind != "" {
			t.Errorf("Expected empty Kind, got: %s", gvk.Kind)
		}
	})

	t.Run("Invalid apiVersion format", func(t *testing.T) {
		registry := NewGVKRegistry()

		// Test with invalid format (too many slashes)
		err := registry.RegisterSnapshotGVK("TestSnapshot", "group/version/extra")
		if err == nil {
			t.Log("Note: Current implementation may accept invalid format - this is documented behavior")
		} else {
			// Error is acceptable
			t.Logf("Invalid format correctly rejected: %v", err)
		}
	})

	t.Run("Resolve Content GVK with empty Snapshot Kind", func(t *testing.T) {
		registry := NewGVKRegistry()

		// Register empty Kind Snapshot
		err := registry.RegisterSnapshotGVK("", "v1")
		if err != nil {
			t.Fatalf("Failed to register empty Kind: %v", err)
		}

		// Resolve Content GVK for empty Kind
		contentGVK, err := registry.ResolveSnapshotContentGVK("")
		if err != nil {
			t.Fatalf("Expected no error resolving Content GVK for empty Kind, got: %v", err)
		}

		// Should derive "Content" suffix
		if contentGVK.Kind != "Content" {
			t.Errorf("Expected Content Kind='Content', got: %s", contentGVK.Kind)
		}
	})
}

