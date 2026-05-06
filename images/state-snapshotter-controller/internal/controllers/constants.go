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

package controllers

const (
	KindSnapshot                   = "Snapshot"
	KindDemoVirtualDiskSnapshot    = "DemoVirtualDiskSnapshot"
	KindDemoVirtualMachineSnapshot = "DemoVirtualMachineSnapshot"
	KindDemoVirtualDisk            = "DemoVirtualDisk"
	KindDemoVirtualMachine         = "DemoVirtualMachine"
)

// API constants for ObjectKeeper
const (
	// DeckhouseAPIVersion is the API version for deckhouse.io resources (ObjectKeeper)
	// Note: This is group/version, not just group, despite the name.
	DeckhouseAPIVersion                 = "deckhouse.io/v1alpha1"
	KindObjectKeeper                    = "ObjectKeeper"
	ObjectKeeperModeFollowObject        = "FollowObject"
	ObjectKeeperModeFollowObjectWithTTL = "FollowObjectWithTTL"
)

// Annotation key constants
const (
	AnnotationKeyTTL = "state-snapshotter.deckhouse.io/ttl" // TTL annotation for automatic deletion
)
