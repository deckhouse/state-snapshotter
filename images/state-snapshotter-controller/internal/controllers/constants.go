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

import (
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/csd"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snaphelpers"
)

const (
	KindSnapshot = controllercommon.KindSnapshot
)

// API constants for ObjectKeeper
const (
	// DeckhouseAPIVersion is the API version for deckhouse.io resources (ObjectKeeper)
	// Note: This is group/version, not just group, despite the name.
	DeckhouseAPIVersion                 = controllercommon.DeckhouseAPIVersion
	KindObjectKeeper                    = controllercommon.KindObjectKeeper
	ObjectKeeperModeFollowObject        = controllercommon.ObjectKeeperModeFollowObject
	ObjectKeeperModeFollowObjectWithTTL = controllercommon.ObjectKeeperModeFollowObjectWithTTL
)

const (
	CSDConditionAccepted      = csd.CSDConditionAccepted
	CSDConditionAccessGranted = csd.CSDConditionAccessGranted
	CSDConditionReady         = csd.CSDConditionReady
)

const (
	CSDReasonKindConflict  = csd.CSDReasonKindConflict
	CSDReasonInvalidSpec   = csd.CSDReasonInvalidSpec
	CSDReadyReasonNotReady = csd.CSDReadyReasonNotReady
)
