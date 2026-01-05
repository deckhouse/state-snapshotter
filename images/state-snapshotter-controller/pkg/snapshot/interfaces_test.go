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
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// testSnapshotLike is a test implementation of SnapshotLike with mutable state for purity testing
type testSnapshotLike struct {
	metav1.ObjectMeta
	specSnapshotRef              *ObjectRef
	statusContentName             string
	statusManifestCaptureRequest  string
	statusVolumeCaptureRequest    string
	statusChildrenSnapshotRefs    []ObjectRef
	statusConditions              []metav1.Condition
	statusDataConsistency         string
	statusDataSnapshotMethod      string
	isNamespaced                 bool
}

func (t *testSnapshotLike) GetObjectKind() schema.ObjectKind {
	// Use the mockObjectKind from conditions_test.go
	// Since it's in the same package, we can reference it
	return &mockObjectKind{}
}

func (t *testSnapshotLike) DeepCopyObject() runtime.Object {
	copy := *t
	return &copy
}

func (t *testSnapshotLike) GetSpecSnapshotRef() *ObjectRef {
	return t.specSnapshotRef
}

func (t *testSnapshotLike) GetStatusContentName() string {
	return t.statusContentName
}

func (t *testSnapshotLike) GetStatusManifestCaptureRequestName() string {
	return t.statusManifestCaptureRequest
}

func (t *testSnapshotLike) GetStatusVolumeCaptureRequestName() string {
	return t.statusVolumeCaptureRequest
}

func (t *testSnapshotLike) GetStatusChildrenSnapshotRefs() []ObjectRef {
	return t.statusChildrenSnapshotRefs
}

func (t *testSnapshotLike) GetStatusConditions() []metav1.Condition {
	return t.statusConditions
}

func (t *testSnapshotLike) SetStatusConditions(conditions []metav1.Condition) {
	t.statusConditions = conditions
}

func (t *testSnapshotLike) GetStatusDataConsistency() string {
	return t.statusDataConsistency
}

func (t *testSnapshotLike) GetStatusDataSnapshotMethod() string {
	return t.statusDataSnapshotMethod
}

func (t *testSnapshotLike) IsNamespaced() bool {
	return t.isNamespaced
}

// testSnapshotContentLike is a test implementation of SnapshotContentLike with mutable state for purity testing
type testSnapshotContentLike struct {
	metav1.ObjectMeta
	specSnapshotRef                    *ObjectRef
	statusManifestCheckpointName       string
	statusDataRef                      *ObjectRef
	statusChildrenSnapshotContentRefs  []ObjectRef
	statusConditions                   []metav1.Condition
	statusDataConsistency              string
	statusDataSnapshotMethod           string
}

func (t *testSnapshotContentLike) GetObjectKind() schema.ObjectKind {
	// Use the mockObjectKind from conditions_test.go
	return &mockObjectKind{}
}

func (t *testSnapshotContentLike) DeepCopyObject() runtime.Object {
	copy := *t
	return &copy
}

func (t *testSnapshotContentLike) GetSpecSnapshotRef() *ObjectRef {
	return t.specSnapshotRef
}

func (t *testSnapshotContentLike) GetStatusManifestCheckpointName() string {
	return t.statusManifestCheckpointName
}

func (t *testSnapshotContentLike) GetStatusDataRef() *ObjectRef {
	return t.statusDataRef
}

func (t *testSnapshotContentLike) GetStatusChildrenSnapshotContentRefs() []ObjectRef {
	return t.statusChildrenSnapshotContentRefs
}

func (t *testSnapshotContentLike) GetStatusConditions() []metav1.Condition {
	return t.statusConditions
}

func (t *testSnapshotContentLike) SetStatusConditions(conditions []metav1.Condition) {
	t.statusConditions = conditions
}

func (t *testSnapshotContentLike) GetStatusDataConsistency() string {
	return t.statusDataConsistency
}

func (t *testSnapshotContentLike) GetStatusDataSnapshotMethod() string {
	return t.statusDataSnapshotMethod
}

