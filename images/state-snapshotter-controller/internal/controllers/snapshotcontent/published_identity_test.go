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

package snapshotcontent

import (
	"testing"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func TestPublishedDataIdentityFrozen(t *testing.T) {
	if publishedDataIdentityFrozen(nil) {
		t.Fatal("nil binding is not frozen")
	}
	empty := &storagev1alpha1.SnapshotDataBinding{}
	if publishedDataIdentityFrozen(empty) {
		t.Fatal("empty names are not frozen")
	}
	frozen := &storagev1alpha1.SnapshotDataBinding{
		SourceRef:   storagev1alpha1.SnapshotSubjectRef{Name: "pvc"},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{Name: "vsc"},
	}
	if !publishedDataIdentityFrozen(frozen) {
		t.Fatal("name+name is frozen even without uid or size")
	}
}

func TestDataBindingIdentityEqualIgnoresUIDAndSize(t *testing.T) {
	a := storagev1alpha1.SnapshotDataBinding{
		SourceRef: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc", Namespace: "ns", UID: "uid-a",
		},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc", UID: "art-a",
		},
		Size: "1Gi",
	}
	b := a
	b.SourceRef.UID = "uid-b"
	b.ArtifactRef.UID = "art-b"
	b.Size = "2Gi"
	if !dataBindingIdentityEqual(a, b) {
		t.Fatal("uid and Size are not identity")
	}
	b.ArtifactRef.Name = "other"
	if dataBindingIdentityEqual(a, b) {
		t.Fatal("artifact name is identity")
	}
	b = a
	b.SourceRef.Namespace = "other"
	if dataBindingIdentityEqual(a, b) {
		t.Fatal("source namespace is identity")
	}
}
