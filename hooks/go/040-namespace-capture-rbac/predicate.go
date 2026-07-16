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

package namespace_capture_rbac

import (
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// needsCaptureRBAC reports whether a Snapshot currently requires the transient per-namespace capture
// RoleBinding (the controller SA reading the live namespace via the wildcard ClusterRole).
//
// It keys on the root manifest-leg latch (status.captureState.commonController.manifestCaptured), NOT on
// live Ready: Ready is a live-health signal that re-opens on artifact/child degradation, but degradation
// is re-validation of existing artifacts — never a fresh read of the live namespace — so it must not
// re-grant broad read rights. The latch flips once the bound SnapshotContent reports the subtree
// manifests persisted, and is monotonic (never flips back), so the grant is dropped for good.
//
//   - import snapshots never capture the live namespace -> false.
//   - manifestCaptured=true: the subtree's manifests are captured; reading the namespace is no longer
//     needed (immune to later Ready degradation or a child disappearing — the point of the latch).
//   - Ready=False with a terminal reason (IsReasonTerminal): manifests can never be captured for this
//     run; the core stops reading the namespace.
//   - otherwise (latch not set yet, Ready non-terminal or absent): capture is in progress or about to
//     start — hold/grant the rights.
func needsCaptureRBAC(snap *storagev1alpha1.Snapshot) bool {
	if snap == nil {
		return false
	}
	if snap.IsImportMode() {
		return false
	}
	if cs := snap.Status.CaptureState; cs != nil && cs.CommonController != nil &&
		cs.CommonController.ManifestCaptured != nil && *cs.CommonController.ManifestCaptured {
		return false
	}
	if cond := apimeta.FindStatusCondition(snap.Status.Conditions, storagev1alpha1.ConditionReady); cond != nil &&
		cond.Status == metav1.ConditionFalse && storagev1alpha1.IsReasonTerminal(cond.Reason) {
		return false
	}
	// Capture in progress or about to start: hold the rights (fail-open for the grant side narrows the
	// fail-closed Phase 6 window; least-privilege is restored once captured or terminally failed).
	return true
}

// namespacesNeedingCaptureRBAC returns the set of namespaces that host at least one Snapshot requiring the
// transient capture RoleBinding (level-based desired state).
func namespacesNeedingCaptureRBAC(snaps []storagev1alpha1.Snapshot) map[string]struct{} {
	desired := make(map[string]struct{})
	for i := range snaps {
		if needsCaptureRBAC(&snaps[i]) {
			desired[snaps[i].Namespace] = struct{}{}
		}
	}
	return desired
}

// captureRoleBindingLabels are stamped on every hook-managed capture RoleBinding. The dedicated
// capture-rbac marker label is what the hook lists/selects on, so it never touches Helm-managed module
// RoleBindings (leader-election, auth-reader) that share heritage/module labels.
func captureRoleBindingLabels() map[string]string {
	return map[string]string{
		"heritage":                 "deckhouse",
		"module":                   modulePluralName,
		captureRBACManagedLabelKey: captureRBACManagedLabelValue,
	}
}