// Test SnapshotLike - Getters Are Pure Functions (Meta-Test)
//
// INTERFACE: pkg/snapshot.SnapshotLike getter methods
//
// PRECONDITION:
// - Object implements SnapshotLike
// - Object has initial state
//
// ACTIONS:
// 1. Record initial state (conditions, refs, etc.)
// 2. Call all getter methods multiple times:
//    - GetSpecSnapshotRef()
//    - GetStatusContentName()
//    - GetStatusChildrenSnapshotRefs()
//    - GetStatusConditions()
//    - GetStatusManifestCaptureRequestName()
//    - GetStatusVolumeCaptureRequestName()
//    - GetStatusDataConsistency()
//    - GetStatusDataSnapshotMethod()
//    - IsNamespaced()
// 3. Record final state
//
// EXPECTED BEHAVIOR:
// - Initial state == final state (no mutations)
// - All getter calls return same values (idempotent)
// - No side effects observed
//
// POSTCONDITION:
// - Object state unchanged after getter calls
// - Getters are pure functions (no mutations, no side effects)
//
// INVARIANT:
// - Getter methods MUST NOT modify object state
// - Getter methods MUST be idempotent
func TestSnapshotLike_GettersArePure(t *testing.T) {
	// Create object with initial state
	initialConditions := []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", Message: "Ready"},
		{Type: "InProgress", Status: metav1.ConditionFalse, Reason: "Completed", Message: "Done"},
	}
	initialChildrenRefs := []ObjectRef{
		{Kind: "VirtualMachineSnapshot", Name: "child-1", Namespace: "default"},
		{Kind: "VirtualMachineSnapshot", Name: "child-2", Namespace: "default"},
	}
	initialSnapshotRef := &ObjectRef{
		Kind:      "VirtualMachineSnapshot",
		Name:      "parent-snapshot",
		Namespace: "default",
	}

	obj := &testSnapshotLike{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		specSnapshotRef:              initialSnapshotRef,
		statusContentName:            "test-content",
		statusManifestCaptureRequest: "test-mcr",
		statusVolumeCaptureRequest:   "test-vcr",
		statusChildrenSnapshotRefs:   initialChildrenRefs,
		statusConditions:              initialConditions,
		statusDataConsistency:        "ApplicationConsistent",
		statusDataSnapshotMethod:      "VolumeSnapshot",
		isNamespaced:                 true,
	}

	// Step 1: Record initial state
	initialConditionsCopy := make([]metav1.Condition, len(obj.statusConditions))
	copy(initialConditionsCopy, obj.statusConditions)
	initialChildrenRefsCopy := make([]ObjectRef, len(obj.statusChildrenSnapshotRefs))
	copy(initialChildrenRefsCopy, obj.statusChildrenSnapshotRefs)

	// Step 2: Call all getter methods multiple times
	// First round of calls
	ref1 := obj.GetSpecSnapshotRef()
	contentName1 := obj.GetStatusContentName()
	mcrName1 := obj.GetStatusManifestCaptureRequestName()
	vcrName1 := obj.GetStatusVolumeCaptureRequestName()
	childrenRefs1 := obj.GetStatusChildrenSnapshotRefs()
	conditions1 := obj.GetStatusConditions()
	dataConsistency1 := obj.GetStatusDataConsistency()
	dataMethod1 := obj.GetStatusDataSnapshotMethod()
	isNamespaced1 := obj.IsNamespaced()

	// Second round of calls (to test idempotency)
	ref2 := obj.GetSpecSnapshotRef()
	contentName2 := obj.GetStatusContentName()
	mcrName2 := obj.GetStatusManifestCaptureRequestName()
	vcrName2 := obj.GetStatusVolumeCaptureRequestName()
	childrenRefs2 := obj.GetStatusChildrenSnapshotRefs()
	conditions2 := obj.GetStatusConditions()
	dataConsistency2 := obj.GetStatusDataConsistency()
	dataMethod2 := obj.GetStatusDataSnapshotMethod()
	isNamespaced2 := obj.IsNamespaced()

	// Third round of calls
	ref3 := obj.GetSpecSnapshotRef()
	childrenRefs3 := obj.GetStatusChildrenSnapshotRefs()
	conditions3 := obj.GetStatusConditions()

	// Step 3: Record final state
	finalConditions := obj.statusConditions
	finalChildrenRefs := obj.statusChildrenSnapshotRefs

	// CRITICAL: Check that initial state == final state (no mutations)
	if !reflect.DeepEqual(finalConditions, initialConditionsCopy) {
		t.Errorf("Conditions were mutated by getter calls. Initial: %v, Final: %v",
			initialConditionsCopy, finalConditions)
	}
	if !reflect.DeepEqual(finalChildrenRefs, initialChildrenRefsCopy) {
		t.Errorf("Children refs were mutated by getter calls. Initial: %v, Final: %v",
			initialChildrenRefsCopy, finalChildrenRefs)
	}

	// CRITICAL: Check that all getter calls return same values (idempotent)
	if !reflect.DeepEqual(ref1, ref2) || !reflect.DeepEqual(ref2, ref3) {
		t.Error("GetSpecSnapshotRef() returned different values on multiple calls (not idempotent)")
	}
	if contentName1 != contentName2 {
		t.Error("GetStatusContentName() returned different values on multiple calls (not idempotent)")
	}
	if mcrName1 != mcrName2 {
		t.Error("GetStatusManifestCaptureRequestName() returned different values on multiple calls (not idempotent)")
	}
	if vcrName1 != vcrName2 {
		t.Error("GetStatusVolumeCaptureRequestName() returned different values on multiple calls (not idempotent)")
	}
	if !reflect.DeepEqual(childrenRefs1, childrenRefs2) || !reflect.DeepEqual(childrenRefs2, childrenRefs3) {
		t.Error("GetStatusChildrenSnapshotRefs() returned different values on multiple calls (not idempotent)")
	}
	if !reflect.DeepEqual(conditions1, conditions2) || !reflect.DeepEqual(conditions2, conditions3) {
		t.Error("GetStatusConditions() returned different values on multiple calls (not idempotent)")
	}
	if dataConsistency1 != dataConsistency2 {
		t.Error("GetStatusDataConsistency() returned different values on multiple calls (not idempotent)")
	}
	if dataMethod1 != dataMethod2 {
		t.Error("GetStatusDataSnapshotMethod() returned different values on multiple calls (not idempotent)")
	}
	if isNamespaced1 != isNamespaced2 {
		t.Error("IsNamespaced() returned different values on multiple calls (not idempotent)")
	}

	// NOTE: Defensive copy check
	// The contract requires that getters don't mutate the object directly.
	// Returning mutable references is acceptable as long as getters themselves don't mutate.
	// This check documents the current behavior - implementations may choose to return copies.
	if len(conditions1) > 0 && len(conditions2) > 0 {
		// Modify returned slice to check if it affects object
		originalMessage := conditions1[0].Message
		conditions1[0].Message = "MODIFIED"
		if !reflect.DeepEqual(obj.statusConditions, initialConditionsCopy) {
			// Implementation returns mutable reference - this is acceptable but documented
			t.Logf("Note: GetStatusConditions() returns mutable reference (implementation detail)")
		}
		// Restore original to avoid affecting other tests
		conditions1[0].Message = originalMessage
	}

	if len(childrenRefs1) > 0 && len(childrenRefs2) > 0 {
		// Modify returned slice to check if it affects object
		originalName := childrenRefs1[0].Name
		childrenRefs1[0].Name = "MODIFIED"
		if !reflect.DeepEqual(obj.statusChildrenSnapshotRefs, initialChildrenRefsCopy) {
			// Implementation returns mutable reference - this is acceptable but documented
			t.Logf("Note: GetStatusChildrenSnapshotRefs() returns mutable reference (implementation detail)")
		}
		// Restore original to avoid affecting other tests
		childrenRefs1[0].Name = originalName
	}
}

