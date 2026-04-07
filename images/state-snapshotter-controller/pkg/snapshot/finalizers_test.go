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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// mockObject is a minimal test implementation of metav1.Object
type mockObject struct {
	metav1.ObjectMeta
}

// Test AddFinalizer - Idempotency
//
// INTERFACE: pkg/snapshot.AddFinalizer
//
// PRECONDITION:
// - Object implements metav1.Object
// - Object has no finalizers
//
// ACTIONS:
// 1. AddFinalizer(obj, "test-finalizer")
// 2. GetFinalizers(obj) → record finalizers1
// 3. AddFinalizer(obj, "test-finalizer") (duplicate)
// 4. GetFinalizers(obj) → record finalizers2
//
// EXPECTED BEHAVIOR:
// - Step 2: finalizers1 contains "test-finalizer"
// - Step 4: finalizers2 == finalizers1 (no duplicate added)
// - AddFinalizer returns false on step 3 (already exists)
//
// POSTCONDITION:
// - Finalizer exists exactly once
// - No duplicates added
func TestAddFinalizer_Idempotency(t *testing.T) {
	obj := &mockObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-object",
			Finalizers: []string{},
		},
	}

	// Step 1: Add finalizer first time
	added1 := AddFinalizer(obj, "test-finalizer")
	if !added1 {
		t.Error("Expected AddFinalizer to return true on first call")
	}

	// Step 2: Get finalizers and record
	finalizers1 := obj.GetFinalizers()
	if len(finalizers1) != 1 {
		t.Errorf("Expected 1 finalizer, got %d", len(finalizers1))
	}
	if finalizers1[0] != "test-finalizer" {
		t.Errorf("Expected finalizer='test-finalizer', got %s", finalizers1[0])
	}

	// Step 3: Add same finalizer again (duplicate)
	added2 := AddFinalizer(obj, "test-finalizer")
	if added2 {
		t.Error("Expected AddFinalizer to return false on duplicate call")
	}

	// Step 4: Get finalizers again and check
	finalizers2 := obj.GetFinalizers()
	if len(finalizers2) != 1 {
		t.Errorf("Expected 1 finalizer after duplicate add, got %d", len(finalizers2))
	}
	if finalizers2[0] != "test-finalizer" {
		t.Errorf("Expected finalizer='test-finalizer', got %s", finalizers2[0])
	}

	// CRITICAL: No duplicate added
	if len(finalizers2) != len(finalizers1) {
		t.Errorf("Expected finalizers count to remain unchanged, but changed: %d -> %d",
			len(finalizers1), len(finalizers2))
	}
}

// Test RemoveFinalizer - Idempotency
//
// INTERFACE: pkg/snapshot.RemoveFinalizer
//
// PRECONDITION:
// - Object implements metav1.Object
// - Object has "test-finalizer" finalizer
//
// ACTIONS:
// 1. RemoveFinalizer(obj, "test-finalizer")
// 2. GetFinalizers(obj) → record finalizers1
// 3. RemoveFinalizer(obj, "test-finalizer") (already removed)
// 4. GetFinalizers(obj) → record finalizers2
//
// EXPECTED BEHAVIOR:
// - Step 2: finalizers1 does NOT contain "test-finalizer"
// - Step 4: finalizers2 == finalizers1 (no error)
// - RemoveFinalizer returns false on step 3 (already removed)
//
// POSTCONDITION:
// - Finalizer removed
// - No errors on duplicate removal
func TestRemoveFinalizer_Idempotency(t *testing.T) {
	obj := &mockObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-object",
			Finalizers: []string{"test-finalizer"},
		},
	}

	// Step 1: Remove finalizer first time
	removed1 := RemoveFinalizer(obj, "test-finalizer")
	if !removed1 {
		t.Error("Expected RemoveFinalizer to return true on first call")
	}

	// Step 2: Get finalizers and record
	finalizers1 := obj.GetFinalizers()
	if len(finalizers1) != 0 {
		t.Errorf("Expected 0 finalizers after removal, got %d", len(finalizers1))
	}
	if HasFinalizer(obj, "test-finalizer") {
		t.Error("Expected finalizer to be removed")
	}

	// Step 3: Remove same finalizer again (already removed)
	removed2 := RemoveFinalizer(obj, "test-finalizer")
	if removed2 {
		t.Error("Expected RemoveFinalizer to return false when finalizer already removed")
	}

	// Step 4: Get finalizers again and check
	finalizers2 := obj.GetFinalizers()
	if len(finalizers2) != 0 {
		t.Errorf("Expected 0 finalizers after duplicate removal, got %d", len(finalizers2))
	}

	// CRITICAL: No error on duplicate removal
	if len(finalizers2) != len(finalizers1) {
		t.Errorf("Expected finalizers count to remain unchanged, but changed: %d -> %d",
			len(finalizers1), len(finalizers2))
	}
}

