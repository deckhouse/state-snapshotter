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

package snaphelpers

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// NewSnapshotContentSpec builds the immutable SnapshotContent spec shared by every content-creation
// site (capture root, import root, generic/domain import leaf, extended VolumeSnapshot import, orphan
// child volume node). snapshotRef is the REQUIRED back-reference to the snapshot subject that binds
// this content via its status.boundSnapshotContentName; it is the anti-spoofing handshake (mirrors CSI
// VolumeSnapshot<->VolumeSnapshotContent) and must be set at creation because spec is immutable.
func NewSnapshotContentSpec(deletionPolicy string, snapshotRef *storagev1alpha1.SnapshotSubjectRef) storagev1alpha1.SnapshotContentSpec {
	return storagev1alpha1.SnapshotContentSpec{
		DeletionPolicy: deletionPolicy,
		SnapshotRef:    snapshotRef,
	}
}

// SnapshotSubjectRefFromSnapshot builds the back-reference for a core namespaced Snapshot.
func SnapshotSubjectRefFromSnapshot(s *storagev1alpha1.Snapshot) *storagev1alpha1.SnapshotSubjectRef {
	return &storagev1alpha1.SnapshotSubjectRef{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       KindSnapshot,
		Namespace:  s.Namespace,
		Name:       s.Name,
		UID:        s.UID,
	}
}

// SnapshotSubjectRefFromObject builds the back-reference for any snapshot subject addressed as an
// unstructured object: a generic/domain XXXSnapshot or a CSI VolumeSnapshot (orphan volume nodes).
func SnapshotSubjectRefFromObject(obj *unstructured.Unstructured) *storagev1alpha1.SnapshotSubjectRef {
	gvk := obj.GroupVersionKind()
	return &storagev1alpha1.SnapshotSubjectRef{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
		UID:        obj.GetUID(),
	}
}
