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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// mockObjectKind is a minimal implementation of schema.ObjectKind for testing
type mockObjectKind struct{}

func (m *mockObjectKind) GroupVersionKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{}
}

func (m *mockObjectKind) SetGroupVersionKind(_ schema.GroupVersionKind) {}

// mockSnapshotLike is a test implementation of SnapshotLike interface
type mockSnapshotLike struct {
	metav1.ObjectMeta
	conditions []metav1.Condition
}

func (m *mockSnapshotLike) GetObjectKind() schema.ObjectKind {
	return &mockObjectKind{}
}

func (m *mockSnapshotLike) DeepCopyObject() runtime.Object {
	// Simple shallow copy for testing
	copied := *m
	return &copied
}

func (m *mockSnapshotLike) GetSpecSnapshotRef() *ObjectRef {
	return nil
}

func (m *mockSnapshotLike) GetStatusContentName() string {
	return ""
}

func (m *mockSnapshotLike) GetStatusManifestCaptureRequestName() string {
	return ""
}

func (m *mockSnapshotLike) GetStatusVolumeCaptureRequestName() string {
	return ""
}

func (m *mockSnapshotLike) GetStatusChildrenSnapshotRefs() []ObjectRef {
	return nil
}

func (m *mockSnapshotLike) GetStatusConditions() []metav1.Condition {
	return m.conditions
}

func (m *mockSnapshotLike) SetStatusConditions(conditions []metav1.Condition) {
	m.conditions = conditions
}

func (m *mockSnapshotLike) GetStatusDataConsistency() string {
	return ""
}

func (m *mockSnapshotLike) GetStatusDataSnapshotMethod() string {
	return ""
}

func (m *mockSnapshotLike) IsNamespaced() bool {
	return true
}

// mockSnapshotContentLike is a test implementation of SnapshotContentLike interface
type mockSnapshotContentLike struct {
	metav1.ObjectMeta
	conditions []metav1.Condition
}

func (m *mockSnapshotContentLike) GetObjectKind() schema.ObjectKind {
	return &mockObjectKind{}
}

func (m *mockSnapshotContentLike) DeepCopyObject() runtime.Object {
	// Simple shallow copy for testing
	copied := *m
	return &copied
}

func (m *mockSnapshotContentLike) GetSpecSnapshotRef() *ObjectRef {
	return nil
}

func (m *mockSnapshotContentLike) GetStatusManifestCheckpointName() string {
	return ""
}

func (m *mockSnapshotContentLike) GetStatusDataRef() *ObjectRef {
	return nil
}

func (m *mockSnapshotContentLike) GetStatusChildrenSnapshotContentRefs() []ObjectRef {
	return nil
}

func (m *mockSnapshotContentLike) GetStatusConditions() []metav1.Condition {
	return m.conditions
}

func (m *mockSnapshotContentLike) SetStatusConditions(conditions []metav1.Condition) {
	m.conditions = conditions
}

func (m *mockSnapshotContentLike) GetStatusDataConsistency() string {
	return ""
}

func (m *mockSnapshotContentLike) GetStatusDataSnapshotMethod() string {
	return ""
}