// Test SnapshotContentLike - Getters Are Pure Functions (Meta-Test)
//
// INTERFACE: pkg/snapshot.SnapshotContentLike getter methods
//
// PRECONDITION:
// - Object implements SnapshotContentLike
// - Object has initial state
//
// ACTIONS:
// 1. Record initial state (conditions, refs, etc.)
// 2. Call all getter methods multiple times:
//    - GetSpecSnapshotRef()
//    - GetStatusManifestCheckpointName()
//    - GetStatusDataRef()
//    - GetStatusChildrenSnapshotContentRefs()
//    - GetStatusConditions()
//    - GetStatusDataConsistency()
//    - GetStatusDataSnapshotMethod()
// 3. Record final state
//
// EXPECTED BEHAVIOR:
// - Initial state == final state (no mutations)
// - All getter calls return same values (idempotent)
// - No side effects observed
//
// POSTCONDITION:
// - Object state unchanged after getter calls
// - Getters are pure functions (no mutations, no side effects)
//
// INVARIANT:
// - Getter methods MUST NOT modify object state
// - Getter methods MUST be idempotent
func TestSnapshotContentLike_GettersArePure(t *testing.T) {
	// Create object with initial state
	initialConditions := []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", Message: "Ready"},
	}
	initialChildrenRefs := []ObjectRef{
		{Kind: "VirtualMachineSnapshotContent", Name: "child-content-1"},
	}
	initialSnapshotRef := &ObjectRef{
		Kind: "VirtualMachineSnapshot",
		Name: "parent-snapshot",
	}
	initialDataRef := &ObjectRef{
		Kind: "VolumeSnapshotContent",
		Name: "vsc-1",
	}

	obj := &testSnapshotContentLike{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-content",
		},
		specSnapshotRef:                   initialSnapshotRef,
		statusManifestCheckpointName:      "test-mcp",
		statusDataRef:                     initialDataRef,
		statusChildrenSnapshotContentRefs: initialChildrenRefs,
		statusConditions:                  initialConditions,
		statusDataConsistency:             "ApplicationConsistent",
		statusDataSnapshotMethod:          "VolumeSnapshot",
	}

	// Step 1: Record initial state
	initialConditionsCopy := make([]metav1.Condition, len(obj.statusConditions))
	copy(initialConditionsCopy, obj.statusConditions)
	initialChildrenRefsCopy := make([]ObjectRef, len(obj.statusChildrenSnapshotContentRefs))
	copy(initialChildrenRefsCopy, obj.statusChildrenSnapshotContentRefs)

	// Step 2: Call all getter methods multiple times
	// First round of calls
	ref1 := obj.GetSpecSnapshotRef()
	mcpName1 := obj.GetStatusManifestCheckpointName()
	dataRef1 := obj.GetStatusDataRef()
	childrenRefs1 := obj.GetStatusChildrenSnapshotContentRefs()
	conditions1 := obj.GetStatusConditions()
	dataConsistency1 := obj.GetStatusDataConsistency()
	dataMethod1 := obj.GetStatusDataSnapshotMethod()

	// Second round of calls (to test idempotency)
	ref2 := obj.GetSpecSnapshotRef()
	mcpName2 := obj.GetStatusManifestCheckpointName()
	dataRef2 := obj.GetStatusDataRef()
	childrenRefs2 := obj.GetStatusChildrenSnapshotContentRefs()
	conditions2 := obj.GetStatusConditions()
	dataConsistency2 := obj.GetStatusDataConsistency()
	dataMethod2 := obj.GetStatusDataSnapshotMethod()

	// Third round of calls
	ref3 := obj.GetSpecSnapshotRef()
	childrenRefs3 := obj.GetStatusChildrenSnapshotContentRefs()
	conditions3 := obj.GetStatusConditions()

	// Step 3: Record final state
	finalConditions := obj.statusConditions
	finalChildrenRefs := obj.statusChildrenSnapshotContentRefs

	// CRITICAL: Check that initial state == final state (no mutations)
	if !reflect.DeepEqual(finalConditions, initialConditionsCopy) {
		t.Errorf("Conditions were mutated by getter calls. Initial: %v, Final: %v",
			initialConditionsCopy, finalConditions)
	}
	if !reflect.DeepEqual(finalChildrenRefs, initialChildrenRefsCopy) {
		t.Errorf("Children refs were mutated by getter calls. Initial: %v, Final: %v",
			initialChildrenRefsCopy, finalChildrenRefs)
	}

	// CRITICAL: Check that all getter calls return same values (idempotent)
	if !reflect.DeepEqual(ref1, ref2) || !reflect.DeepEqual(ref2, ref3) {
		t.Error("GetSpecSnapshotRef() returned different values on multiple calls (not idempotent)")
	}
	if mcpName1 != mcpName2 {
		t.Error("GetStatusManifestCheckpointName() returned different values on multiple calls (not idempotent)")
	}
	if !reflect.DeepEqual(dataRef1, dataRef2) {
		t.Error("GetStatusDataRef() returned different values on multiple calls (not idempotent)")
	}
	if !reflect.DeepEqual(childrenRefs1, childrenRefs2) || !reflect.DeepEqual(childrenRefs2, childrenRefs3) {
		t.Error("GetStatusChildrenSnapshotContentRefs() returned different values on multiple calls (not idempotent)")
	}
	if !reflect.DeepEqual(conditions1, conditions2) || !reflect.DeepEqual(conditions2, conditions3) {
		t.Error("GetStatusConditions() returned different values on multiple calls (not idempotent)")
	}
	if dataConsistency1 != dataConsistency2 {
		t.Error("GetStatusDataConsistency() returned different values on multiple calls (not idempotent)")
	}
	if dataMethod1 != dataMethod2 {
		t.Error("GetStatusDataSnapshotMethod() returned different values on multiple calls (not idempotent)")
	}

	// NOTE: Defensive copy check
	// The contract requires that getters don't mutate the object directly.
	// Returning mutable references is acceptable as long as getters themselves don't mutate.
	// This check documents the current behavior - implementations may choose to return copies.
	if len(conditions1) > 0 && len(conditions2) > 0 {
		// Modify returned slice to check if it affects object
		originalMessage := conditions1[0].Message
		conditions1[0].Message = "MODIFIED"
		if !reflect.DeepEqual(obj.statusConditions, initialConditionsCopy) {
			// Implementation returns mutable reference - this is acceptable but documented
			t.Logf("Note: GetStatusConditions() returns mutable reference (implementation detail)")
		}
		// Restore original to avoid affecting other tests
		conditions1[0].Message = originalMessage
	}

	if len(childrenRefs1) > 0 && len(childrenRefs2) > 0 {
		// Modify returned slice to check if it affects object
		originalName := childrenRefs1[0].Name
		childrenRefs1[0].Name = "MODIFIED"
		if !reflect.DeepEqual(obj.statusChildrenSnapshotContentRefs, initialChildrenRefsCopy) {
			// Implementation returns mutable reference - this is acceptable but documented
			t.Logf("Note: GetStatusChildrenSnapshotContentRefs() returns mutable reference (implementation detail)")
		}
		// Restore original to avoid affecting other tests
		childrenRefs1[0].Name = originalName
	}
}

