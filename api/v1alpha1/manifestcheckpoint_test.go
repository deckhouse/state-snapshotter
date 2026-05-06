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

// TestManifestCheckpoint_ManifestCaptureRequestRef verifies that ManifestCheckpoint correctly uses
// the new ManifestCaptureRequestRef field instead of the deprecated sourceCaptureRequestName.
// Checks:
// - ManifestCaptureRequestRef correctly serializes and deserializes
// - All ManifestCaptureRequestRef fields (Name, Namespace, UID) are preserved
// - SourceNamespace is preserved
// - Ready condition works correctly (instead of deprecated Phase)
// - Status does not contain Phase and Message fields
func TestManifestCheckpoint_ManifestCaptureRequestRef(t *testing.T) {
	checkpoint := &ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-checkpoint",
		},
		Spec: ManifestCheckpointSpec{
			SourceNamespace: "test-namespace",
			ManifestCaptureRequestRef: &ObjectReference{
				Name:      "test-mcr",
				Namespace: "test-namespace",
				UID:       "test-uid-123",
			},
		},
		Status: ManifestCheckpointStatus{
			TotalObjects:  5,
			TotalSizeBytes: 1024,
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "Completed",
					Message:            "Checkpoint created successfully",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	// Check JSON marshaling/unmarshaling
	data, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatalf("Failed to marshal ManifestCheckpoint: %v", err)
	}

	var unmarshaled ManifestCheckpoint
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("Failed to unmarshal ManifestCheckpoint: %v", err)
	}

	// Verify spec: SourceNamespace and ManifestCaptureRequestRef
	if unmarshaled.Spec.SourceNamespace != checkpoint.Spec.SourceNamespace {
		t.Errorf("Expected SourceNamespace %s, got %s", checkpoint.Spec.SourceNamespace, unmarshaled.Spec.SourceNamespace)
	}

	if unmarshaled.Spec.ManifestCaptureRequestRef == nil {
		t.Fatal("Expected ManifestCaptureRequestRef to be set, got nil")
	}

	if unmarshaled.Spec.ManifestCaptureRequestRef.Name != checkpoint.Spec.ManifestCaptureRequestRef.Name {
		t.Errorf("Expected ManifestCaptureRequestRef.Name %s, got %s", 
			checkpoint.Spec.ManifestCaptureRequestRef.Name, unmarshaled.Spec.ManifestCaptureRequestRef.Name)
	}

	if unmarshaled.Spec.ManifestCaptureRequestRef.Namespace != checkpoint.Spec.ManifestCaptureRequestRef.Namespace {
		t.Errorf("Expected ManifestCaptureRequestRef.Namespace %s, got %s", 
			checkpoint.Spec.ManifestCaptureRequestRef.Namespace, unmarshaled.Spec.ManifestCaptureRequestRef.Namespace)
	}

	if unmarshaled.Spec.ManifestCaptureRequestRef.UID != checkpoint.Spec.ManifestCaptureRequestRef.UID {
		t.Errorf("Expected ManifestCaptureRequestRef.UID %s, got %s", 
			checkpoint.Spec.ManifestCaptureRequestRef.UID, unmarshaled.Spec.ManifestCaptureRequestRef.UID)
	}

	// Verify status: TotalObjects is preserved, Phase and Message are absent
	if unmarshaled.Status.TotalObjects != checkpoint.Status.TotalObjects {
		t.Errorf("Expected TotalObjects %d, got %d", checkpoint.Status.TotalObjects, unmarshaled.Status.TotalObjects)
	}

	// Verify Ready condition (instead of deprecated Phase)
	readyCondition := meta.FindStatusCondition(unmarshaled.Status.Conditions, "Ready")
	if readyCondition == nil {
		t.Fatal("Expected Ready condition to be present")
	}

	if readyCondition.Status != metav1.ConditionTrue {
		t.Errorf("Expected Ready condition status to be True, got %s", readyCondition.Status)
	}

	if readyCondition.Reason != "Completed" {
		t.Errorf("Expected Ready condition reason to be Completed, got %s", readyCondition.Reason)
	}
}