// Test SetCondition - Idempotency
//
// INTERFACE: pkg/snapshot.SetCondition
//
// PRECONDITION:
// - Object implements SnapshotLike or SnapshotContentLike
// - Condition type exists
//
// ACTIONS:
// 1. SetCondition(obj, "Ready", True, "Completed", "Ready")
// 2. GetCondition(obj, "Ready")
// 3. SetCondition(obj, "Ready", True, "Completed", "Ready") (same values)
// 4. GetCondition(obj, "Ready")
//
// EXPECTED BEHAVIOR:
// - Step 2: Condition exists with status=True
// - Step 4: Condition exists with status=True
// - LastTransitionTime in step 2 == LastTransitionTime in step 4 (not updated)
//
// POSTCONDITION:
// - Condition exists with correct values
// - LastTransitionTime not updated on identical set
func TestSetCondition_Idempotency(t *testing.T) {
	// Test with SnapshotLike
	t.Run("SnapshotLike", func(t *testing.T) {
		obj := &mockSnapshotLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-snapshot"},
			conditions: []metav1.Condition{},
		}

		// Step 1: Set condition first time
		SetCondition(obj, ConditionReady, metav1.ConditionTrue, ReasonReady, "Ready")

		// Step 2: Get condition and record LastTransitionTime
		cond1 := GetCondition(obj, ConditionReady)
		if cond1 == nil {
			t.Fatal("Expected condition to exist after first SetCondition")
		}
		if cond1.Status != metav1.ConditionTrue {
			t.Errorf("Expected status=True, got %v", cond1.Status)
		}
		if cond1.Reason != ReasonReady {
			t.Errorf("Expected reason=%s, got %s", ReasonReady, cond1.Reason)
		}
		// Record LastTransitionTime in local variable to avoid comparing same pointer
		t1 := cond1.LastTransitionTime.Time

		// Small delay to ensure time difference if LastTransitionTime were to be updated
		time.Sleep(10 * time.Millisecond)

		// Step 3: Set same condition again (idempotent call)
		SetCondition(obj, ConditionReady, metav1.ConditionTrue, ReasonReady, "Ready")

		// Step 4: Get condition again and check LastTransitionTime
		cond2 := GetCondition(obj, ConditionReady)
		if cond2 == nil {
			t.Fatal("Expected condition to exist after second SetCondition")
		}
		if cond2.Status != metav1.ConditionTrue {
			t.Errorf("Expected status=True, got %v", cond2.Status)
		}

		// CRITICAL: LastTransitionTime should NOT be updated on identical set
		t2 := cond2.LastTransitionTime.Time
		if !t2.Equal(t1) {
			t.Errorf("Expected LastTransitionTime to remain unchanged, but it was updated: %v -> %v",
				t1, t2)
		}
	})

	// Test with SnapshotContentLike
	t.Run("SnapshotContentLike", func(t *testing.T) {
		obj := &mockSnapshotContentLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-content"},
			conditions: []metav1.Condition{},
		}

		// Step 1: Set condition first time
		SetCondition(obj, ConditionReady, metav1.ConditionTrue, ReasonReady, "Ready")

		// Step 2: Get condition and record LastTransitionTime
		cond1 := GetCondition(obj, ConditionReady)
		if cond1 == nil {
			t.Fatal("Expected condition to exist after first SetCondition")
		}
		// Record LastTransitionTime in local variable to avoid comparing same pointer
		t1 := cond1.LastTransitionTime.Time

		// Small delay
		time.Sleep(10 * time.Millisecond)

		// Step 3: Set same condition again (idempotent call)
		SetCondition(obj, ConditionReady, metav1.ConditionTrue, ReasonReady, "Ready")

		// Step 4: Get condition again and check LastTransitionTime
		cond2 := GetCondition(obj, ConditionReady)
		if cond2 == nil {
			t.Fatal("Expected condition to exist after second SetCondition")
		}

		// CRITICAL: LastTransitionTime should NOT be updated on identical set
		t2 := cond2.LastTransitionTime.Time
		if !t2.Equal(t1) {
			t.Errorf("Expected LastTransitionTime to remain unchanged, but it was updated: %v -> %v",
				t1, t2)
		}
	})
}

