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
//   - InvalidSpec  (user error, retry is useless)
//   - Failed       (execution error after valid start)
//
// Without explicit reasons Ready=False would be ambiguous and misleading.
const (
	// ConditionReasonProcessing indicates operation is in progress
	// This is the ONLY non-terminal reason for Ready=False
	ConditionReasonProcessing = "Processing"

	// ConditionReasonCompleted indicates successful completion
	// This is the ONLY allowed reason for Ready=True
	ConditionReasonCompleted = "Completed"

	// ConditionReasonFailed indicates operation failed
	// Used for execution-time failures where the request itself was valid,
	// but the controller could not complete the operation (API errors, conflicts,
	// infrastructure issues, etc.). These failures are terminal.
	ConditionReasonFailed = "Failed"

	// ConditionReasonInvalidSpec indicates invalid resource specification
	// Used for runtime semantic validation that cannot be expressed via CRD schema
	// validation (e.g. empty targets, unsupported object kinds, invalid combinations
	// of fields). Such requests are valid YAML but meaningless to execute.
	// Retry is useless; the user must fix the spec.
	ConditionReasonInvalidSpec = "InvalidSpec"

	// ConditionReasonInternalError indicates internal error (legacy, for backward compatibility)
	ConditionReasonInternalError = "InternalError"
)