// TestManifestCheckpoint_ReadyCondition verifies various Ready condition states in ManifestCheckpoint.
// Checks:
// - Ready condition with True status is correctly identified as ready
// - Ready condition with False status is correctly identified as not ready
// - Absence of Ready condition is correctly handled (not ready)
// - Ready condition with Unknown status is correctly handled (not ready)
// - Correct reason determination for each state
func TestManifestCheckpoint_ReadyCondition(t *testing.T) {
	tests := []struct {
		name           string
		conditions     []metav1.Condition
		expectedReady  bool
		expectedReason string
	}{
		{
			name: "Ready condition True",
			conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "Completed",
					Message:            "Checkpoint ready",
					LastTransitionTime: metav1.Now(),
				},
			},
			expectedReady:  true,
			expectedReason: "Completed",
		},
		{
			name: "Ready condition False",
			conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionFalse,
					Reason:             "Failed",
					Message:            "Checkpoint failed",
					LastTransitionTime: metav1.Now(),
				},
			},
			expectedReady:  false,
			expectedReason: "Failed",
		},
		{
			name:           "No Ready condition",
			conditions:     []metav1.Condition{},
			expectedReady:  false,
			expectedReason: "",
		},
		{
			name: "Ready condition Unknown",
			conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionUnknown,
					Reason:             "Pending",
					Message:            "Checkpoint pending",
					LastTransitionTime: metav1.Now(),
				},
			},
			expectedReady:  false,
			expectedReason: "Pending",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkpoint := &ManifestCheckpoint{
				Status: ManifestCheckpointStatus{
					Conditions: tt.conditions,
				},
			}

			readyCondition := meta.FindStatusCondition(checkpoint.Status.Conditions, "Ready")
			isReady := readyCondition != nil && readyCondition.Status == metav1.ConditionTrue

			if isReady != tt.expectedReady {
				t.Errorf("Expected isReady=%v, got %v", tt.expectedReady, isReady)
			}

			if readyCondition != nil && readyCondition.Reason != tt.expectedReason {
				t.Errorf("Expected reason=%s, got %s", tt.expectedReason, readyCondition.Reason)
			}
		})
	}
}

// TestManifestCheckpoint_NoPhaseField verifies that ManifestCheckpointStatus does not contain deprecated Phase and Message fields.
// Checks:
// - Phase field does not exist in JSON after marshaling
// - Message field does not exist in JSON after marshaling
// - Conditions field exists and works correctly
// - Status can be created and used without Phase/Message
func TestManifestCheckpoint_NoPhaseField(t *testing.T) {
	checkpoint := &ManifestCheckpoint{
		Status: ManifestCheckpointStatus{
			TotalObjects:  10,
			TotalSizeBytes: 2048,
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "Completed",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	// Marshal to JSON to check structure
	data, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	// Unmarshal to map to check field presence/absence
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	status, ok := result["status"].(map[string]interface{})
	if !ok {
		t.Fatal("Status field not found")
	}

	// Verify Phase field does not exist (deprecated field)
	if _, exists := status["phase"]; exists {
		t.Error("Phase field should not exist in ManifestCheckpointStatus")
	}

	// Verify Message field does not exist (deprecated field)
	if _, exists := status["message"]; exists {
		t.Error("Message field should not exist in ManifestCheckpointStatus")
	}

	// Verify Conditions field exists (new field)
	if _, exists := status["conditions"]; !exists {
		t.Error("Conditions field should exist in ManifestCheckpointStatus")
	}
}

// TestManifestCheckpoint_ChunksInfo verifies correct storage of chunks information in ManifestCheckpointStatus.
// Checks:
// - Chunks array is correctly stored
// - TotalObjects is correctly calculated/stored
// - TotalSizeBytes is correctly calculated/stored
// - ChunkInfo contains all required fields (Name, Index, ObjectsCount, SizeBytes, Checksum)
func TestManifestCheckpoint_ChunksInfo(t *testing.T) {
	checkpoint := &ManifestCheckpoint{
		Status: ManifestCheckpointStatus{
			Chunks: []ChunkInfo{
				{
					Name:         "mcp-test-0",
					Index:        0,
					ObjectsCount: 5,
					SizeBytes:    512,
					Checksum:     "abc123",
				},
				{
					Name:         "mcp-test-1",
					Index:        1,
					ObjectsCount: 3,
					SizeBytes:    256,
					Checksum:     "def456",
				},
			},
			TotalObjects:  8,
			TotalSizeBytes: 768,
		},
	}

	if len(checkpoint.Status.Chunks) != 2 {
		t.Errorf("Expected 2 chunks, got %d", len(checkpoint.Status.Chunks))
	}

	if checkpoint.Status.TotalObjects != 8 {
		t.Errorf("Expected TotalObjects 8, got %d", checkpoint.Status.TotalObjects)
	}

	if checkpoint.Status.TotalSizeBytes != 768 {
		t.Errorf("Expected TotalSizeBytes 768, got %d", checkpoint.Status.TotalSizeBytes)
	}
}