// Test SetCondition - Status Change Updates LastTransitionTime
//
// INTERFACE: pkg/snapshot.SetCondition
//
// PRECONDITION:
// - Object implements SnapshotLike or SnapshotContentLike
// - Condition "Ready" exists with status=False
//
// ACTIONS:
// 1. GetCondition(obj, "Ready") → record LastTransitionTime1
// 2. SetCondition(obj, "Ready", True, "Completed", "Ready")
// 3. GetCondition(obj, "Ready") → record LastTransitionTime2
//
// EXPECTED BEHAVIOR:
// - LastTransitionTime2 > LastTransitionTime1 (updated on status change)
//
// POSTCONDITION:
// - Condition status changed from False to True
// - LastTransitionTime updated
func TestSetCondition_StatusChangeUpdatesLastTransitionTime(t *testing.T) {
	// Test with SnapshotLike
	t.Run("SnapshotLike", func(t *testing.T) {
		obj := &mockSnapshotLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-snapshot"},
			conditions: []metav1.Condition{},
		}

		// Set initial condition with status=False
		SetCondition(obj, ConditionReady, metav1.ConditionFalse, "Failed", "Error")

		// Step 1: Get condition and record LastTransitionTime1
		cond1 := GetCondition(obj, ConditionReady)
		if cond1 == nil {
			t.Fatal("Expected condition to exist")
		}
		if cond1.Status != metav1.ConditionFalse {
			t.Errorf("Expected initial status=False, got %v", cond1.Status)
		}
		// Record LastTransitionTime in local variable to avoid comparing same pointer
		t1 := cond1.LastTransitionTime.Time

		// Small delay to ensure time difference
		time.Sleep(10 * time.Millisecond)

		// Step 2: Change status to True
		SetCondition(obj, ConditionReady, metav1.ConditionTrue, ReasonReady, "Ready")

		// Step 3: Get condition and record LastTransitionTime2
		cond2 := GetCondition(obj, ConditionReady)
		if cond2 == nil {
			t.Fatal("Expected condition to exist after status change")
		}
		if cond2.Status != metav1.ConditionTrue {
			t.Errorf("Expected status=True after change, got %v", cond2.Status)
		}

		// CRITICAL: LastTransitionTime SHOULD be updated on status change
		t2 := cond2.LastTransitionTime.Time
		if !t2.After(t1) {
			t.Errorf("Expected LastTransitionTime to be updated on status change, but it was not: %v -> %v",
				t1, t2)
		}
	})

	// Test with SnapshotContentLike
	t.Run("SnapshotContentLike", func(t *testing.T) {
		obj := &mockSnapshotContentLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-content"},
			conditions: []metav1.Condition{},
		}

		// Set initial condition with status=False
		SetCondition(obj, ConditionReady, metav1.ConditionFalse, "Failed", "Error")

		cond1 := GetCondition(obj, ConditionReady)
		// Record LastTransitionTime in local variable to avoid comparing same pointer
		t1 := cond1.LastTransitionTime.Time

		time.Sleep(10 * time.Millisecond)

		// Change status to True
		SetCondition(obj, ConditionReady, metav1.ConditionTrue, ReasonReady, "Ready")

		cond2 := GetCondition(obj, ConditionReady)

		// CRITICAL: LastTransitionTime SHOULD be updated on status change
		t2 := cond2.LastTransitionTime.Time
		if !t2.After(t1) {
			t.Errorf("Expected LastTransitionTime to be updated on status change, but it was not: %v -> %v",
				t1, t2)
		}
	})
}

// Test IsReady - Returns True Only When Ready=True
//
// INTERFACE: pkg/snapshot.IsReady
//
// PRECONDITION:
// - Object implements SnapshotLike or SnapshotContentLike
//
// TEST SCENARIOS:
//
// SCENARIO 1: Ready=True
// - ACTIONS: SetCondition(obj, "Ready", True, "Completed", "Ready")
// - EXPECTED: IsReady(obj) == true
//
// SCENARIO 2: Ready=False
// - ACTIONS: SetCondition(obj, "Ready", False, "Failed", "Error")
// - EXPECTED: IsReady(obj) == false
//
// SCENARIO 3: Ready condition missing
// - ACTIONS: No condition set
// - EXPECTED: IsReady(obj) == false
func TestIsReady_ReturnsTrueOnlyWhenReadyTrue(t *testing.T) {
	t.Run("SnapshotLike", func(t *testing.T) {
		obj := &mockSnapshotLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-snapshot"},
			conditions: []metav1.Condition{},
		}

		// SCENARIO 1: Ready=True
		SetCondition(obj, ConditionReady, metav1.ConditionTrue, ReasonReady, "Ready")
		if !IsReady(obj) {
			t.Error("Expected IsReady() to return true when Ready=True")
		}

		// SCENARIO 2: Ready=False
		SetCondition(obj, ConditionReady, metav1.ConditionFalse, "Failed", "Error")
		if IsReady(obj) {
			t.Error("Expected IsReady() to return false when Ready=False")
		}

		// SCENARIO 3: Ready condition missing
		obj.conditions = []metav1.Condition{}
		if IsReady(obj) {
			t.Error("Expected IsReady() to return false when Ready condition is missing")
		}
	})

	t.Run("SnapshotContentLike", func(t *testing.T) {
		obj := &mockSnapshotContentLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-content"},
			conditions: []metav1.Condition{},
		}

		// SCENARIO 1: Ready=True
		SetCondition(obj, ConditionReady, metav1.ConditionTrue, ReasonReady, "Ready")
		if !IsReady(obj) {
			t.Error("Expected IsReady() to return true when Ready=True")
		}

		// SCENARIO 2: Ready=False
		SetCondition(obj, ConditionReady, metav1.ConditionFalse, "Failed", "Error")
		if IsReady(obj) {
			t.Error("Expected IsReady() to return false when Ready=False")
		}

		// SCENARIO 3: Ready condition missing
		obj.conditions = []metav1.Condition{}
		if IsReady(obj) {
			t.Error("Expected IsReady() to return false when Ready condition is missing")
		}
	})
}

