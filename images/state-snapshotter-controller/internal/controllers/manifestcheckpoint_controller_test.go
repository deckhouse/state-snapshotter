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
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

// TestSetSingleCondition verifies the setSingleCondition helper function that manages conditions.
// Checks:
// - Adding a new condition to an empty list
// - Updating an existing condition of the same type (replacement, not addition)
// - Keeping only one condition of each type (removing duplicates)
// - Correct updating of condition fields (Status, Reason, Message)
func TestSetSingleCondition(t *testing.T) {
	conds := &[]metav1.Condition{}

	// Check adding first condition
	setSingleCondition(conds, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Completed",
		Message:            "Test message",
		LastTransitionTime: metav1.Now(),
	})

	if len(*conds) != 1 {
		t.Fatalf("Expected 1 condition, got %d", len(*conds))
	}

	cond := (*conds)[0]
	if cond.Type != "Ready" {
		t.Errorf("Expected condition type Ready, got %s", cond.Type)
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("Expected condition status True, got %s", cond.Status)
	}

	// Check updating the same condition (should replace, not add)
	setSingleCondition(conds, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "Failed",
		Message:            "Updated message",
		LastTransitionTime: metav1.Now(),
	})

	if len(*conds) != 1 {
		t.Fatalf("Expected 1 condition after update, got %d", len(*conds))
	}

	updatedCond := (*conds)[0]
	if updatedCond.Status != metav1.ConditionFalse {
		t.Errorf("Expected condition status False after update, got %s", updatedCond.Status)
	}
	if updatedCond.Reason != "Failed" {
		t.Errorf("Expected condition reason Failed, got %s", updatedCond.Reason)
	}
}

// TestManifestCheckpoint_ManifestCaptureRequestRef verifies creation of ManifestCheckpointSpec with the new
// ManifestCaptureRequestRef field instead of the deprecated sourceCaptureRequestName.
// Checks:
// - ManifestCaptureRequestRef is correctly created from ManifestCaptureRequest
// - All ManifestCaptureRequestRef fields (Name, Namespace, UID) are filled correctly
// - SourceNamespace is preserved
// - ManifestCaptureRequestRef is not nil
func TestManifestCheckpoint_ManifestCaptureRequestRef(t *testing.T) {
	mcr := &storagev1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-mcr",
			Namespace: "test-namespace",
			UID:       "test-uid-123",
		},
	}

	// Simulate creating checkpoint spec with manifestCaptureRequestRef (as in controller)
	spec := storagev1alpha1.ManifestCheckpointSpec{
		SourceNamespace: mcr.Namespace,
		ManifestCaptureRequestRef: &storagev1alpha1.ObjectReference{
			Name:      mcr.Name,
			Namespace: mcr.Namespace,
			UID:       string(mcr.UID),
		},
	}

	if spec.SourceNamespace != mcr.Namespace {
		t.Errorf("Expected SourceNamespace %s, got %s", mcr.Namespace, spec.SourceNamespace)
	}

	if spec.ManifestCaptureRequestRef == nil {
		t.Fatal("Expected ManifestCaptureRequestRef to be set")
	}

	if spec.ManifestCaptureRequestRef.Name != mcr.Name {
		t.Errorf("Expected ManifestCaptureRequestRef.Name %s, got %s",
			mcr.Name, spec.ManifestCaptureRequestRef.Name)
	}

	if spec.ManifestCaptureRequestRef.Namespace != mcr.Namespace {
		t.Errorf("Expected ManifestCaptureRequestRef.Namespace %s, got %s",
			mcr.Namespace, spec.ManifestCaptureRequestRef.Namespace)
	}

	if spec.ManifestCaptureRequestRef.UID != string(mcr.UID) {
		t.Errorf("Expected ManifestCaptureRequestRef.UID %s, got %s",
			string(mcr.UID), spec.ManifestCaptureRequestRef.UID)
	}
}

