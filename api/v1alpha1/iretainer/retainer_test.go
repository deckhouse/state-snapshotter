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

package iretainer

import (
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestRetainer_NoPhaseInStatus verifies that RetainerStatus does not contain Phase field.
// Checks:
// - Status can be created without Phase (field removed from API)
// - Conditions work correctly
// - Active condition can be checked via meta.FindStatusCondition
// - Code compiles and works without Phase
func TestRetainer_NoPhaseInStatus(t *testing.T) {
	status := IRetainerStatus{
		Conditions: []metav1.Condition{
			{
				Type:               "Active",
				Status:             metav1.ConditionTrue,
				Reason:             "ObjectExists",
				Message:            "Following object",
				LastTransitionTime: metav1.Now(),
			},
		},
	}

	// Verify status fields work without Phase
	if len(status.Conditions) != 1 {
		t.Errorf("Expected 1 condition, got %d", len(status.Conditions))
	}

	// Verify Active condition can be found and used (instead of Phase)
	activeCondition := meta.FindStatusCondition(status.Conditions, "Active")
	if activeCondition == nil {
		t.Fatal("Expected Active condition to be present")
	}

	if activeCondition.Status != metav1.ConditionTrue {
		t.Errorf("Expected Active condition status True, got %s", activeCondition.Status)
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
		t.Error("Phase field should not exist in RetainerStatus")
	}
}

// TestRetainer_ActiveCondition verifies that Retainer uses Active condition instead of Phase.
// Checks:
// - Active condition with True status indicates retainer is active
// - Active condition with False status indicates retainer is expired/invalid
// - Active condition semantics differ from Ready condition (different business logic)
func TestRetainer_ActiveCondition(t *testing.T) {
	tests := []struct {
		name           string
		status         IRetainerStatus
		expectedActive bool
		expectedReason string
	}{
		{
			name: "Active condition True",
			status: IRetainerStatus{
				Conditions: []metav1.Condition{
					{
						Type:               "Active",
						Status:             metav1.ConditionTrue,
						Reason:             "ObjectExists",
						Message:            "Following object",
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			expectedActive: true,
			expectedReason: "ObjectExists",
		},
		{
			name: "Active condition False",
			status: IRetainerStatus{
				Conditions: []metav1.Condition{
					{
						Type:               "Active",
						Status:             metav1.ConditionFalse,
						Reason:             "Expired",
						Message:            "TTL expired",
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			expectedActive: false,
			expectedReason: "Expired",
		},
		{
			name: "No Active condition",
			status: IRetainerStatus{
				Conditions: []metav1.Condition{},
			},
			expectedActive: false,
			expectedReason: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			retainer := &IRetainer{
				Status: tt.status,
			}

			activeCondition := meta.FindStatusCondition(retainer.Status.Conditions, "Active")
			isActive := activeCondition != nil && activeCondition.Status == metav1.ConditionTrue

			if isActive != tt.expectedActive {
				t.Errorf("Expected isActive=%v, got %v", tt.expectedActive, isActive)
			}

			if activeCondition != nil {
				if activeCondition.Reason != tt.expectedReason {
					t.Errorf("Expected reason=%s, got %s", tt.expectedReason, activeCondition.Reason)
				}
			} else if tt.expectedReason != "" {
				t.Errorf("Expected reason=%s, but condition is nil", tt.expectedReason)
			}
		})
	}
}

// TestRetainer_ActiveVsReady verifies that Retainer uses Active condition, not Ready.
// This test ensures semantic separation: Retainer is "active/expired", not "ready/not ready".
func TestRetainer_ActiveVsReady(t *testing.T) {
	retainer := &IRetainer{
		Status: IRetainerStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "Active",
					Status:             metav1.ConditionTrue,
					Reason:             "ObjectExists",
					Message:            "Following object",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	// Retainer should have Active condition, not Ready
	activeCondition := meta.FindStatusCondition(retainer.Status.Conditions, "Active")
	if activeCondition == nil {
		t.Fatal("Expected Active condition to be present")
	}

	readyCondition := meta.FindStatusCondition(retainer.Status.Conditions, "Ready")
	if readyCondition != nil {
		t.Error("Retainer should not have Ready condition, it uses Active condition")
	}

	// Verify Active condition semantics
	if activeCondition.Status != metav1.ConditionTrue {
		t.Errorf("Expected Active condition status True, got %s", activeCondition.Status)
	}
}

