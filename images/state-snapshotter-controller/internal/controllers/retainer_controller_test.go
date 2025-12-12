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

package controllers

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	iretainer "github.com/deckhouse/state-snapshotter/api/v1alpha1/iretainer"
)

// TestRetainer_NoPhaseInStatus verifies that RetainerStatus can be used without Phase field.
// Checks:
// - Status can be created without Phase (field removed from API)
// - Conditions work correctly
// - Active condition can be checked via meta.FindStatusCondition
// - Code compiles and works without Phase
func TestRetainer_NoPhaseInStatus(t *testing.T) {
	status := iretainer.IRetainerStatus{
		Conditions: []metav1.Condition{
			{
				Type:               ConditionTypeActive,
				Status:             metav1.ConditionTrue,
				Reason:             "ObjectExists",
				Message:            "Following object",
				LastTransitionTime: metav1.Now(),
			},
		},
	}

	retainer := &iretainer.IRetainer{
		Status: status,
	}

	// Verify status fields work without Phase
	if len(retainer.Status.Conditions) != 1 {
		t.Errorf("Expected 1 condition, got %d", len(retainer.Status.Conditions))
	}

	// Verify Active condition can be found and used (instead of Phase)
	activeCondition := meta.FindStatusCondition(retainer.Status.Conditions, ConditionTypeActive)
	if activeCondition == nil {
		t.Fatal("Expected Active condition to be present")
	}

	if activeCondition.Status != metav1.ConditionTrue {
		t.Errorf("Expected Active condition status True, got %s", activeCondition.Status)
	}
}

// TestRetainer_ActiveConditionCheck verifies the logic for determining retainer activity through Active condition.
// Checks:
// - Active condition with True status → retainer is active
// - Active condition with False status → retainer is expired/invalid
// - Absence of Active condition → retainer is not active
// - Using meta.FindStatusCondition to find condition
func TestRetainer_ActiveConditionCheck(t *testing.T) {
	tests := []struct {
		name           string
		status         iretainer.IRetainerStatus
		expectedActive bool
		expectedReason string
	}{
		{
			name: "Active condition True",
			status: iretainer.IRetainerStatus{
				Conditions: []metav1.Condition{
					{
						Type:               ConditionTypeActive,
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
			status: iretainer.IRetainerStatus{
				Conditions: []metav1.Condition{
					{
						Type:               ConditionTypeActive,
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
			status: iretainer.IRetainerStatus{
				Conditions: []metav1.Condition{},
			},
			expectedActive: false,
			expectedReason: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			retainer := &iretainer.IRetainer{
				Status: tt.status,
			}

			activeCondition := meta.FindStatusCondition(retainer.Status.Conditions, ConditionTypeActive)
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
	retainer := &iretainer.IRetainer{
		Status: iretainer.IRetainerStatus{
			Conditions: []metav1.Condition{
				{
					Type:               ConditionTypeActive,
					Status:             metav1.ConditionTrue,
					Reason:             "ObjectExists",
					Message:            "Following object",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	// Retainer should have Active condition, not Ready
	activeCondition := meta.FindStatusCondition(retainer.Status.Conditions, ConditionTypeActive)
	if activeCondition == nil {
		t.Fatal("Expected Active condition to be present")
	}

	readyCondition := meta.FindStatusCondition(retainer.Status.Conditions, ConditionTypeReady)
	if readyCondition != nil {
		t.Error("Retainer should not have Ready condition, it uses Active condition")
	}

	// Verify Active condition semantics
	if activeCondition.Status != metav1.ConditionTrue {
		t.Errorf("Expected Active condition status True, got %s", activeCondition.Status)
	}
}

// TestConditionTypeActive_Constant verifies that ConditionTypeActive constant is correctly defined.
// Checks:
// - ConditionTypeActive constant exists
// - Constant value is "Active"
// - Constant is different from ConditionTypeReady
func TestConditionTypeActive_Constant(t *testing.T) {
	if ConditionTypeActive != "Active" {
		t.Errorf("Expected ConditionTypeActive to be 'Active', got %s", ConditionTypeActive)
	}

	if ConditionTypeActive == ConditionTypeReady {
		t.Error("ConditionTypeActive should be different from ConditionTypeReady")
	}

	if ConditionTypeActive == ConditionTypeFailed {
		t.Error("ConditionTypeActive should be different from ConditionTypeFailed")
	}
}