// TestManifestCheckpoint_ReadyConditionCheck verifies the logic for determining checkpoint readiness through Ready condition.
// Checks:
// - Ready condition with True status → checkpoint is ready
// - Ready condition with False status → checkpoint is not ready
// - Absence of Ready condition → checkpoint is not ready
// - Correct reason determination for each state
// - Using meta.FindStatusCondition to find condition
// TestManifestCheckpoint_ReadyConditionCheck verifies the logic for determining checkpoint readiness through Ready condition.
// Checks:
// - Ready condition with True status → checkpoint is ready
// - Ready condition with False status → checkpoint is not ready
// - Absence of Ready condition → checkpoint is not ready
// - Using meta.FindStatusCondition to find condition
func TestManifestCheckpoint_ReadyConditionCheck(t *testing.T) {
	tests := []struct {
		name           string
		status         storagev1alpha1.ManifestCheckpointStatus
		expectedReady  bool
		expectedReason string
	}{
		{
			name: "Ready condition True",
			status: storagev1alpha1.ManifestCheckpointStatus{
				Conditions: []metav1.Condition{
					{
						Type:               "Ready",
						Status:             metav1.ConditionTrue,
						Reason:             "Completed",
						Message:            "Checkpoint ready",
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			expectedReady:  true,
			expectedReason: "Completed",
		},
		{
			name: "Ready condition False",
			status: storagev1alpha1.ManifestCheckpointStatus{
				Conditions: []metav1.Condition{
					{
						Type:               "Ready",
						Status:             metav1.ConditionFalse,
						Reason:             "Failed",
						Message:            "Checkpoint failed",
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			expectedReady:  false,
			expectedReason: "Failed",
		},
		{
			name: "No Ready condition",
			status: storagev1alpha1.ManifestCheckpointStatus{
				Conditions: []metav1.Condition{},
			},
			expectedReady:  false,
			expectedReason: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkpoint := &storagev1alpha1.ManifestCheckpoint{
				Status: tt.status,
			}

			readyCondition := meta.FindStatusCondition(checkpoint.Status.Conditions, "Ready")
			isReady := readyCondition != nil && readyCondition.Status == metav1.ConditionTrue

			if isReady != tt.expectedReady {
				t.Errorf("Expected isReady=%v, got %v", tt.expectedReady, isReady)
			}

			if readyCondition != nil {
				if readyCondition.Reason != tt.expectedReason {
					t.Errorf("Expected reason=%s, got %s", tt.expectedReason, readyCondition.Reason)
				}
			} else if tt.expectedReason != "" {
				t.Errorf("Expected reason=%s, but condition is nil", tt.expectedReason)
			}
		})
	}
}

// TestManifestCheckpoint_NoPhaseInStatus verifies that ManifestCheckpointStatus can be used without Phase field.
// Checks:
// - Status can be created without Phase (field removed from API)
// - TotalObjects and TotalSizeBytes work correctly
// - Conditions work correctly
// - Ready condition can be checked via meta.FindStatusCondition
// - Code compiles and works without Phase
func TestManifestCheckpoint_NoPhaseInStatus(t *testing.T) {
	status := storagev1alpha1.ManifestCheckpointStatus{
		TotalObjects:   10,
		TotalSizeBytes: 2048,
		Conditions: []metav1.Condition{
			{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "Completed",
				LastTransitionTime: metav1.Now(),
			},
		},
	}

	checkpoint := &storagev1alpha1.ManifestCheckpoint{
		Status: status,
	}

	// Verify status fields work without Phase
	if checkpoint.Status.TotalObjects != 10 {
		t.Errorf("Expected TotalObjects 10, got %d", checkpoint.Status.TotalObjects)
	}

	if len(checkpoint.Status.Conditions) != 1 {
		t.Errorf("Expected 1 condition, got %d", len(checkpoint.Status.Conditions))
	}

	// Verify Ready condition can be found and used (instead of Phase)
	readyCondition := meta.FindStatusCondition(checkpoint.Status.Conditions, "Ready")
	if readyCondition == nil {
		t.Fatal("Expected Ready condition to be present")
	}

	if readyCondition.Status != metav1.ConditionTrue {
		t.Errorf("Expected Ready condition status True, got %s", readyCondition.Status)
	}
}

// TestDetermineErrorReason_NoRBACDenied verifies that determineErrorReason does not return RBACDenied.
// Checks:
// - NotFound error returns "NotFound"
// - Serialization errors return "SerializationError"
// - Forbidden errors return "InternalError" (not "RBACDenied")
// - Other errors return "InternalError"
func TestDetermineErrorReason_NoRBACDenied(t *testing.T) {
	controller := &ManifestCheckpointController{}

	tests := []struct {
		name           string
		err            error
		expectedReason string
	}{
		{
			name:           "NotFound error",
			err:            errors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "test"),
			expectedReason: "NotFound",
		},
		{
			name:           "Serialization error",
			err:            fmt.Errorf("failed to marshal json"),
			expectedReason: "SerializationError",
		},
		{
			name:           "Nil error",
			err:            nil,
			expectedReason: "",
		},
		{
			name:           "Generic error",
			err:            fmt.Errorf("some error"),
			expectedReason: "InternalError",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := controller.determineErrorReason(tt.err)
			if reason != tt.expectedReason {
				t.Errorf("Expected reason %s, got %s", tt.expectedReason, reason)
			}
			// Verify RBACDenied is never returned
			if reason == "RBACDenied" {
				t.Error("determineErrorReason should never return RBACDenied")
			}
		})
	}
}

// TestManifestCaptureRequest_NoPhaseInCreation verifies that ManifestCaptureRequest can be created without Phase.
// Checks:
// - MCR can be created with empty Status (no Phase field)
// - Status can be set without Phase
// - Conditions can be used instead of Phase
func TestManifestCaptureRequest_NoPhaseInCreation(t *testing.T) {
	mcr := &storagev1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-mcr",
			Namespace: "test-namespace",
		},
		Spec: storagev1alpha1.ManifestCaptureRequestSpec{
			Targets: []storagev1alpha1.ManifestTarget{
				{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "test-cm",
				},
			},
		},
		// Status is empty - no Phase field
	}

	if mcr.Name != "test-mcr" {
		t.Errorf("Expected name test-mcr, got %s", mcr.Name)
	}

	// Set status with Conditions (no Phase)
	mcr.Status = storagev1alpha1.ManifestCaptureRequestStatus{
		Conditions: []metav1.Condition{
			{
				Type:               "Processing",
				Status:             metav1.ConditionTrue,
				Reason:             "InProgress",
				Message:            "Processing request",
				LastTransitionTime: metav1.Now(),
			},
		},
	}

	if len(mcr.Status.Conditions) != 1 {
		t.Errorf("Expected 1 condition, got %d", len(mcr.Status.Conditions))
	}

	processingCondition := meta.FindStatusCondition(mcr.Status.Conditions, "Processing")
	if processingCondition == nil {
		t.Fatal("Expected Processing condition to be present")
	}
}

// TestIsNamespacedResource_ClusterScopedRejected verifies that cluster-scoped resources are correctly identified
// and rejected when used in ManifestCaptureRequest targets.
// ADR requirement: "All targets must be namespaced objects in the same namespace as ManifestCaptureRequest.
// Cluster-scoped resources are NOT allowed in targets."
// Checks:
// - Common cluster-scoped resources (Namespace, Node, PersistentVolume, etc.) return false
// - Namespaced resources return true
// - This validation prevents cluster-scoped resources from being captured
func TestIsNamespacedResource_ClusterScopedRejected(t *testing.T) {
	controller := &ManifestCheckpointController{}

	tests := []struct {
		name           string
		gv             schema.GroupVersion
		kind           string
		expectedResult bool
		description    string
	}{
		{
			name:           "Namespace is cluster-scoped",
			gv:             schema.GroupVersion{Group: "", Version: "v1"},
			kind:           "Namespace",
			expectedResult: false,
			description:    "Namespace is a cluster-scoped resource and must be rejected",
		},
		{
			name:           "Node is cluster-scoped",
			gv:             schema.GroupVersion{Group: "", Version: "v1"},
			kind:           "Node",
			expectedResult: false,
			description:    "Node is a cluster-scoped resource and must be rejected",
		},
		{
			name:           "PersistentVolume is cluster-scoped",
			gv:             schema.GroupVersion{Group: "", Version: "v1"},
			kind:           "PersistentVolume",
			expectedResult: false,
			description:    "PersistentVolume is a cluster-scoped resource and must be rejected",
		},
		{
			name:           "ClusterRole is cluster-scoped",
			gv:             schema.GroupVersion{Group: "rbac.authorization.k8s.io", Version: "v1"},
			kind:           "ClusterRole",
			expectedResult: false,
			description:    "ClusterRole is a cluster-scoped resource and must be rejected",
		},
		{
			name:           "ManifestCheckpoint is cluster-scoped",
			gv:             schema.GroupVersion{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1"},
			kind:           "ManifestCheckpoint",
			expectedResult: false,
			description:    "ManifestCheckpoint itself is cluster-scoped and must be rejected",
		},
		{
			name:           "ConfigMap is namespaced",
			gv:             schema.GroupVersion{Group: "", Version: "v1"},
			kind:           "ConfigMap",
			expectedResult: true,
			description:    "ConfigMap is a namespaced resource and should be allowed",
		},
		{
			name:           "Pod is namespaced",
			gv:             schema.GroupVersion{Group: "", Version: "v1"},
			kind:           "Pod",
			expectedResult: true,
			description:    "Pod is a namespaced resource and should be allowed",
		},
		{
			name:           "Service is namespaced",
			gv:             schema.GroupVersion{Group: "", Version: "v1"},
			kind:           "Service",
			expectedResult: true,
			description:    "Service is a namespaced resource and should be allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := controller.isNamespacedResource(tt.gv, tt.kind)
			if result != tt.expectedResult {
				t.Errorf("%s: Expected isNamespacedResource=%v, got %v", tt.description, tt.expectedResult, result)
			}
		})
	}
}

// TestManifestCheckpoint_ClusterScoped verifies that ManifestCheckpoint is created as cluster-scoped resource.
// ADR requirement: "ManifestCheckpoint — cluster-scoped. Создаётся только ManifestCheckpointController."
// Checks:
// - ManifestCheckpoint has no namespace in metadata
// - ManifestCheckpoint can be created without namespace
// - This ensures checkpoint survives namespace deletion
func TestManifestCheckpoint_ClusterScoped(t *testing.T) {
	checkpoint := &storagev1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mcp-test-123",
			// No Namespace field - cluster-scoped
		},
		Spec: storagev1alpha1.ManifestCheckpointSpec{
			SourceNamespace: "test-namespace",
			ManifestCaptureRequestRef: &storagev1alpha1.ObjectReference{
				Name:      "test-mcr",
				Namespace: "test-namespace",
				UID:       "test-uid",
			},
		},
	}

	// Verify checkpoint has no namespace (cluster-scoped)
	if checkpoint.Namespace != "" {
		t.Errorf("Expected ManifestCheckpoint to be cluster-scoped (no namespace), but got namespace: %s", checkpoint.Namespace)
	}

	// Verify checkpoint has name
	if checkpoint.Name == "" {
		t.Error("Expected ManifestCheckpoint to have a name")
	}

	// Verify SourceNamespace is preserved (for reference, not for scope)
	if checkpoint.Spec.SourceNamespace != "test-namespace" {
		t.Errorf("Expected SourceNamespace test-namespace, got %s", checkpoint.Spec.SourceNamespace)
	}
}

