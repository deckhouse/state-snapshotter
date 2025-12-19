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

// Condition type constants
// Only Ready condition is used - it is set to True on success or False on final failure
const (
	ConditionTypeReady = "Ready"
)

// Condition reason constants
//
// NOTE:
// Ready=False is not synonymous with failure in this controller.
// reason distinguishes between:
//   - Processing   (operation in progress, non-terminal)
//   - Failed       (operation could not be completed, terminal)
//
// Without explicit reasons Ready=False would be ambiguous and misleading.
const (
	// ConditionReasonProcessing indicates that the controller has accepted the request
	// and the operation is currently in progress.
	// This is the ONLY non-terminal Ready=False state.
	ConditionReasonProcessing = "Processing"

	// ConditionReasonCompleted indicates successful completion of the operation.
	// This is the ONLY allowed reason for Ready=True.
	ConditionReasonCompleted = "Completed"

	// ConditionReasonFailed indicates that the operation could not be completed.
	// Covers all terminal failure cases regardless of root cause.
	ConditionReasonFailed = "Failed"
)