// Test SnapshotLike - Getters Do Not Mutate Nil/Empty Values
//
// Tests that getters handle nil and empty values correctly without side effects
func TestSnapshotLike_GettersHandleNilAndEmpty(t *testing.T) {
	obj := &testSnapshotLike{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-snapshot",
		},
		specSnapshotRef:              nil, // nil ref
		statusContentName:            "",   // empty string
		statusManifestCaptureRequest: "",
		statusVolumeCaptureRequest:   "",
		statusChildrenSnapshotRefs:   nil, // nil slice
		statusConditions:             nil, // nil slice
		statusDataConsistency:        "",
		statusDataSnapshotMethod:     "",
		isNamespaced:                 false,
	}

	// Call all getters multiple times - should not panic and return consistent values
	for i := 0; i < 3; i++ {
		ref := obj.GetSpecSnapshotRef()
		if ref != nil {
			t.Errorf("Expected nil ref, got %v", ref)
		}

		contentName := obj.GetStatusContentName()
		if contentName != "" {
			t.Errorf("Expected empty content name, got %s", contentName)
		}

		childrenRefs := obj.GetStatusChildrenSnapshotRefs()
		if childrenRefs != nil && len(childrenRefs) != 0 {
			t.Errorf("Expected nil or empty children refs, got %v", childrenRefs)
		}

		conditions := obj.GetStatusConditions()
		if conditions != nil && len(conditions) != 0 {
			t.Errorf("Expected nil or empty conditions, got %v", conditions)
		}

		isNamespaced := obj.IsNamespaced()
		if isNamespaced != false {
			t.Errorf("Expected IsNamespaced=false, got %v", isNamespaced)
		}
	}
}