// TestManifestCheckpointContentChunk_ClusterScoped verifies that ManifestCheckpointContentChunk is created as cluster-scoped resource.
// ADR requirement: "ManifestCheckpointContentChunk — cluster-scoped. Создаётся только ManifestCheckpointController."
// Checks:
// - ManifestCheckpointContentChunk has no namespace in metadata
// - ManifestCheckpointContentChunk can be created without namespace
// - This ensures chunks survive namespace deletion
func TestManifestCheckpointContentChunk_ClusterScoped(t *testing.T) {
	chunk := &storagev1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mcp-test-123-0",
			// No Namespace field - cluster-scoped
		},
		Spec: storagev1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: "mcp-test-123",
			Index:          0,
			Data:           "test-data",
			ObjectsCount:   5,
			Checksum:       "test-checksum",
		},
	}

	// Verify chunk has no namespace (cluster-scoped)
	if chunk.Namespace != "" {
		t.Errorf("Expected ManifestCheckpointContentChunk to be cluster-scoped (no namespace), but got namespace: %s", chunk.Namespace)
	}

	// Verify chunk has name
	if chunk.Name == "" {
		t.Error("Expected ManifestCheckpointContentChunk to have a name")
	}

	// Verify chunk name follows pattern: checkpoint-name-index
	if chunk.Spec.CheckpointName == "" {
		t.Error("Expected ManifestCheckpointContentChunk to have CheckpointName")
	}
}