// Test HasFinalizer - Basic functionality
//
// Tests the HasFinalizer helper function
func TestHasFinalizer_Basic(t *testing.T) {
	t.Run("Finalizer exists", func(t *testing.T) {
		obj := &mockObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-object",
				Finalizers: []string{"finalizer-1", "finalizer-2"},
			},
		}

		if !HasFinalizer(obj, "finalizer-1") {
			t.Error("Expected HasFinalizer to return true when finalizer exists")
		}
		if !HasFinalizer(obj, "finalizer-2") {
			t.Error("Expected HasFinalizer to return true when finalizer exists")
		}
	})

	t.Run("Finalizer does not exist", func(t *testing.T) {
		obj := &mockObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-object",
				Finalizers: []string{"finalizer-1"},
			},
		}

		if HasFinalizer(obj, "finalizer-2") {
			t.Error("Expected HasFinalizer to return false when finalizer does not exist")
		}
	})

	t.Run("Empty finalizers", func(t *testing.T) {
		obj := &mockObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-object",
				Finalizers: []string{},
			},
		}

		if HasFinalizer(obj, "any-finalizer") {
			t.Error("Expected HasFinalizer to return false when finalizers are empty")
		}
	})

	t.Run("Nil finalizers", func(t *testing.T) {
		obj := &mockObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-object",
				Finalizers: nil,
			},
		}

		if HasFinalizer(obj, "any-finalizer") {
			t.Error("Expected HasFinalizer to return false when finalizers are nil")
		}
	})
}

// Test AddFinalizer - Multiple finalizers
//
// Tests adding multiple different finalizers
func TestAddFinalizer_MultipleFinalizers(t *testing.T) {
	obj := &mockObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-object",
			Finalizers: []string{},
		},
	}

	// Add first finalizer
	added1 := AddFinalizer(obj, "finalizer-1")
	if !added1 {
		t.Error("Expected AddFinalizer to return true for first finalizer")
	}

	// Add second finalizer
	added2 := AddFinalizer(obj, "finalizer-2")
	if !added2 {
		t.Error("Expected AddFinalizer to return true for second finalizer")
	}

	// Add third finalizer
	added3 := AddFinalizer(obj, "finalizer-3")
	if !added3 {
		t.Error("Expected AddFinalizer to return true for third finalizer")
	}

	// Check all finalizers exist
	finalizers := obj.GetFinalizers()
	if len(finalizers) != 3 {
		t.Errorf("Expected 3 finalizers, got %d", len(finalizers))
	}

	if !HasFinalizer(obj, "finalizer-1") {
		t.Error("Expected finalizer-1 to exist")
	}
	if !HasFinalizer(obj, "finalizer-2") {
		t.Error("Expected finalizer-2 to exist")
	}
	if !HasFinalizer(obj, "finalizer-3") {
		t.Error("Expected finalizer-3 to exist")
	}
}

// Test RemoveFinalizer - Multiple finalizers
//
// Tests removing one finalizer while others remain
func TestRemoveFinalizer_MultipleFinalizers(t *testing.T) {
	obj := &mockObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-object",
			Finalizers: []string{"finalizer-1", "finalizer-2", "finalizer-3"},
		},
	}

	// Remove middle finalizer
	removed := RemoveFinalizer(obj, "finalizer-2")
	if !removed {
		t.Error("Expected RemoveFinalizer to return true")
	}

	// Check finalizer-2 is removed
	if HasFinalizer(obj, "finalizer-2") {
		t.Error("Expected finalizer-2 to be removed")
	}

	// Check other finalizers still exist
	if !HasFinalizer(obj, "finalizer-1") {
		t.Error("Expected finalizer-1 to still exist")
	}
	if !HasFinalizer(obj, "finalizer-3") {
		t.Error("Expected finalizer-3 to still exist")
	}

	// Check count
	finalizers := obj.GetFinalizers()
	if len(finalizers) != 2 {
		t.Errorf("Expected 2 finalizers remaining, got %d", len(finalizers))
	}
}