// Test SnapshotContentLike - Getters Do Not Mutate Nil/Empty Values
//
// Tests that getters handle nil and empty values correctly without side effects
func TestSnapshotContentLike_GettersHandleNilAndEmpty(t *testing.T) {
	obj := &testSnapshotContentLike{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-content",
		},
		specSnapshotRef:                   nil,
		statusManifestCheckpointName:      "",
		statusDataRef:                     nil,
		statusChildrenSnapshotContentRefs: nil,
		statusConditions:                  nil,
		statusDataConsistency:             "",
		statusDataSnapshotMethod:          "",
	}

	// Call all getters multiple times - should not panic and return consistent values
	for i := 0; i < 3; i++ {
		ref := obj.GetSpecSnapshotRef()
		if ref != nil {
			t.Errorf("Expected nil ref, got %v", ref)
		}

		dataRef := obj.GetStatusDataRef()
		if dataRef != nil {
			t.Errorf("Expected nil data ref, got %v", dataRef)
		}

		childrenRefs := obj.GetStatusChildrenSnapshotContentRefs()
		if childrenRefs != nil && len(childrenRefs) != 0 {
			t.Errorf("Expected nil or empty children refs, got %v", childrenRefs)
		}

		conditions := obj.GetStatusConditions()
		if conditions != nil && len(conditions) != 0 {
			t.Errorf("Expected nil or empty conditions, got %v", conditions)
		}
	}
}

