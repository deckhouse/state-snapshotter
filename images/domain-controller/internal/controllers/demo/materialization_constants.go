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

package demo

import "time"

const (
	demoPhasePending = "Pending"
	demoPhaseReady   = "Ready"
	demoPhaseFailed  = "Failed"

	demoReasonRestorePending     = "RestorePending"
	demoReasonSnapshotNotReady   = "SnapshotNotReady"
	demoReasonContentNotReady    = "ContentNotReady"
	demoReasonRestoreDenied      = "RestoreDenied"
	demoReasonInvalidDataSource  = "InvalidDataSource"
	demoReasonInvalidRestoreSpec = "InvalidRestoreSpec"
	demoReasonPVCNotReady        = "PVCNotReady"
	demoReasonDiskNotReady       = "DiskNotReady"
	demoReasonMissingPVCName      = "MissingPVCName"
	demoReasonMissingSize         = "MissingSize"
	demoReasonMissingStorageClass = "MissingStorageClass"

	demoLabelManagedBy = "demo.state-snapshotter.deckhouse.io/managed-by"
	demoLabelDiskName  = "demo.state-snapshotter.deckhouse.io/disk"
	demoLabelVMName    = "demo.state-snapshotter.deckhouse.io/vm"

	demoManagedByValue = "domain-controller"

	vrrAPIVersion = "storage.deckhouse.io/v1alpha1"
	vrrKind       = "VolumeRestoreRequest"

	vscKind = "VolumeSnapshotContent"

	objectKeeperKind       = "ObjectKeeper"
	objectKeeperAPIVersion = "deckhouse.io/v1alpha1"

	defaultDemoResourceRequeueAfter = 500 * time.Millisecond
)
