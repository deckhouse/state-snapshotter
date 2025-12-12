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

package v1alpha1

import (
	"encoding/json"
	"testing"
)

// TestObjectReference_MarshalJSON verifies that ObjectReference correctly serializes and deserializes to/from JSON.
// Checks:
// - Successful marshaling of ObjectReference to JSON
// - Successful unmarshaling of JSON back to ObjectReference
// - All fields (Name, Namespace, UID) are preserved after round-trip through JSON
func TestObjectReference_MarshalJSON(t *testing.T) {
	ref := &ObjectReference{
		Name:      "test-mcr",
		Namespace: "test-namespace",
		UID:       "test-uid-123",
	}

	// Check JSON marshaling
	data, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("Failed to marshal ObjectReference: %v", err)
	}

	// Check JSON unmarshaling
	var unmarshaled ObjectReference
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("Failed to unmarshal ObjectReference: %v", err)
	}

	// Verify all fields are preserved
	if unmarshaled.Name != ref.Name {
		t.Errorf("Expected Name %s, got %s", ref.Name, unmarshaled.Name)
	}
	if unmarshaled.Namespace != ref.Namespace {
		t.Errorf("Expected Namespace %s, got %s", ref.Namespace, unmarshaled.Namespace)
	}
	if unmarshaled.UID != ref.UID {
		t.Errorf("Expected UID %s, got %s", ref.UID, unmarshaled.UID)
	}
}

// TestObjectReference_EmptyFields verifies that ObjectReference with empty fields is handled correctly.
// Checks:
// - Successful marshaling of ObjectReference with empty fields
// - Successful unmarshaling of JSON with empty fields
// - Empty strings are preserved as empty strings (not nil)
func TestObjectReference_EmptyFields(t *testing.T) {
	ref := &ObjectReference{
		Name:      "",
		Namespace: "",
		UID:       "",
	}

	// Check marshaling of empty fields
	data, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("Failed to marshal empty ObjectReference: %v", err)
	}

	// Check unmarshaling of empty fields
	var unmarshaled ObjectReference
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("Failed to unmarshal empty ObjectReference: %v", err)
	}

	// Verify empty fields remain empty
	if unmarshaled.Name != "" {
		t.Errorf("Expected empty Name, got %s", unmarshaled.Name)
	}
	if unmarshaled.Namespace != "" {
		t.Errorf("Expected empty Namespace, got %s", unmarshaled.Namespace)
	}
	if unmarshaled.UID != "" {
		t.Errorf("Expected empty UID, got %s", unmarshaled.UID)
	}
}
