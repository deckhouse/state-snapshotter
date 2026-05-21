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
	"testing"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

func TestValidateDataRefsForPublish_ok(t *testing.T) {
	t.Parallel()
	expected := []vcpkg.Target{{UID: "a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "p", Namespace: "ns"}}
	refs := []vcpkg.DataBinding{{
		TargetUID: "a",
		Target:    vcpkg.Target{UID: "a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "p", Namespace: "ns"},
		Artifact:  vcpkg.ArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc"},
	}}
	if err := ValidateDataRefsForPublish(expected, refs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateDataRefsForPublish_missingDataRef(t *testing.T) {
	t.Parallel()
	expected := []vcpkg.Target{{UID: "a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "p", Namespace: "ns"}}
	if err := ValidateDataRefsForPublish(expected, nil); err == nil {
		t.Fatal("expected error for missing dataRef")
	}
}

func TestValidateDataRefsForPublish_wrongTargetName(t *testing.T) {
	t.Parallel()
	expected := []vcpkg.Target{{UID: "uid-a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: "ns"}}
	refs := []vcpkg.DataBinding{{
		TargetUID: "uid-a",
		Target:    vcpkg.Target{UID: "uid-a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-wrong", Namespace: "ns"},
		Artifact:  vcpkg.ArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc"},
	}}
	if err := ValidateDataRefsForPublish(expected, refs); err == nil {
		t.Fatal("expected error for mismatched target.name")
	}
}

func TestValidateDataRefsForPublish_targetUIDNotEqualTargetUID(t *testing.T) {
	t.Parallel()
	expected := []vcpkg.Target{{UID: "uid-a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: "ns"}}
	refs := []vcpkg.DataBinding{{
		TargetUID: "uid-a",
		Target:    vcpkg.Target{UID: "uid-other", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: "ns"},
		Artifact:  vcpkg.ArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc"},
	}}
	if err := ValidateDataRefsForPublish(expected, refs); err == nil {
		t.Fatal("expected error when targetUID != target.uid")
	}
}

func TestContentDataRefsCoverExpectedTargets_rejectsWrongUID(t *testing.T) {
	t.Parallel()
	expected := []vcpkg.Target{{UID: "uid-a"}, {UID: "uid-b"}}
	published := []storagev1alpha1.SnapshotDataBinding{
		{TargetUID: "uid-a", Artifact: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-a"}},
		{TargetUID: "uid-wrong", Artifact: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-b"}},
	}
	if ContentDataRefsCoverExpectedTargets(published, expected) {
		t.Fatal("stale/wrong targetUID must not satisfy coverage")
	}
}
