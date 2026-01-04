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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types according to ADR
const (
	// ConditionInProgress indicates the object is in progress (creation only)
	ConditionInProgress = "InProgress"

	// ConditionReady indicates the object is ready for use
	ConditionReady = "Ready"

	// ConditionManifestsReady indicates manifests are ready (MCR Ready=True)
	ConditionManifestsReady = "ManifestsReady"

	// ConditionDataReady indicates data is ready (VCR Ready=True, if applicable)
	ConditionDataReady = "DataReady"

	// ConditionChildrenSnapshotsReady indicates all child snapshots are ready
	ConditionChildrenSnapshotsReady = "ChildrenSnapshotsReady"

	// ConditionHandledByDomainSpecificController indicates domain controller has started processing
	ConditionHandledByDomainSpecificController = "HandledByDomainSpecificController"

	// ConditionHandledByCommonController indicates common controller has started processing
	ConditionHandledByCommonController = "HandledByCommonController"
)

// Reasons for Ready=False
const (
	ReasonContentMissing        = "ContentMissing"
	ReasonChildSnapshotMissing  = "ChildSnapshotMissing"
	ReasonArtifactMissing       = "ArtifactMissing"
	ReasonDeleting              = "Deleting"
)

// Reasons for Ready=True
const (
	ReasonReady = "Ready"
)

// SetCondition sets a condition on a SnapshotLike or SnapshotContentLike object
// This is the ONLY way to write conditions - all controllers must use this function
// This ensures consistency and prevents races
func SetCondition(obj interface{}, conditionType string, status metav1.ConditionStatus, reason, message string) {
	var conditions []metav1.Condition
	var setter func([]metav1.Condition)

	switch v := obj.(type) {
	case SnapshotLike:
		conditions = v.GetStatusConditions()
		setter = v.SetStatusConditions
	case SnapshotContentLike:
		conditions = v.GetStatusConditions()
		setter = v.SetStatusConditions
	default:
		return
	}

	cond := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}

	// Remove existing condition of the same type
	meta.RemoveStatusCondition(&conditions, conditionType)
	// Add new condition
	meta.SetStatusCondition(&conditions, cond)

	setter(conditions)
}

// HasCondition checks if an object has a condition with the given type and status
func HasCondition(obj interface{}, conditionType string, status metav1.ConditionStatus) bool {
	var conditions []metav1.Condition

	switch v := obj.(type) {
	case SnapshotLike:
		conditions = v.GetStatusConditions()
	case SnapshotContentLike:
		conditions = v.GetStatusConditions()
	default:
		return false
	}

	cond := meta.FindStatusCondition(conditions, conditionType)
	return cond != nil && cond.Status == status
}

// GetCondition returns the condition with the given type, or nil if not found
func GetCondition(obj interface{}, conditionType string) *metav1.Condition {
	var conditions []metav1.Condition

	switch v := obj.(type) {
	case SnapshotLike:
		conditions = v.GetStatusConditions()
	case SnapshotContentLike:
		conditions = v.GetStatusConditions()
	default:
		return nil
	}

	return meta.FindStatusCondition(conditions, conditionType)
}

// IsReady returns true if the object has Ready=True condition
func IsReady(obj interface{}) bool {
	return HasCondition(obj, ConditionReady, metav1.ConditionTrue)
}

// IsInProgress returns true if the object has InProgress=True condition
func IsInProgress(obj interface{}) bool {
	return HasCondition(obj, ConditionInProgress, metav1.ConditionTrue)
}

// IsTerminal returns true if the object is in a terminal state (Ready=True or Ready=False)
func IsTerminal(obj interface{}) bool {
	readyCond := GetCondition(obj, ConditionReady)
	return readyCond != nil && (readyCond.Status == metav1.ConditionTrue || readyCond.Status == metav1.ConditionFalse)
}

