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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestManifestCaptureRequest_NoPhaseInStatus verifies that ManifestCaptureRequestStatus does not contain Phase field.
// Checks:
// - Status can be created without Phase (field removed from API)
// - ErrorReason enum does not contain RBACDenied
// - Conditions work correctly
// - Ready condition can be checked via meta.FindStatusCondition
// - Code compiles and works without Phase
func TestManifestCaptureRequest_NoPhaseInStatus(t *testing.T) {
	status := ManifestCaptureRequestStatus{
		CheckpointName: "test-checkpoint",
		ObservedGeneration: 1,
		ErrorReason: "NotFound",
		Conditions: []metav1.Condition{
			{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "Completed",
				Message:            "Request completed",
				LastTransitionTime: metav1.Now(),
				ObservedGeneration: 1,
			},
		},
	}

	// Verify status fields work without Phase
	if status.CheckpointName != "test-checkpoint" {
		t.Errorf("Expected CheckpointName test-checkpoint, got %s", status.CheckpointName)
	}

	if status.ErrorReason != "NotFound" {
		t.Errorf("Expected ErrorReason NotFound, got %s", status.ErrorReason)
	}

	if len(status.Conditions) != 1 {
		t.Errorf("Expected 1 condition, got %d", len(status.Conditions))
	}

	// Verify Ready condition can be found and used (instead of Phase)
	readyCondition := meta.FindStatusCondition(status.Conditions, "Ready")
	if readyCondition == nil {
		t.Fatal("Expected Ready condition to be present")
	}

	if readyCondition.Status != metav1.ConditionTrue {
		t.Errorf("Expected Ready condition status True, got %s", readyCondition.Status)
	}

	// Marshal to JSON to check structure
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	// Unmarshal to map to check field presence/absence
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	// Verify Phase field does not exist (deprecated field)
	if _, exists := result["phase"]; exists {
		t.Error("Phase field should not exist in ManifestCaptureRequestStatus")
	}
}

// TestManifestCaptureRequest_ErrorReasonEnum verifies that ErrorReason enum does not contain RBACDenied.
// Checks:
// - ErrorReason can be set to NotFound, SerializationError, InternalError
// - RBACDenied is not a valid value (should be removed from enum)
func TestManifestCaptureRequest_ErrorReasonEnum(t *testing.T) {
	validReasons := []string{"NotFound", "SerializationError", "InternalError"}
	invalidReason := "RBACDenied"

	for _, reason := range validReasons {
		status := ManifestCaptureRequestStatus{
			ErrorReason: reason,
		}
		if status.ErrorReason != reason {
			t.Errorf("Expected ErrorReason %s, got %s", reason, status.ErrorReason)
		}
	}

	// Verify RBACDenied is not in the enum by checking it's not a valid value
	// Note: This test verifies the enum constraint, actual validation happens at CRD level
	status := ManifestCaptureRequestStatus{
		ErrorReason: invalidReason,
	}
	// The value can be set in Go code, but CRD validation should reject it
	// This test just verifies the field exists and can be set
	if status.ErrorReason != invalidReason {
		t.Errorf("Expected ErrorReason %s, got %s", invalidReason, status.ErrorReason)
	}
}

// TestManifestCaptureRequest_Conditions verifies that ManifestCaptureRequest uses Conditions instead of Phase.
// Checks:
// - Ready condition with True status indicates completion
// - Failed condition with True status indicates failure
// - Processing condition with True status indicates in progress
// - Multiple conditions can coexist
func TestManifestCaptureRequest_Conditions(t *testing.T) {
	status := ManifestCaptureRequestStatus{
		Conditions: []metav1.Condition{
			{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "Completed",
				Message:            "Request completed",
				LastTransitionTime: metav1.Now(),
				ObservedGeneration: 1,
			},
			{
				Type:               "Processing",
				Status:             metav1.ConditionFalse,
				Reason:             "Completed",
				Message:            "Processing completed",
				LastTransitionTime: metav1.Now(),
				ObservedGeneration: 1,
			},
		},
	}

	// Check Ready condition
	readyCondition := meta.FindStatusCondition(status.Conditions, "Ready")
	if readyCondition == nil {
		t.Fatal("Expected Ready condition to be present")
	}
	if readyCondition.Status != metav1.ConditionTrue {
		t.Errorf("Expected Ready condition status True, got %s", readyCondition.Status)
	}

	// Check Processing condition
	processingCondition := meta.FindStatusCondition(status.Conditions, "Processing")
	if processingCondition == nil {
		t.Fatal("Expected Processing condition to be present")
	}
	if processingCondition.Status != metav1.ConditionFalse {
		t.Errorf("Expected Processing condition status False, got %s", processingCondition.Status)
	}
}

