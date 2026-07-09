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

package snapshot

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func TestFindParentsReferencingChildSnapshot_MatchesGVKAndName(t *testing.T) {
	ctx := context.Background()
	gvk := schema.GroupVersionKind{Group: "demo.test", Version: "v1", Kind: "DemoSnap"}
	child := &unstructured.Unstructured{}
	child.SetGroupVersionKind(gvk)
	child.SetNamespace("ns-a")
	child.SetName("snap-1")

	parent := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-a", Name: "parent"},
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{
				{
					APIVersion: "demo.test/v1",
					Kind:       "DemoSnap",
					Name:       "snap-1",
				},
			},
		},
	}
	sc := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(sc).WithObjects(parent).
		WithIndex(&storagev1alpha1.Snapshot{}, SnapshotChildrenRefFieldIndex, snapshotChildrenRefIndexValues).Build()
	reqs := findParentsReferencingChildSnapshot(ctx, cl, child)
	if len(reqs) != 1 || reqs[0].Namespace != "ns-a" || reqs[0].Name != "parent" {
		t.Fatalf("got %+v", reqs)
	}
}

// Namespace-local run tree: a Snapshot in another namespace must not be returned because parent lookup is
// scoped to the child namespace only. This is the safety proof for the deliberately namespace-LESS index
// key (GVK+name, no namespace): two parents in different namespaces reference the SAME child name+GVK and
// therefore index under the SAME key, so isolation must come from the InNamespace(childNS) List filter, not
// from the key. Asserted in BOTH directions so neither namespace can leak into the other.
func TestFindParentsReferencingChildSnapshot_OnlySameNamespaceAsChild(t *testing.T) {
	ctx := context.Background()
	gvk := schema.GroupVersionKind{Group: "demo.test", Version: "v1", Kind: "DemoSnap"}
	newChild := func(ns string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		u.SetNamespace(ns)
		u.SetName("snap-1")
		return u
	}

	parentSame := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-a", Name: "root"},
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{
				{APIVersion: "demo.test/v1", Kind: "DemoSnap", Name: "snap-1"},
			},
		},
	}
	parentOther := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-b", Name: "other-root"},
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{
				{APIVersion: "demo.test/v1", Kind: "DemoSnap", Name: "snap-1"},
			},
		},
	}
	sc := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(sc).WithObjects(parentSame, parentOther).
		WithIndex(&storagev1alpha1.Snapshot{}, SnapshotChildrenRefFieldIndex, snapshotChildrenRefIndexValues).Build()

	if reqs := findParentsReferencingChildSnapshot(ctx, cl, newChild("ns-a")); len(reqs) != 1 || reqs[0].Namespace != "ns-a" || reqs[0].Name != "root" {
		t.Fatalf("child ns-a/snap-1 must wake only ns-a/root, got %+v", reqs)
	}
	if reqs := findParentsReferencingChildSnapshot(ctx, cl, newChild("ns-b")); len(reqs) != 1 || reqs[0].Namespace != "ns-b" || reqs[0].Name != "other-root" {
		t.Fatalf("child ns-b/snap-1 must wake only ns-b/other-root, got %+v", reqs)
	}
}

// A deleted child snapshot is gone from the API, so the relay can no longer read its identity. It must
// reconstruct the child from the watch GVK + request key and still wake every parent that references
// it, otherwise child-Snapshot deletion would not be propagated event-driven (INV-FAIL-PROP).
func TestBuildSyntheticDeletedChild_MatchesReferencingParent(t *testing.T) {
	ctx := context.Background()
	gvk := schema.GroupVersionKind{Group: "demo.test", Version: "v1", Kind: "DemoSnap"}

	parent := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-a", Name: "parent"},
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{
				{APIVersion: "demo.test/v1", Kind: "DemoSnap", Name: "snap-1"},
			},
		},
	}
	sc := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(sc).WithObjects(parent).
		WithIndex(&storagev1alpha1.Snapshot{}, SnapshotChildrenRefFieldIndex, snapshotChildrenRefIndexValues).Build()

	// Synthetic child built only from GVK + namespace/name (the object itself no longer exists).
	synthetic := buildSyntheticDeletedChild(gvk, "ns-a", "snap-1")
	if synthetic.GetAPIVersion() != "demo.test/v1" || synthetic.GetKind() != "DemoSnap" {
		t.Fatalf("synthetic child GVK mismatch: %s/%s", synthetic.GetAPIVersion(), synthetic.GetKind())
	}
	reqs := findParentsReferencingChildSnapshot(ctx, cl, synthetic)
	if len(reqs) != 1 || reqs[0].Namespace != "ns-a" || reqs[0].Name != "parent" {
		t.Fatalf("deleted child should wake referencing parent, got %+v", reqs)
	}
}

