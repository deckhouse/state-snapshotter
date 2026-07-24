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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// obj builds an unstructured object of the given GVK with name, optional labels and ownerRef kinds.
func obj(gvk schema.GroupVersionKind, name string, labels map[string]string, ownerKinds ...string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	if labels != nil {
		u.SetLabels(labels)
	}
	var owners []metav1.OwnerReference
	for _, k := range ownerKinds {
		owners = append(owners, metav1.OwnerReference{APIVersion: "x/v1", Kind: k, Name: "o"})
	}
	if owners != nil {
		u.SetOwnerReferences(owners)
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

func snapGVK(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: groupStorage, Version: "v1alpha1", Kind: kind}
}

func TestApplyStampsChildSnapshotButNotRoot(t *testing.T) {
	ctx := context.Background()
	root := obj(snapGVK("Snapshot"), "my-app-backup", nil)           // user-named root → NOT ours
	child := obj(snapGVK("Snapshot"), "nss-snap-aabbccdd-0011", nil) // deterministic child → ours

	cl := newClient(root, child)
	targets := []Target{{GVK: snapGVK("Snapshot"), IsOurs: hasNamePrefix("nss-snap-")}}

	rep, err := Apply(ctx, cl, targets)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.PatchedTotal() != 1 {
		t.Fatalf("expected 1 patched, got %d", rep.PatchedTotal())
	}

	gotRoot := &unstructured.Unstructured{}
	gotRoot.SetGroupVersionKind(snapGVK("Snapshot"))
	if err := cl.Get(ctx, client.ObjectKey{Name: "my-app-backup"}, gotRoot); err != nil {
		t.Fatalf("get root: %v", err)
	}
	if storagev1alpha1.IsDeleteProtected(gotRoot) {
		t.Fatalf("root Snapshot must NOT be marked")
	}

	gotChild := &unstructured.Unstructured{}
	gotChild.SetGroupVersionKind(snapGVK("Snapshot"))
	if err := cl.Get(ctx, client.ObjectKey{Name: "nss-snap-aabbccdd-0011"}, gotChild); err != nil {
		t.Fatalf("get child: %v", err)
	}
	if !storagev1alpha1.IsDeleteProtected(gotChild) {
		t.Fatalf("child Snapshot must be marked")
	}
}

func TestApplyIsIdempotentAndGateBecomesMet(t *testing.T) {
	ctx := context.Background()
	child := obj(snapGVK("Snapshot"), "nss-snap-a-b", nil)
	content := obj(snapGVK("SnapshotContent"), "nss-content-x", nil)

	cl := newClient(child, content)
	targets := []Target{
		{GVK: snapGVK("Snapshot"), IsOurs: hasNamePrefix("nss-snap-")},
		{GVK: snapGVK("SnapshotContent"), IsOurs: always},
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
	// Client serves only Snapshot; the SnapshotContent kind is not installed on this cluster. Apply must
	// not fail — a protected Kind with no objects to migrate is a no-op, not an error.
	child := obj(snapGVK("Snapshot"), "nss-snap-a-b", nil)
	cl := newClient(child)
	targets := []Target{
		{GVK: snapGVK("Snapshot"), IsOurs: hasNamePrefix("nss-snap-")},
		{GVK: snapGVK("SnapshotContent"), IsOurs: always},
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
		if k.GVK.Kind == "SnapshotContent" && (k.Total != 0 || k.OursUnmarked != 0 || k.Patched != 0) {
			t.Fatalf("uninstalled SnapshotContent must contribute nothing, got %+v", k)
		}
	}
}

func TestClassifierFailsClosedForSharedKinds(t *testing.T) {
	// ObjectKeeper: only our deterministic names are ours; a foreign keeper is left alone.
	okOurs := obj(schema.GroupVersionKind{Group: groupDeckhouse, Version: "v1alpha1", Kind: kindObjectKeeper}, "nss-ok-123", nil)
	okForeign := obj(schema.GroupVersionKind{Group: groupDeckhouse, Version: "v1alpha1", Kind: kindObjectKeeper}, "some-other-keeper", nil)

	c := hasNamePrefix("nss-ok-", "nss-import-ok-")
	if !c(okOurs) {
		t.Fatalf("our ObjectKeeper must classify as ours")
	}
	if c(okForeign) {
		t.Fatalf("foreign ObjectKeeper must NOT classify as ours (fail closed)")
	}

	// VolumeSnapshotContent: ours iff owned by an ObjectKeeper (fresh managed VSC) OR a SnapshotContent
	// (durable VSC after the ownership handoff re-parents it). Both must classify as ours; anything else foreign.
	vscOursKeeper := obj(schema.GroupVersionKind{Group: groupCSI, Version: "v1", Kind: "VolumeSnapshotContent"}, "snapshot-x", nil, kindObjectKeeper)
	vscOursContent := obj(schema.GroupVersionKind{Group: groupCSI, Version: "v1", Kind: "VolumeSnapshotContent"}, "snapshot-y", nil, kindSnapshotContent)
	vscForeign := obj(schema.GroupVersionKind{Group: groupCSI, Version: "v1", Kind: "VolumeSnapshotContent"}, "snapcontent-user", nil, "VolumeSnapshot")
	oc := anyOf(ownedByKind(kindObjectKeeper), ownedByKind(kindSnapshotContent))
	if !oc(vscOursKeeper) {
		t.Fatalf("VSC owned by ObjectKeeper must classify as ours")
	}
	if !oc(vscOursContent) {
		t.Fatalf("durable VSC owned by SnapshotContent (post-handoff) must classify as ours")
	}
	if oc(vscForeign) {
		t.Fatalf("VSC not owned by our ObjectKeeper/SnapshotContent must NOT classify as ours (fail closed)")
	}

	// VolumeSnapshot: ours iff managed=true or orphan-VS name.
	vsManaged := obj(schema.GroupVersionKind{Group: groupCSI, Version: "v1", Kind: "VolumeSnapshot"}, "user-vs", map[string]string{labelManaged: "true"})
	vsVetoed := obj(schema.GroupVersionKind{Group: groupCSI, Version: "v1", Kind: "VolumeSnapshot"}, "user-vs2", map[string]string{labelManaged: "false"})
	vsPlain := obj(schema.GroupVersionKind{Group: groupCSI, Version: "v1", Kind: "VolumeSnapshot"}, "user-vs3", nil)
	vc := anyOf(managedIsTrue, hasNamePrefix("nss-vs-"))
	if !vc(vsManaged) {
		t.Fatalf("managed=true VS must classify as ours")
	}
	if vc(vsVetoed) {
		t.Fatalf("vetoed VS must NOT classify as ours")
	}
	if vc(vsPlain) {
		t.Fatalf("plain user VS must NOT classify as ours")
	}
}