// Test IsInProgress - Returns True Only When InProgress=True
//
// INTERFACE: pkg/snapshot.IsInProgress
//
// PRECONDITION:
// - Object implements SnapshotLike or SnapshotContentLike
//
// TEST SCENARIOS:
//
// SCENARIO 1: InProgress=True
// - ACTIONS: SetCondition(obj, "InProgress", True, "Processing", "In progress")
// - EXPECTED: IsInProgress(obj) == true
//
// SCENARIO 2: InProgress=False
// - ACTIONS: SetCondition(obj, "InProgress", False, "Completed", "Done")
// - EXPECTED: IsInProgress(obj) == false
//
// SCENARIO 3: InProgress condition missing
// - ACTIONS: No condition set
// - EXPECTED: IsInProgress(obj) == false
func TestIsInProgress_ReturnsTrueOnlyWhenInProgressTrue(t *testing.T) {
	t.Run("SnapshotLike", func(t *testing.T) {
		obj := &mockSnapshotLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-snapshot"},
			conditions: []metav1.Condition{},
		}

		// SCENARIO 1: InProgress=True
		SetCondition(obj, ConditionInProgress, metav1.ConditionTrue, "Processing", "In progress")
		if !IsInProgress(obj) {
			t.Error("Expected IsInProgress() to return true when InProgress=True")
		}

		// SCENARIO 2: InProgress=False
		SetCondition(obj, ConditionInProgress, metav1.ConditionFalse, "Completed", "Done")
		if IsInProgress(obj) {
			t.Error("Expected IsInProgress() to return false when InProgress=False")
		}

		// SCENARIO 3: InProgress condition missing
		obj.conditions = []metav1.Condition{}
		if IsInProgress(obj) {
			t.Error("Expected IsInProgress() to return false when InProgress condition is missing")
		}
	})

	t.Run("SnapshotContentLike", func(t *testing.T) {
		obj := &mockSnapshotContentLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-content"},
			conditions: []metav1.Condition{},
		}

		// SCENARIO 1: InProgress=True
		SetCondition(obj, ConditionInProgress, metav1.ConditionTrue, "Processing", "In progress")
		if !IsInProgress(obj) {
			t.Error("Expected IsInProgress() to return true when InProgress=True")
		}

		// SCENARIO 2: InProgress=False
		SetCondition(obj, ConditionInProgress, metav1.ConditionFalse, "Completed", "Done")
		if IsInProgress(obj) {
			t.Error("Expected IsInProgress() to return false when InProgress=False")
		}

		// SCENARIO 3: InProgress condition missing
		obj.conditions = []metav1.Condition{}
		if IsInProgress(obj) {
			t.Error("Expected IsInProgress() to return false when InProgress condition is missing")
		}
	})
}

