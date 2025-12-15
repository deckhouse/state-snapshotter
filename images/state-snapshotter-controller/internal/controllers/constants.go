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

// Condition type constants
const (
	ConditionTypeReady      = "Ready"
	ConditionTypeFailed     = "Failed"
	ConditionTypeProcessing = "Processing"
	ConditionTypeActive     = "Active"
)

// Condition reason constants
const (
	ConditionReasonCompleted          = "Completed"
	ConditionReasonInProgress         = "InProgress"
	ConditionReasonInternalError      = "InternalError"
	ConditionReasonNotFound           = "NotFound"
	ConditionReasonSerializationError = "SerializationError"
	ConditionReasonInvalidTTL         = "InvalidTTL" // For invalid TTL annotation format
)

// Retainer-specific condition reasons (for ConditionTypeActive)
const (
	// RetainerConditionReasonObjectExists indicates the FollowObject exists and matches UID
	RetainerConditionReasonObjectExists = "ObjectExists"
	// RetainerConditionReasonTTLActive indicates TTL is active and not expired
	RetainerConditionReasonTTLActive = "TTLActive"
	// RetainerConditionReasonTTLExpired indicates TTL has expired
	RetainerConditionReasonTTLExpired = "TTLExpired"
	// RetainerConditionReasonObjectNotFound indicates FollowObject was not found
	RetainerConditionReasonObjectNotFound = "ObjectNotFound"
	// RetainerConditionReasonUIDMismatch indicates FollowObject UID mismatch (object recreated)
	RetainerConditionReasonUIDMismatch = "UIDMismatch"
	// RetainerConditionReasonNamespaceTerminating indicates namespace is terminating
	RetainerConditionReasonNamespaceTerminating = "NamespaceTerminating"
	// RetainerConditionReasonMissingFollowObjectRef indicates FollowObjectRef is missing
	RetainerConditionReasonMissingFollowObjectRef = "MissingFollowObjectRef"
	// RetainerConditionReasonMissingTTL indicates TTL is missing
	RetainerConditionReasonMissingTTL = "MissingTTL"
	// RetainerConditionReasonInvalidAPIVersion indicates invalid APIVersion
	RetainerConditionReasonInvalidAPIVersion = "InvalidAPIVersion"
	// RetainerConditionReasonInvalidTTL indicates invalid TTL duration
	RetainerConditionReasonInvalidTTL = "InvalidTTL"
	// RetainerConditionReasonInvalidMode indicates unknown mode
	RetainerConditionReasonInvalidMode = "InvalidMode"
)

// API constants for ObjectKeeper
const (
	// DeckhouseAPIVersion is the API version for deckhouse.io resources (ObjectKeeper)
	// Note: This is group/version, not just group, despite the name.
	DeckhouseAPIVersion = "deckhouse.io/v1alpha1"
	KindObjectKeeper    = "ObjectKeeper"
)

// Annotation key constants
const (
	AnnotationKeyTTL = "state-snapshotter.deckhouse.io/ttl" // TTL annotation for automatic deletion
)
