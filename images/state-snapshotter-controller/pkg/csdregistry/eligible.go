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

// Package csdregistry derives unified snapshot GVK pairs from CustomSnapshotDefinition
// for wiring GenericSnapshotBinderController / SnapshotContentController (R2 phase 2).
package csdregistry

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

const (
	conditionAccepted  = "Accepted"
	conditionRBACReady = "RBACReady"
)

// CSDWatchEligible implements the ADR activation predicate (same inputs as runtime watch formula):
// Accepted=True, RBACReady=True, and both conditions have observedGeneration == metadata.generation.
// Ready is not read as an input.
func CSDWatchEligible(d *storagev1alpha1.CustomSnapshotDefinition) bool {
	if d == nil {
		return false
	}
	gen := d.GetGeneration()
	acc := meta.FindStatusCondition(d.Status.Conditions, conditionAccepted)
	rbac := meta.FindStatusCondition(d.Status.Conditions, conditionRBACReady)
	if acc == nil || acc.Status != metav1.ConditionTrue || acc.ObservedGeneration != gen {
		return false
	}
	if rbac == nil || rbac.Status != metav1.ConditionTrue || rbac.ObservedGeneration != gen {
		return false
	}
	return true
}
