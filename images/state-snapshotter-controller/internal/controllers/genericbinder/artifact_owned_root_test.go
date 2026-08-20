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

package genericbinder

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// volumeSnapshotFixture builds a CSI VolumeSnapshot as the binder sees it: optionally provenance-stamped,
// optionally a child (controller-owned by a parent Snapshot), optionally bound to a VSC.
func volumeSnapshotFixture(provenance bool, parentSnapshot string, boundVSC string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: snapshot.CSISnapshotGroup, Version: snapshot.CSISnapshotVersion, Kind: snapshot.KindVolumeSnapshot,
	})
	obj.SetNamespace("ns1")
	obj.SetName("user-snap")
	if provenance {
		storagev1alpha1.StampProvenance(obj)
	}
	if parentSnapshot != "" {
		ctrlTrue := true
		obj.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
			Kind:       "Snapshot",
			Name:       parentSnapshot,
			UID:        types.UID(parentSnapshot + "-uid"),
			Controller: &ctrlTrue,
		}})
	}
	if boundVSC != "" {
		_ = unstructured.SetNestedField(obj.Object, boundVSC, "status", "boundVolumeSnapshotContentName")
	}
	return obj
}

// The root content of a snapshot this module did not create is owned by that snapshot's own CSI artifact,
// not by an ObjectKeeper. The predicate that picks that shape asks three separate questions, and each one
// alone is enough to fall back to the ordinary tree shape.
func TestIsArtifactOwnedRoot(t *testing.T) {
	otherKindRoot := &unstructured.Unstructured{Object: map[string]interface{}{}}
	otherKindRoot.SetGroupVersionKind(domainSnapshotGVK)
	otherKindRoot.SetName("widget")

	tests := []struct {
		name string
		obj  *unstructured.Unstructured
		want bool
	}{
		{
			name: "user's own root VolumeSnapshot: the artifact owns the content",
			obj:  volumeSnapshotFixture(false, "", ""),
			want: true,
		},
		{
			name: "root VolumeSnapshot this module created: ordinary keeper-owned root",
			obj:  volumeSnapshotFixture(true, "", ""),
			want: false,
		},
		{
			name: "child VolumeSnapshot: owned by its parent's content, provenance or not",
			obj:  volumeSnapshotFixture(false, "ns-snapshot", ""),
			want: false,
		},
		{
			name: "root of another kind: only CSI snapshots have an artifact that exists on its own",
			obj:  otherKindRoot,
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isArtifactOwnedRoot(tc.obj); got != tc.want {
				t.Fatalf("isArtifactOwnedRoot = %v, want %v", got, tc.want)
			}
		})
	}
}

// Resolving the owner waits for CSI to bind the snapshot, then points at the bound VolumeSnapshotContent by
// UID. Nothing is invented while the binding is not observable: an unbound snapshot and a published-but-
// absent VSC both report pending, because a content created now would have no owner and would be GC'd.
func TestResolveBoundArtifactOwnerRef(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	vsc := &unstructured.Unstructured{Object: map[string]interface{}{}}
	vsc.SetGroupVersionKind(schema.GroupVersionKind{
		Group: snapshot.CSISnapshotGroup, Version: snapshot.CSISnapshotVersion, Kind: snapshot.KindVolumeSnapshotContent,
	})
	vsc.SetName("snapcontent-1")
	vsc.SetUID(types.UID("snapcontent-1-uid"))

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vsc).Build()
	r := &GenericSnapshotBinderController{Client: cl}

	t.Run("bound snapshot resolves to its VolumeSnapshotContent", func(t *testing.T) {
		ref, pending, err := r.resolveBoundArtifactOwnerRef(ctx, volumeSnapshotFixture(false, "", "snapcontent-1"))
		if err != nil || pending {
			t.Fatalf("resolve = (pending %v, err %v), want a resolved owner", pending, err)
		}
		if ref.Kind != snapshot.KindVolumeSnapshotContent || ref.Name != "snapcontent-1" ||
			ref.UID != types.UID("snapcontent-1-uid") || ref.Controller == nil || !*ref.Controller {
			t.Fatalf("ownerReference = %+v, want controller ref to snapcontent-1 by UID", ref)
		}
	})

	t.Run("unbound snapshot is pending", func(t *testing.T) {
		ref, pending, err := r.resolveBoundArtifactOwnerRef(ctx, volumeSnapshotFixture(false, "", ""))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if !pending || ref != nil {
			t.Fatalf("resolve = (ref %+v, pending %v), want pending with no owner", ref, pending)
		}
	})

	t.Run("bound to a VolumeSnapshotContent that is not observable is pending", func(t *testing.T) {
		ref, pending, err := r.resolveBoundArtifactOwnerRef(ctx, volumeSnapshotFixture(false, "", "snapcontent-gone"))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if !pending || ref != nil {
			t.Fatalf("resolve = (ref %+v, pending %v), want pending rather than an invented owner", ref, pending)
		}
	})
}