func TestFindParentsReferencingChildSnapshot_SameNameDifferentGVKNoFalsePositive(t *testing.T) {
	ctx := context.Background()
	gvkChild := schema.GroupVersionKind{Group: "demo.test", Version: "v1", Kind: "KindB"}
	child := &unstructured.Unstructured{}
	child.SetGroupVersionKind(gvkChild)
	child.SetNamespace("ns1")
	child.SetName("x")

	parent := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "parent"},
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{
				{
					APIVersion: "demo.test/v1",
					Kind:       "KindA",
					Name:       "x",
				},
			},
		},
	}
	sc := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(sc).WithObjects(parent).
		WithIndex(&storagev1alpha1.Snapshot{}, SnapshotChildrenRefFieldIndex, snapshotChildrenRefIndexValues).Build()
	reqs := findParentsReferencingChildSnapshot(ctx, cl, child)
	if len(reqs) != 0 {
		t.Fatalf("expected no parent, got %+v", reqs)
	}
}

func TestSnapshotChildRefIndexKey(t *testing.T) {
	// Exact format: normalized GVK string + NUL + name; identical for the ref and a live child of the
	// same GVK regardless of apiVersion string formatting.
	want := schema.FromAPIVersionAndKind("demo.test/v1", "DemoSnap").String() + "\x00" + "snap-1"
	if got := snapshotChildRefIndexKey("demo.test/v1", "DemoSnap", "snap-1"); got != want {
		t.Fatalf("key=%q want %q", got, want)
	}
	// Incomplete identity yields no key (mirrors childSnapshotRefMatchesUnstructuredChild requirements).
	for _, tc := range []struct{ av, k, n string }{
		{"", "DemoSnap", "snap-1"},
		{"demo.test/v1", "", "snap-1"},
		{"demo.test/v1", "DemoSnap", ""},
	} {
		if got := snapshotChildRefIndexKey(tc.av, tc.k, tc.n); got != "" {
			t.Fatalf("incomplete identity %+v must yield empty key, got %q", tc, got)
		}
	}
}

func TestSnapshotChildrenRefIndexValues(t *testing.T) {
	// Multiple children (mixed kinds) → one key each; incomplete refs skipped.
	snap := &storagev1alpha1.Snapshot{
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{
				{APIVersion: "demo.test/v1", Kind: "KindA", Name: "a"},
				{APIVersion: "demo.test/v1", Kind: "KindB", Name: "b"},
				{APIVersion: "", Kind: "KindC", Name: "c"}, // incomplete -> skipped
			},
		},
	}
	keys := snapshotChildrenRefIndexValues(snap)
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %v", keys)
	}
	wantA := snapshotChildRefIndexKey("demo.test/v1", "KindA", "a")
	wantB := snapshotChildRefIndexKey("demo.test/v1", "KindB", "b")
	if keys[0] != wantA || keys[1] != wantB {
		t.Fatalf("keys=%v want [%q %q]", keys, wantA, wantB)
	}

	// Empty children refs → no keys.
	if got := snapshotChildrenRefIndexValues(&storagev1alpha1.Snapshot{}); len(got) != 0 {
		t.Fatalf("empty childrenSnapshotRefs must yield no keys, got %v", got)
	}
	// Wrong object type → no keys.
	if got := snapshotChildrenRefIndexValues(&storagev1alpha1.SnapshotContent{}); got != nil {
		t.Fatalf("non-Snapshot object must yield nil, got %v", got)
	}
}

// The relay's "bound child not yet listed -> requeue" race mitigation MUST fire only for direct
// children controller-owned by a unified Snapshot. The head/root Snapshot (no ownerReference) and
// grandchildren owned by a domain snapshot MUST NOT match, otherwise the relay hot-loops forever.
func TestChildHasUnifiedSnapshotControllerOwner(t *testing.T) {
	ctrlTrue := true
	ctrlFalse := false
	snapAPIVersion := storagev1alpha1.SchemeGroupVersion.String()

	newChild := func(owners ...metav1.OwnerReference) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.GroupVersionKind{Group: "demo.test", Version: "v1", Kind: "DemoSnap"})
		u.SetNamespace("ns-a")
		u.SetName("child")
		u.SetOwnerReferences(owners)
		return u
	}

	cases := []struct {
		name  string
		child *unstructured.Unstructured
		want  bool
	}{
		{
			name:  "root snapshot has no ownerReference",
			child: newChild(),
			want:  false,
		},
		{
			name: "direct child controller-owned by unified Snapshot",
			child: newChild(metav1.OwnerReference{
				APIVersion: snapAPIVersion, Kind: "Snapshot", Name: "root", Controller: &ctrlTrue,
			}),
			want: true,
		},
		{
			name: "grandchild owned by a domain snapshot",
			child: newChild(metav1.OwnerReference{
				APIVersion: "demo.state-snapshotter.deckhouse.io/v1alpha1",
				Kind:       "DemoVirtualMachineSnapshot", Name: "vm", Controller: &ctrlTrue,
			}),
			want: false,
		},
		{
			name: "non-controller owner Snapshot does not count",
			child: newChild(metav1.OwnerReference{
				APIVersion: snapAPIVersion, Kind: "Snapshot", Name: "root", Controller: &ctrlFalse,
			}),
			want: false,
		},
		{
			name: "Snapshot kind in a different group does not count",
			child: newChild(metav1.OwnerReference{
				APIVersion: "other.group/v1", Kind: "Snapshot", Name: "x", Controller: &ctrlTrue,
			}),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := childHasUnifiedSnapshotControllerOwner(tc.child); got != tc.want {
				t.Fatalf("childHasUnifiedSnapshotControllerOwner=%v, want %v", got, tc.want)
			}
		})
	}
}
