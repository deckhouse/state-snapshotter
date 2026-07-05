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

package volumecapture

import (
	"fmt"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

const kindVolumeSnapshotContent = "VolumeSnapshotContent"

// ValidateDataRefsForPublish checks foundation VCR.status.dataRefs[] before handoff/publish (PR-4).
func ValidateDataRefsForPublish(expected []vcpkg.Target, refs []vcpkg.DataBinding) error {
	if len(expected) == 0 {
		return fmt.Errorf("expected targets must not be empty")
	}
	if len(refs) != len(expected) {
		return fmt.Errorf("dataRefs count %d != targets count %d", len(refs), len(expected))
	}
	byUID := make(map[string]vcpkg.Target, len(expected))
	for _, t := range expected {
		if t.UID == "" {
			return fmt.Errorf("expected target has empty uid")
		}
		byUID[t.UID] = t
	}
	seen := make(map[string]struct{}, len(refs))
	for i, ref := range refs {
		if ref.TargetUID == "" {
			return fmt.Errorf("dataRefs[%d]: missing targetUID", i)
		}
		if _, dup := seen[ref.TargetUID]; dup {
			return fmt.Errorf("dataRefs: duplicate targetUID %q", ref.TargetUID)
		}
		seen[ref.TargetUID] = struct{}{}
		exp, ok := byUID[ref.TargetUID]
		if !ok {
			return fmt.Errorf("dataRefs: unexpected targetUID %q", ref.TargetUID)
		}
		if err := validateDataRefTargetIdentity(i, ref, exp); err != nil {
			return err
		}
		if ref.Artifact.APIVersion == "" || ref.Artifact.Kind == "" || ref.Artifact.Name == "" {
			return fmt.Errorf("dataRefs[%d]: artifact apiVersion/kind/name required (targetUID=%s)", i, ref.TargetUID)
		}
		if ref.Artifact.Kind != kindVolumeSnapshotContent {
			return fmt.Errorf("dataRefs[%d]: artifact kind %q must be %s (targetUID=%s)", i, ref.Artifact.Kind, kindVolumeSnapshotContent, ref.TargetUID)
		}
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("dataRefs: missing bindings for %d expected target(s)", len(expected)-len(seen))
	}
	return nil
}

// ContentDataRefsCoverExpectedTargets reports whether published content bindings match all expected PVC UIDs.
func ContentDataRefsCoverExpectedTargets(published []storagev1alpha1.SnapshotDataBinding, expected []vcpkg.Target) bool {
	if len(expected) == 0 {
		return len(published) == 0
	}
	if len(published) != len(expected) {
		return false
	}
	want := make(map[string]struct{}, len(expected))
	for _, t := range expected {
		want[t.UID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(published))
	for _, b := range published {
		uid := string(b.Source.UID)
		if uid == "" {
			return false
		}
		if _, ok := want[uid]; !ok {
			return false
		}
		if _, dup := seen[uid]; dup {
			return false
		}
		seen[uid] = struct{}{}
		if b.Artifact.APIVersion == "" || b.Artifact.Kind != kindVolumeSnapshotContent || b.Artifact.Name == "" {
			return false
		}
	}
	return len(seen) == len(expected)
}

func validateDataRefTargetIdentity(index int, ref vcpkg.DataBinding, expected vcpkg.Target) error {
	if ref.Target.UID == "" || ref.Target.APIVersion == "" || ref.Target.Kind == "" ||
		ref.Target.Name == "" || ref.Target.Namespace == "" {
		return fmt.Errorf("dataRefs[%d]: target object must set uid, apiVersion, kind, name, namespace (targetUID=%s)", index, ref.TargetUID)
	}
	if ref.TargetUID != ref.Target.UID {
		return fmt.Errorf("dataRefs[%d]: targetUID %q != target.uid %q", index, ref.TargetUID, ref.Target.UID)
	}
	if !volumeCaptureTargetsEqual(expected, ref.Target) {
		return fmt.Errorf("dataRefs[%d]: target object does not match expected PVC for targetUID %q", index, ref.TargetUID)
	}
	return nil
}

func volumeCaptureTargetsEqual(a, b vcpkg.Target) bool {
	return a.UID == b.UID &&
		a.APIVersion == b.APIVersion &&
		a.Kind == b.Kind &&
		a.Name == b.Name &&
		a.Namespace == b.Namespace
}