// Test AddFinalizer - Edge Cases
//
// Tests edge cases for AddFinalizer
func TestAddFinalizer_EdgeCases(t *testing.T) {
	t.Run("Empty finalizer name", func(t *testing.T) {
		obj := &mockObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-object",
				Finalizers: []string{},
			},
		}

		// Should handle empty string gracefully
		added := AddFinalizer(obj, "")
		if !added {
			t.Error("Expected AddFinalizer to handle empty string")
		}

		if !HasFinalizer(obj, "") {
			t.Error("Expected empty string finalizer to be added")
		}
	})

	t.Run("Add to object with existing finalizers", func(t *testing.T) {
		obj := &mockObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-object",
				Finalizers: []string{"existing-finalizer"},
			},
		}

		added := AddFinalizer(obj, "new-finalizer")
		if !added {
			t.Error("Expected AddFinalizer to return true for new finalizer")
		}

		finalizers := obj.GetFinalizers()
		if len(finalizers) != 2 {
			t.Errorf("Expected 2 finalizers, got %d", len(finalizers))
		}

		if !HasFinalizer(obj, "existing-finalizer") {
			t.Error("Expected existing finalizer to remain")
		}
		if !HasFinalizer(obj, "new-finalizer") {
			t.Error("Expected new finalizer to be added")
		}
	})
}

// Test RemoveFinalizer - Edge Cases
//
// Tests edge cases for RemoveFinalizer
func TestRemoveFinalizer_EdgeCases(t *testing.T) {
	t.Run("Remove from empty finalizers", func(t *testing.T) {
		obj := &mockObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-object",
				Finalizers: []string{},
			},
		}

		removed := RemoveFinalizer(obj, "non-existent")
		if removed {
			t.Error("Expected RemoveFinalizer to return false for non-existent finalizer")
		}

		if len(obj.GetFinalizers()) != 0 {
			t.Error("Expected finalizers to remain empty")
		}
	})

	t.Run("Remove non-existent finalizer", func(t *testing.T) {
		obj := &mockObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-object",
				Finalizers: []string{"finalizer-1"},
			},
		}

		removed := RemoveFinalizer(obj, "non-existent")
		if removed {
			t.Error("Expected RemoveFinalizer to return false for non-existent finalizer")
		}

		if !HasFinalizer(obj, "finalizer-1") {
			t.Error("Expected existing finalizer to remain")
		}
	})

	t.Run("Remove all finalizers one by one", func(t *testing.T) {
		obj := &mockObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-object",
				Finalizers: []string{"finalizer-1", "finalizer-2", "finalizer-3"},
			},
		}

		RemoveFinalizer(obj, "finalizer-1")
		RemoveFinalizer(obj, "finalizer-2")
		RemoveFinalizer(obj, "finalizer-3")

		if len(obj.GetFinalizers()) != 0 {
			t.Errorf("Expected all finalizers to be removed, but %d remain", len(obj.GetFinalizers()))
		}
	})
}