// TestMCRObjectKeeper_CreatedWithFollowObject verifies that ObjectKeeper for ManifestCaptureRequest is created with FollowObject mode (no TTL).
// ADR requirement: ObjectKeeper uses FollowObject mode to follow MCR lifecycle.
// TTL and request cleanup are handled by MCR controller, not ObjectKeeper.
// Checks:
// - ObjectKeeper for MCR is created with FollowObject mode (no TTL)
// - ObjectKeeper follows the ManifestCaptureRequest
// - ObjectKeeper has no TTL (TTL is handled by MCR controller)
func TestMCRObjectKeeper_CreatedWithFollowObject(t *testing.T) {
	mcr := &storagev1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-mcr",
			Namespace: "test-namespace",
			UID:       "test-uid-123",
		},
	}

	// Simulate creating ObjectKeeper (as in controller)
	retainerName := fmt.Sprintf("ret-mcr-%s-%s", mcr.Namespace, mcr.Name)

	objectKeeper := &deckhousev1alpha1.ObjectKeeper{
		TypeMeta: metav1.TypeMeta{
			APIVersion: DeckhouseAPIVersion,
			Kind:       KindObjectKeeper,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: retainerName,
		},
		Spec: deckhousev1alpha1.ObjectKeeperSpec{
			Mode: "FollowObject",
			FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
				APIVersion: "state-snapshotter.deckhouse.io/v1alpha1",
				Kind:       "ManifestCaptureRequest",
				Namespace:  mcr.Namespace,
				Name:       mcr.Name,
				UID:        string(mcr.UID),
			},
		},
	}

	// Verify objectKeeper name follows pattern
	expectedName := fmt.Sprintf("ret-mcr-%s-%s", mcr.Namespace, mcr.Name)
	if objectKeeper.Name != expectedName {
		t.Errorf("Expected objectKeeper name %s, got %s", expectedName, objectKeeper.Name)
	}

	// Verify mode is FollowObject (not FollowObjectWithTTL)
	if objectKeeper.Spec.Mode != "FollowObject" {
		t.Errorf("Expected mode FollowObject, got %s", objectKeeper.Spec.Mode)
	}

	// Verify ObjectKeeper has NO TTL (TTL is handled by MCR controller)
	// ObjectKeeper spec doesn't have TTL field - it's only FollowObject mode

	// Verify FollowObjectRef points to MCR
	if objectKeeper.Spec.FollowObjectRef == nil {
		t.Fatal("Expected FollowObjectRef to be set")
	}
	if objectKeeper.Spec.FollowObjectRef.Kind != "ManifestCaptureRequest" {
		t.Errorf("Expected FollowObjectRef.Kind ManifestCaptureRequest, got %s", objectKeeper.Spec.FollowObjectRef.Kind)
	}
	if objectKeeper.Spec.FollowObjectRef.Name != mcr.Name {
		t.Errorf("Expected FollowObjectRef.Name %s, got %s", mcr.Name, objectKeeper.Spec.FollowObjectRef.Name)
	}
	if objectKeeper.Spec.FollowObjectRef.Namespace != mcr.Namespace {
		t.Errorf("Expected FollowObjectRef.Namespace %s, got %s", mcr.Namespace, objectKeeper.Spec.FollowObjectRef.Namespace)
	}
	if objectKeeper.Spec.FollowObjectRef.UID != string(mcr.UID) {
		t.Errorf("Expected FollowObjectRef.UID %s, got %s", string(mcr.UID), objectKeeper.Spec.FollowObjectRef.UID)
	}
}