// Test SetCondition - Edge Cases
//
// Tests edge cases for SetCondition function:
// - nil object
// - object that doesn't implement SnapshotLike or SnapshotContentLike
// - empty condition type
// - multiple conditions
func TestSetCondition_EdgeCases(t *testing.T) {
	t.Run("Nil object", func(_ *testing.T) {
		// EXPECTED BEHAVIOR: no panic, no side effects
		// Should handle nil gracefully without panicking
		SetCondition(nil, ConditionReady, metav1.ConditionTrue, ReasonReady, "Ready")
		// Test passes if no panic occurred
	})

	t.Run("Invalid object type", func(_ *testing.T) {
		// EXPECTED BEHAVIOR: no panic, no side effects
		// Object that doesn't implement SnapshotLike or SnapshotContentLike
		invalidObj := struct{ name string }{name: "test"}
		// Should handle invalid type gracefully without panicking
		SetCondition(invalidObj, ConditionReady, metav1.ConditionTrue, ReasonReady, "Ready")
		// Test passes if no panic occurred
	})

	t.Run("Multiple conditions", func(t *testing.T) {
		obj := &mockSnapshotLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-snapshot"},
			conditions: []metav1.Condition{},
		}

		// Set multiple conditions
		SetCondition(obj, ConditionReady, metav1.ConditionTrue, ReasonReady, "Ready")
		SetCondition(obj, ConditionInProgress, metav1.ConditionTrue, "Processing", "In progress")
		SetCondition(obj, ConditionHandledByCommonController, metav1.ConditionTrue, "Started", "Started")

		// All conditions should exist
		if GetCondition(obj, ConditionReady) == nil {
			t.Error("Expected Ready condition to exist")
		}
		if GetCondition(obj, ConditionInProgress) == nil {
			t.Error("Expected InProgress condition to exist")
		}
		if GetCondition(obj, ConditionHandledByCommonController) == nil {
			t.Error("Expected HandledByCommonController condition to exist")
		}

		// Should have exactly 3 conditions
		conditions := obj.GetStatusConditions()
		if len(conditions) != 3 {
			t.Errorf("Expected 3 conditions, got %d", len(conditions))
		}
	})

	t.Run("Update existing condition", func(t *testing.T) {
		obj := &mockSnapshotLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-snapshot"},
			conditions: []metav1.Condition{},
		}

		// Set condition with initial values
		SetCondition(obj, ConditionReady, metav1.ConditionTrue, ReasonReady, "Initial message")

		cond1 := GetCondition(obj, ConditionReady)
		if cond1.Message != "Initial message" {
			t.Errorf("Expected message='Initial message', got %s", cond1.Message)
		}
		// Record LastTransitionTime in local variable
		t1 := cond1.LastTransitionTime.Time

		// Update condition with new message (same status)
		SetCondition(obj, ConditionReady, metav1.ConditionTrue, ReasonReady, "Updated message")

		cond2 := GetCondition(obj, ConditionReady)
		if cond2.Message != "Updated message" {
			t.Errorf("Expected message='Updated message', got %s", cond2.Message)
		}
		// LastTransitionTime should NOT change (same status)
		t2 := cond2.LastTransitionTime.Time
		if !t2.Equal(t1) {
			t.Error("Expected LastTransitionTime to remain unchanged when updating message with same status")
		}
	})
}

