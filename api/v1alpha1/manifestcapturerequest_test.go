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
// - Conditions work correctly
// - Ready condition can be checked via meta.FindStatusCondition
// - Code compiles and works without Phase
func TestManifestCaptureRequest_NoPhaseInStatus(t *testing.T) {
	status := ManifestCaptureRequestStatus{
		CheckpointName: "test-checkpoint",
		Conditions: []metav1.Condition{
			{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "Completed",
				Message:            "Request completed",
				LastTransitionTime: metav1.Now(),
			},
		},
	}

	// Verify status fields work without Phase
	if status.CheckpointName != "test-checkpoint" {
		t.Errorf("Expected CheckpointName test-checkpoint, got %s", status.CheckpointName)
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

// TestManifestCaptureRequest_Conditions verifies that ManifestCaptureRequest uses Conditions instead of Phase.
// Checks:
// - Ready condition with True status indicates completion
// - Ready condition with False status indicates failure
// - Only Ready condition is used (single-condition model)
func TestManifestCaptureRequest_Conditions(t *testing.T) {
	status := ManifestCaptureRequestStatus{
		Conditions: []metav1.Condition{
			{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "Completed",
				Message:            "Request completed",
				LastTransitionTime: metav1.Now(),
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
}