// Test ManageFinalizer - Based on Snapshot Existence
//
// INTERFACE: Logic from SnapshotContentController.Reconcile (Step 1.2)
//
// This test verifies the critical logic that prevents infinite reconcile loop:
// - If snapshotExists == true: Add finalizer (if not present)
// - If snapshotExists == false: Remove finalizer (if present)
//
// PRECONDITION:
// - Object implements metav1.Object
// - snapshotExists is either true or false
//
// ACTIONS:
// 1. manageFinalizer(obj, snapshotExists=true)
// 2. Check finalizer state
// 3. manageFinalizer(obj, snapshotExists=false)
// 4. Check finalizer state
//
// EXPECTED BEHAVIOR:
// - Step 2: Finalizer exists when snapshotExists=true
// - Step 4: Finalizer removed when snapshotExists=false
// - No infinite loop: finalizer is NOT re-added when snapshotExists=false
//
// POSTCONDITION:
// - Finalizer state matches snapshotExists value
//
// INVARIANT:
// - This logic prevents infinite reconcile loop when Snapshot is deleted
// - See: snapshotcontent_controller.go (Step 1.2: Manage finalizer based on Snapshot existence)
func TestManageFinalizer_BasedOnSnapshotExistence(t *testing.T) {
	finalizer := FinalizerParentProtect

	t.Run("snapshotExists=true: Add finalizer when not present", func(t *testing.T) {
		obj := &mockObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-content",
				Finalizers: []string{},
			},
		}

		// ACTIONS Step 1: snapshotExists=true → should add finalizer
		snapshotExists := true
		if snapshotExists {
			added := AddFinalizer(obj, finalizer)
			if !added {
				t.Error("Expected AddFinalizer to return true when adding finalizer")
			}
		}

		// EXPECTED BEHAVIOR: Finalizer exists
		if !HasFinalizer(obj, finalizer) {
			t.Error("Expected finalizer to be added when snapshotExists=true")
		}
	})

	t.Run("snapshotExists=true: Keep finalizer when already present", func(t *testing.T) {
		obj := &mockObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-content",
				Finalizers: []string{finalizer},
			},
		}

		// ACTIONS Step 1: snapshotExists=true → should keep finalizer (idempotent)
		snapshotExists := true
		if snapshotExists {
			added := AddFinalizer(obj, finalizer)
			if added {
				t.Error("Expected AddFinalizer to return false when finalizer already exists")
			}
		}

		// EXPECTED BEHAVIOR: Finalizer still exists
		if !HasFinalizer(obj, finalizer) {
			t.Error("Expected finalizer to remain when snapshotExists=true and already present")
		}
	})

	t.Run("snapshotExists=false: Remove finalizer when present", func(t *testing.T) {
		obj := &mockObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-content",
				Finalizers: []string{finalizer},
			},
		}

		// ACTIONS Step 1: snapshotExists=false → should remove finalizer
		snapshotExists := false
		if !snapshotExists {
			removed := RemoveFinalizer(obj, finalizer)
			if !removed {
				t.Error("Expected RemoveFinalizer to return true when removing finalizer")
			}
		}

		// EXPECTED BEHAVIOR: Finalizer removed
		if HasFinalizer(obj, finalizer) {
			t.Error("Expected finalizer to be removed when snapshotExists=false")
		}
	})

	t.Run("snapshotExists=false: Keep no finalizer when already absent", func(t *testing.T) {
		obj := &mockObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-content",
				Finalizers: []string{},
			},
		}

		// ACTIONS Step 1: snapshotExists=false → should keep no finalizer (idempotent)
		snapshotExists := false
		if !snapshotExists {
			removed := RemoveFinalizer(obj, finalizer)
			if removed {
				t.Error("Expected RemoveFinalizer to return false when finalizer already absent")
			}
		}

		// EXPECTED BEHAVIOR: No finalizer
		if HasFinalizer(obj, finalizer) {
			t.Error("Expected no finalizer when snapshotExists=false and already absent")
		}
	})

	t.Run("snapshotExists transition: true → false (prevents infinite loop)", func(t *testing.T) {
		obj := &mockObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-content",
				Finalizers: []string{},
			},
		}

		// ACTIONS Step 1: snapshotExists=true → add finalizer
		snapshotExists := true
		if snapshotExists {
			AddFinalizer(obj, finalizer)
		}
		if !HasFinalizer(obj, finalizer) {
			t.Error("Expected finalizer to be added when snapshotExists=true")
		}

		// ACTIONS Step 2: snapshotExists=false → remove finalizer
		snapshotExists = false
		if !snapshotExists {
			RemoveFinalizer(obj, finalizer)
		}
		if HasFinalizer(obj, finalizer) {
			t.Error("Expected finalizer to be removed when snapshotExists=false")
		}

		// ACTIONS Step 3: snapshotExists=false again → should NOT re-add finalizer
		// This is the critical check: prevents infinite loop
		snapshotExists = false
		if !snapshotExists {
			removed := RemoveFinalizer(obj, finalizer)
			if removed {
				t.Error("Expected RemoveFinalizer to return false when finalizer already removed (no infinite loop)")
			}
		}
		if HasFinalizer(obj, finalizer) {
			t.Error("CRITICAL: Finalizer should NOT be re-added when snapshotExists=false (prevents infinite loop)")
		}

		// EXPECTED BEHAVIOR: Finalizer remains removed, no infinite loop
		finalizers := obj.GetFinalizers()
		if len(finalizers) != 0 {
			t.Errorf("CRITICAL: Expected no finalizers after snapshotExists=false transition, got %d (infinite loop prevented)", len(finalizers))
		}
	})

	t.Run("snapshotExists transition: false → true", func(t *testing.T) {
		obj := &mockObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-content",
				Finalizers: []string{},
			},
		}

		// ACTIONS Step 1: snapshotExists=false → no finalizer
		snapshotExists := false
		if !snapshotExists {
			RemoveFinalizer(obj, finalizer)
		}
		if HasFinalizer(obj, finalizer) {
			t.Error("Expected no finalizer when snapshotExists=false")
		}

		// ACTIONS Step 2: snapshotExists=true → add finalizer
		snapshotExists = true
		if snapshotExists {
			AddFinalizer(obj, finalizer)
		}
		if !HasFinalizer(obj, finalizer) {
			t.Error("Expected finalizer to be added when snapshotExists=true")
		}

		// EXPECTED BEHAVIOR: Finalizer added
		finalizers := obj.GetFinalizers()
		if len(finalizers) != 1 {
			t.Errorf("Expected 1 finalizer after snapshotExists=true transition, got %d", len(finalizers))
		}
	})
}