// Test IsTerminal - Returns True Only When Ready=True or Ready=False
//
// INTERFACE: pkg/snapshot.IsTerminal
//
// PRECONDITION:
// - Object implements SnapshotLike or SnapshotContentLike
//
// TEST SCENARIOS:
//
// SCENARIO 1: Ready=True (terminal state)
// - ACTIONS: SetCondition(obj, "Ready", True, "Completed", "Ready")
// - EXPECTED: IsTerminal(obj) == true
//
// SCENARIO 2: Ready=False (terminal state)
// - ACTIONS: SetCondition(obj, "Ready", False, "Failed", "Error")
// - EXPECTED: IsTerminal(obj) == true
//
// SCENARIO 3: Ready condition missing (non-terminal)
// - ACTIONS: No condition set
// - EXPECTED: IsTerminal(obj) == false
//
// SCENARIO 4: Ready=Unknown (non-terminal)
// - ACTIONS: SetCondition(obj, "Ready", Unknown, "Processing", "In progress")
// - EXPECTED: IsTerminal(obj) == false
func TestIsTerminal_ReturnsTrueOnlyWhenReadyTrueOrFalse(t *testing.T) {
	t.Run("SnapshotLike", func(t *testing.T) {
		obj := &mockSnapshotLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-snapshot"},
			conditions: []metav1.Condition{},
		}

		// SCENARIO 1: Ready=True (terminal state)
		SetCondition(obj, ConditionReady, metav1.ConditionTrue, ReasonReady, "Ready")
		if !IsTerminal(obj) {
			t.Error("Expected IsTerminal() to return true when Ready=True")
		}

		// SCENARIO 2: Ready=False (terminal state)
		SetCondition(obj, ConditionReady, metav1.ConditionFalse, "Failed", "Error")
		if !IsTerminal(obj) {
			t.Error("Expected IsTerminal() to return true when Ready=False")
		}

		// SCENARIO 3: Ready condition missing (non-terminal)
		obj.conditions = []metav1.Condition{}
		if IsTerminal(obj) {
			t.Error("Expected IsTerminal() to return false when Ready condition is missing")
		}

		// SCENARIO 4: Ready=Unknown (non-terminal)
		SetCondition(obj, ConditionReady, metav1.ConditionUnknown, "Processing", "In progress")
		if IsTerminal(obj) {
			t.Error("Expected IsTerminal() to return false when Ready=Unknown")
		}
	})

	t.Run("SnapshotContentLike", func(t *testing.T) {
		obj := &mockSnapshotContentLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-content"},
			conditions: []metav1.Condition{},
		}

		// SCENARIO 1: Ready=True (terminal state)
		SetCondition(obj, ConditionReady, metav1.ConditionTrue, ReasonReady, "Ready")
		if !IsTerminal(obj) {
			t.Error("Expected IsTerminal() to return true when Ready=True")
		}

		// SCENARIO 2: Ready=False (terminal state)
		SetCondition(obj, ConditionReady, metav1.ConditionFalse, "Failed", "Error")
		if !IsTerminal(obj) {
			t.Error("Expected IsTerminal() to return true when Ready=False")
		}

		// SCENARIO 3: Ready condition missing (non-terminal)
		obj.conditions = []metav1.Condition{}
		if IsTerminal(obj) {
			t.Error("Expected IsTerminal() to return false when Ready condition is missing")
		}

		// SCENARIO 4: Ready=Unknown (non-terminal)
		SetCondition(obj, ConditionReady, metav1.ConditionUnknown, "Processing", "In progress")
		if IsTerminal(obj) {
			t.Error("Expected IsTerminal() to return false when Ready=Unknown")
		}
	})
}

// Test GetCondition - Edge Cases
//
// INTERFACE: pkg/snapshot.GetCondition
//
// Tests edge cases for GetCondition function:
// - nil object
// - object that doesn't implement SnapshotLike or SnapshotContentLike
// - condition type that doesn't exist
// - multiple conditions (should return correct one)
func TestGetCondition_EdgeCases(t *testing.T) {
	t.Run("Nil object", func(_ *testing.T) {
		// EXPECTED BEHAVIOR: return nil without panic
		cond := GetCondition(nil, ConditionReady)
		if cond != nil {
			t.Error("Expected GetCondition(nil, ...) to return nil")
		}
	})

	t.Run("Invalid object type", func(_ *testing.T) {
		// EXPECTED BEHAVIOR: return nil without panic
		invalidObj := struct{ name string }{name: "test"}
		cond := GetCondition(invalidObj, ConditionReady)
		if cond != nil {
			t.Error("Expected GetCondition(invalidObj, ...) to return nil")
		}
	})

	t.Run("Condition type doesn't exist", func(t *testing.T) {
		obj := &mockSnapshotLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-snapshot"},
			conditions: []metav1.Condition{},
		}
		// Set a different condition
		SetCondition(obj, ConditionInProgress, metav1.ConditionTrue, "Processing", "In progress")
		// Try to get Ready condition (doesn't exist)
		cond := GetCondition(obj, ConditionReady)
		if cond != nil {
			t.Error("Expected GetCondition() to return nil when condition doesn't exist")
		}
	})

	t.Run("Multiple conditions - returns correct one", func(t *testing.T) {
		obj := &mockSnapshotLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-snapshot"},
			conditions: []metav1.Condition{},
		}
		// Set multiple conditions
		SetCondition(obj, ConditionReady, metav1.ConditionTrue, ReasonReady, "Ready")
		SetCondition(obj, ConditionInProgress, metav1.ConditionTrue, "Processing", "In progress")
		SetCondition(obj, ConditionHandledByCommonController, metav1.ConditionTrue, "Started", "Started")

		// Get Ready condition
		readyCond := GetCondition(obj, ConditionReady)
		if readyCond == nil {
			t.Fatal("Expected Ready condition to exist")
		}
		if readyCond.Type != ConditionReady {
			t.Errorf("Expected condition type=%s, got %s", ConditionReady, readyCond.Type)
		}
		if readyCond.Status != metav1.ConditionTrue {
			t.Errorf("Expected Ready condition status=True, got %v", readyCond.Status)
		}

		// Get InProgress condition
		inProgressCond := GetCondition(obj, ConditionInProgress)
		if inProgressCond == nil {
			t.Fatal("Expected InProgress condition to exist")
		}
		if inProgressCond.Type != ConditionInProgress {
			t.Errorf("Expected condition type=%s, got %s", ConditionInProgress, inProgressCond.Type)
		}
	})
}

