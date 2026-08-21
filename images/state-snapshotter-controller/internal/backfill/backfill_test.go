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

package backfill

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// obj builds an unstructured object of the given GVK with a name and optional labels.
func obj(gvk schema.GroupVersionKind, name string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	if labels != nil {
		u.SetLabels(labels)
	}
	return u
}

// newClient builds a fake client that can List/Patch the given unstructured objects. It registers each
// distinct GVK (and its List) so the controller-runtime fake client resolves unstructured List calls.
func newClient(objs ...*unstructured.Unstructured) client.Client {
	scheme := runtime.NewScheme()
	seen := map[schema.GroupVersionKind]bool{}
	b := fake.NewClientBuilder()
	for _, o := range objs {
		gvk := o.GroupVersionKind()
		if !seen[gvk] {
			seen[gvk] = true
			scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
			listGVK := gvk
			listGVK.Kind += "List"
			scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
		}
		b = b.WithObjects(o)
	}
	return b.WithScheme(scheme).Build()
}

// snapGVK is the unified Snapshot kind this module owns; csiGVK is the shared CSI kind. These two are
// what DefaultTargets protects, so the fixtures below need no other kind.
func snapGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: groupStorage, Version: "v1alpha1", Kind: "Snapshot"}
}

func csiGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: groupCSI, Version: "v1", Kind: "VolumeSnapshot"}
}

func TestApplyStampsChildSnapshotButNotRoot(t *testing.T) {
	ctx := context.Background()
	root := obj(snapGVK(), "my-app-backup", nil)           // user-named root → NOT ours
	child := obj(snapGVK(), "nss-snap-aabbccdd-0011", nil) // deterministic child → ours

	cl := newClient(root, child)
	targets := []Target{{GVK: snapGVK(), IsOurs: hasNamePrefix("nss-snap-")}}

	rep, err := Apply(ctx, cl, targets)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.PatchedTotal() != 1 {
		t.Fatalf("expected 1 patched, got %d", rep.PatchedTotal())
	}

	gotRoot := &unstructured.Unstructured{}
	gotRoot.SetGroupVersionKind(snapGVK())
	if err := cl.Get(ctx, client.ObjectKey{Name: "my-app-backup"}, gotRoot); err != nil {
		t.Fatalf("get root: %v", err)
	}
	if storagev1alpha1.IsDeleteProtected(gotRoot) {
		t.Fatalf("root Snapshot must NOT be marked")
	}

	gotChild := &unstructured.Unstructured{}
	gotChild.SetGroupVersionKind(snapGVK())
	if err := cl.Get(ctx, client.ObjectKey{Name: "nss-snap-aabbccdd-0011"}, gotChild); err != nil {
		t.Fatalf("get child: %v", err)
	}
	if !storagev1alpha1.IsDeleteProtected(gotChild) {
		t.Fatalf("child Snapshot must be marked")
	}
}

func TestApplyIsIdempotentAndGateBecomesMet(t *testing.T) {
	ctx := context.Background()
	child := obj(snapGVK(), "nss-snap-a-b", nil)
	ourVS := obj(csiGVK(), "nss-vs-a-b", map[string]string{
		storagev1alpha1.LabelProvenance: storagev1alpha1.LabelProvenanceUnifiedSnapshot,
	})

	cl := newClient(child, ourVS)
	targets := []Target{
		{GVK: snapGVK(), IsOurs: hasNamePrefix("nss-snap-")},
		{GVK: csiGVK(), IsOurs: hasProvenance},
	}

	// Gate is NOT met before backfill.
	pre, err := Verify(ctx, cl, targets)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if pre.OursUnmarkedTotal() != 2 {
		t.Fatalf("expected 2 unmarked before backfill, got %d", pre.OursUnmarkedTotal())
	}

	first, err := Apply(ctx, cl, targets)
	if err != nil {
		t.Fatalf("apply#1: %v", err)
	}
	if first.PatchedTotal() != 2 {
		t.Fatalf("expected 2 patched on first apply, got %d", first.PatchedTotal())
	}

	// Idempotent: a second apply patches nothing.
	second, err := Apply(ctx, cl, targets)
	if err != nil {
		t.Fatalf("apply#2: %v", err)
	}
	if second.PatchedTotal() != 0 {
		t.Fatalf("expected 0 patched on second apply (idempotent), got %d", second.PatchedTotal())
	}

	// Gate is now met.
	post, err := Verify(ctx, cl, targets)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if post.OursUnmarkedTotal() != 0 {
		t.Fatalf("expected gate met (0 unmarked), got %d", post.OursUnmarkedTotal())
	}
}

func TestApplyToleratesUninstalledKind(t *testing.T) {
	ctx := context.Background()
	// Client serves only Snapshot; the CSI VolumeSnapshot kind is not installed on this cluster. Apply must
	// not fail — a protected Kind with no objects to migrate is a no-op, not an error.
	child := obj(snapGVK(), "nss-snap-a-b", nil)
	cl := newClient(child)
	targets := []Target{
		{GVK: snapGVK(), IsOurs: hasNamePrefix("nss-snap-")},
		{GVK: csiGVK(), IsOurs: hasProvenance},
	}

	rep, err := Apply(ctx, cl, targets)
	if err != nil {
		t.Fatalf("apply must tolerate an uninstalled kind, got: %v", err)
	}
	// The present kind is still migrated.
	if rep.PatchedTotal() != 1 {
		t.Fatalf("expected the served child Snapshot to be patched, got %d", rep.PatchedTotal())
	}
	// The uninstalled kind contributes nothing to the gate.
	for _, k := range rep.Kinds {
		if k.GVK.Kind == "VolumeSnapshot" && (k.Total != 0 || k.OursUnmarked != 0 || k.Patched != 0) {
			t.Fatalf("uninstalled VolumeSnapshot must contribute nothing, got %+v", k)
		}
	}
}

// VolumeSnapshot is the one shared kind left to migrate, and the label that separates the two sides of the
// share is provenance. Being MANAGED is not that label: a snapshot a user took by hand is captured like any
// other and carries managed=true, while nothing about it belongs to this module — stamping protection onto
// it is precisely the defect this classifier must not reintroduce, because its owner could then no longer
// delete their own snapshot.
func TestClassifierFailsClosedForSharedKinds(t *testing.T) {
	const labelManaged = storagev1alpha1.APIGroup + "/managed"

	ourVS := obj(csiGVK(), "some-vs", map[string]string{
		storagev1alpha1.LabelProvenance: storagev1alpha1.LabelProvenanceUnifiedSnapshot,
	})
	legacyOrphanVS := obj(csiGVK(), "nss-vs-a-b", nil)
	capturedUserVS := obj(csiGVK(), "user-vs", map[string]string{labelManaged: "true"})
	vetoedUserVS := obj(csiGVK(), "user-vs2", map[string]string{labelManaged: "false"})
	plainUserVS := obj(csiGVK(), "user-vs3", nil)

	vc := anyOf(hasProvenance, hasNamePrefix("nss-vs-"))
	for _, tc := range []struct {
		name string
		obj  *unstructured.Unstructured
		want bool
	}{
		{"a snapshot this module created says so itself", ourVS, true},
		{"a node made before the origin label existed still carries its deterministic name", legacyOrphanVS, true},
		{"a user's own snapshot is captured, which is not the same as ours", capturedUserVS, false},
		{"a snapshot another controller owns is vetoed and stays untouched", vetoedUserVS, false},
		{"a plain user snapshot carries no signal at all", plainUserVS, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := vc(tc.obj); got != tc.want {
				t.Fatalf("classifier = %v, want %v", got, tc.want)
			}
		})
	}
}