// Test HasCondition - Edge Cases
//
// INTERFACE: pkg/snapshot.HasCondition
//
// Tests edge cases for HasCondition function:
// - nil object
// - object that doesn't implement SnapshotLike or SnapshotContentLike
// - condition type that doesn't exist
// - condition exists but with different status
// - condition exists with correct status
func TestHasCondition_EdgeCases(t *testing.T) {
	t.Run("Nil object", func(_ *testing.T) {
		// EXPECTED BEHAVIOR: return false without panic
		has := HasCondition(nil, ConditionReady, metav1.ConditionTrue)
		if has {
			t.Error("Expected HasCondition(nil, ...) to return false")
		}
	})

	t.Run("Invalid object type", func(_ *testing.T) {
		// EXPECTED BEHAVIOR: return false without panic
		invalidObj := struct{ name string }{name: "test"}
		has := HasCondition(invalidObj, ConditionReady, metav1.ConditionTrue)
		if has {
			t.Error("Expected HasCondition(invalidObj, ...) to return false")
		}
	})

	t.Run("Condition type doesn't exist", func(t *testing.T) {
		obj := &mockSnapshotLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-snapshot"},
			conditions: []metav1.Condition{},
		}
		// Set a different condition
		SetCondition(obj, ConditionInProgress, metav1.ConditionTrue, "Processing", "In progress")
		// Check for Ready condition (doesn't exist)
		has := HasCondition(obj, ConditionReady, metav1.ConditionTrue)
		if has {
			t.Error("Expected HasCondition() to return false when condition doesn't exist")
		}
	})

	t.Run("Condition exists but with different status", func(t *testing.T) {
		obj := &mockSnapshotLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-snapshot"},
			conditions: []metav1.Condition{},
		}
		// Set Ready condition with status=False
		SetCondition(obj, ConditionReady, metav1.ConditionFalse, "Failed", "Error")
		// Check for Ready=True (should return false)
		has := HasCondition(obj, ConditionReady, metav1.ConditionTrue)
		if has {
			t.Error("Expected HasCondition() to return false when condition exists but with different status")
		}
		// Check for Ready=False (should return true)
		has = HasCondition(obj, ConditionReady, metav1.ConditionFalse)
		if !has {
			t.Error("Expected HasCondition() to return true when condition exists with correct status")
		}
	})

	t.Run("Condition exists with correct status", func(t *testing.T) {
		obj := &mockSnapshotLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-snapshot"},
			conditions: []metav1.Condition{},
		}
		// Set Ready condition with status=True
		SetCondition(obj, ConditionReady, metav1.ConditionTrue, ReasonReady, "Ready")
		// Check for Ready=True (should return true)
		has := HasCondition(obj, ConditionReady, metav1.ConditionTrue)
		if !has {
			t.Error("Expected HasCondition() to return true when condition exists with correct status")
		}
	})

	t.Run("SnapshotContentLike", func(t *testing.T) {
		obj := &mockSnapshotContentLike{
			ObjectMeta: metav1.ObjectMeta{Name: "test-content"},
			conditions: []metav1.Condition{},
		}
		// Set Ready condition
		SetCondition(obj, ConditionReady, metav1.ConditionTrue, ReasonReady, "Ready")
		// Check for Ready=True (should return true)
		has := HasCondition(obj, ConditionReady, metav1.ConditionTrue)
		if !has {
			t.Error("Expected HasCondition() to return true for SnapshotContentLike when condition exists")
		}
	})
}
