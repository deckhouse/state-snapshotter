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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/state-snapshotter/api/names"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// seedRestoreContentStatus latches a durable content into the recycle-bin state used by restore:
// parentDeleted=true, Ready=True, a manifestCheckpointName, and the given content-graph children.
func seedRestoreContentStatus(t *testing.T, ctx context.Context, cl client.Client, name string, children ...string) {
	t.Helper()
	c := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: name}, c); err != nil {
		t.Fatalf("get content %s: %v", name, err)
	}
	c.Status.ParentDeleted = true
	c.Status.ManifestCheckpointName = "mcp-" + name
	meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
		Type: snapshotpkg.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed",
	})
	for _, ch := range children {
		c.Status.ChildrenSnapshotContentRefs = append(c.Status.ChildrenSnapshotContentRefs,
			storagev1alpha1.SnapshotContentChildRef{Name: ch})
	}
	if err := cl.Status().Update(ctx, c); err != nil {
		t.Fatalf("seed content status %s: %v", name, err)
	}
}

func restoreRootSnapshot(uid types.UID) *storagev1alpha1.Snapshot {
	return &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns", UID: uid},
		Spec: storagev1alpha1.SnapshotSpec{
			Mode:   storagev1alpha1.SnapshotModeStaticBind,
			Source: &storagev1alpha1.SnapshotSource{SnapshotContentName: "root-content"},
		},
	}
}

func restoreRootContent(rootUID types.UID) *storagev1alpha1.SnapshotContent {
	return &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "Snapshot",
				Name: "snap", Namespace: "ns", UID: rootUID,
			},
		},
	}
}

// TestReconcileStaticBindRestoreTree_DomainRecreateIdempotentRecursion covers the core recycle-bin
// orchestration: walking the durable content tree, re-creating the domain XxxxSnapshot CRs (StaticBind +
// ownerRef), reconstructing the root Snapshot tree, and doing so recursively and idempotently. The child
// content back-reference re-point moved to the domain binder (content-single-writer §4 Slice 3 / decision
// #8: the binder is the sole writer of content.spec), so the core here MUST leave the child content's
// snapshotRef untouched — the binder re-points it when it reconciles the re-created StaticBind CR
// (covered by genericbinder.TestRepointContentSnapshotRefToSelf_*).
func TestReconcileStaticBindRestoreTree_DomainRecreateIdempotentRecursion(t *testing.T) {
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	domainGV := schema.GroupVersion{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1"}
	domainKind := "DemoVirtualDiskSnapshot"
	domainGVK := domainGV.WithKind(domainKind)
	scheme.AddKnownTypeWithName(domainGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(domainGV.WithKind(domainKind+"List"), &unstructured.UnstructuredList{})

	rootUID := types.UID("root-uid")
	childUID := types.UID("child-content-uid")
	grandUID := types.UID("grand-content-uid")

	snap := restoreRootSnapshot(rootUID)
	rootContent := restoreRootContent(rootUID)
	childContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "child-content", UID: childUID},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: domainGV.String(), Kind: domainKind, Name: "old-domain-cr", Namespace: "ns", UID: types.UID("old-domain-uid"),
			},
		},
	}
	grandContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "grand-content", UID: grandUID},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: domainGV.String(), Kind: domainKind, Name: "old-grand-cr", Namespace: "ns", UID: types.UID("old-grand-uid"),
			},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(snap, rootContent, childContent, grandContent).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()

	seedRestoreContentStatus(t, ctx, cl, "root-content", "child-content")
	seedRestoreContentStatus(t, ctx, cl, "child-content", "grand-content")
	seedRestoreContentStatus(t, ctx, cl, "grand-content")

	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	// First reconcile binds the root; second runs the tree orchestration.
	if _, err := r.reconcileStaticBind(ctx, snap); err != nil {
		t.Fatalf("bind: %v", err)
	}
	bound := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "snap"}, bound); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileStaticBind(ctx, bound); err != nil {
		t.Fatalf("orchestrate: %v", err)
	}

	childCRName := names.ChildSnapshotName(rootUID, childUID)
	grandCRName := names.ChildSnapshotName(rootUID, grandUID)

	assertDomainCR := func(name, contentName string) {
		t.Helper()
		cr := &unstructured.Unstructured{}
		cr.SetGroupVersionKind(domainGVK)
		if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: name}, cr); err != nil {
			t.Fatalf("get re-created domain CR %s: %v", name, err)
		}
		if mode, _, _ := unstructured.NestedString(cr.Object, "spec", "mode"); mode != string(storagev1alpha1.SnapshotModeStaticBind) {
			t.Fatalf("%s spec.mode=%q, want StaticBind", name, mode)
		}
		if src, _, _ := unstructured.NestedString(cr.Object, "spec", "source", "snapshotContentName"); src != contentName {
			t.Fatalf("%s spec.source.snapshotContentName=%q, want %q", name, src, contentName)
		}
		ors := cr.GetOwnerReferences()
		if len(ors) != 1 || ors[0].Kind != "Snapshot" || ors[0].Name != "snap" || ors[0].UID != rootUID {
			t.Fatalf("%s ownerReferences=%#v, want single Snapshot/snap owner", name, ors)
		}
	}
	assertDomainCR(childCRName, "child-content")
	assertDomainCR(grandCRName, "grand-content")

	// The core no longer re-points the child content's back-reference (that moved to the binder). It must
	// be left exactly as it was in the recycle bin (pointing at the ORIGINAL, now-deleted CR); the binder
	// re-points it to the re-created CR when it reconciles the StaticBind leaf.
	gotChild := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "child-content"}, gotChild); err != nil {
		t.Fatal(err)
	}
	if gotChild.Spec.SnapshotRef.Name != "old-domain-cr" || gotChild.Spec.SnapshotRef.UID != types.UID("old-domain-uid") {
		t.Fatalf("core must NOT re-point the child content snapshotRef (binder owns content.spec now), got %#v", gotChild.Spec.SnapshotRef)
	}

	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "snap"}, bound); err != nil {
		t.Fatal(err)
	}
	foundChild := false
	for _, ref := range bound.Status.ChildrenSnapshotRefs {
		if ref.APIVersion == domainGV.String() && ref.Kind == domainKind && ref.Name == childCRName {
			foundChild = true
		}
	}
	if !foundChild {
		t.Fatalf("root Snapshot childrenSnapshotRefs missing re-created domain child %q: %#v", childCRName, bound.Status.ChildrenSnapshotRefs)
	}

	// Idempotency: a further reconcile creates no duplicates.
	if _, err := r.reconcileStaticBind(ctx, bound); err != nil {
		t.Fatalf("idempotent reconcile: %v", err)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(domainGV.WithKind(domainKind + "List"))
	if err := cl.List(ctx, list, client.InNamespace("ns")); err != nil {
		t.Fatalf("list domain CRs: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("want exactly 2 re-created domain CRs (idempotent), got %d", len(list.Items))
	}
}

// TestReconcileStaticBindRestoreTree_OrphanVolumeLeaf covers the orphan volume-node leaf restore: the
// durable leaf content is NOT re-created, but its CSI VolumeSnapshot handle is re-created (pre-provisioned
// to the surviving VolumeSnapshotContent), the leaf back-reference is re-pointed to the new handle uid, and
// the INV-ORPHAN4 handle (VolumeSnapshot.status.boundSnapshotContentName) is written.
func TestReconcileStaticBindRestoreTree_OrphanVolumeLeaf(t *testing.T) {
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	scheme.AddKnownTypeWithName(csiVolumeSnapshotGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{
		Group: csiVolumeSnapshotGVK.Group, Version: csiVolumeSnapshotGVK.Version, Kind: "VolumeSnapshotList",
	}, &unstructured.UnstructuredList{})

	rootUID := types.UID("root-uid")
	pvcUID := "pvc-uid-1"

	snap := restoreRootSnapshot(rootUID)
	rootContent := restoreRootContent(rootUID)
	leafContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "leaf-content",
			UID:    types.UID("leaf-uid"),
			Labels: map[string]string{snapshotpkg.LabelChildVolumeNode: "true"},
		},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: snapshotpkg.CSISnapshotAPIVersion, Kind: snapshotpkg.KindVolumeSnapshot,
				Name: "old-vs", Namespace: "ns", UID: types.UID("old-vs-uid"),
			},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(snap, rootContent, leafContent).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}, csiVolumeSnapshotStatusStub()).Build()

	seedRestoreContentStatus(t, ctx, cl, "root-content", "leaf-content")
	// leaf content: recycle-bin + its durable dataRef -> surviving VolumeSnapshotContent.
	leaf := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "leaf-content"}, leaf); err != nil {
		t.Fatal(err)
	}
	leaf.Status.ParentDeleted = true
	leaf.Status.ManifestCheckpointName = "mcp-leaf"
	leaf.Status.Data = &storagev1alpha1.SnapshotDataBinding{
		Source: storagev1alpha1.SnapshotSubjectRef{
			UID: types.UID(pvcUID), APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: "ns", Name: "pvc-1",
		},
		Artifact: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: snapshotpkg.CSISnapshotAPIVersion, Kind: snapshotpkg.KindVolumeSnapshotContent, Name: "vsc-durable",
		},
	}
	meta.SetStatusCondition(&leaf.Status.Conditions, metav1.Condition{
		Type: snapshotpkg.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed",
	})
	if err := cl.Status().Update(ctx, leaf); err != nil {
		t.Fatalf("seed leaf status: %v", err)
	}

	r := &SnapshotReconciler{Client: cl, APIReader: cl}
	if _, err := r.reconcileStaticBind(ctx, snap); err != nil {
		t.Fatalf("bind: %v", err)
	}
	bound := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "snap"}, bound); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileStaticBind(ctx, bound); err != nil {
		t.Fatalf("orchestrate: %v", err)
	}

	vsName := names.OrphanVolumeSnapshotName(rootUID, types.UID(pvcUID))
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: vsName}, vs); err != nil {
		t.Fatalf("get re-created VolumeSnapshot %s: %v", vsName, err)
	}
	if src, _, _ := unstructured.NestedString(vs.Object, "spec", "source", "volumeSnapshotContentName"); src != "vsc-durable" {
		t.Fatalf("VS spec.source.volumeSnapshotContentName=%q, want pre-provisioned vsc-durable", src)
	}
	if boundName, _, _ := unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName"); boundName != "leaf-content" {
		t.Fatalf("VS status.boundSnapshotContentName=%q, want leaf-content (INV-ORPHAN4)", boundName)
	}
	ors := vs.GetOwnerReferences()
	if len(ors) != 1 || ors[0].Kind != "Snapshot" || ors[0].UID != rootUID {
		t.Fatalf("VS ownerReferences=%#v, want single Snapshot/snap owner", ors)
	}

	// The durable leaf content must survive (not be re-created) with its dataRef intact and re-pointed ref.
	gotLeaf := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "leaf-content"}, gotLeaf); err != nil {
		t.Fatal(err)
	}
	if gotLeaf.UID != types.UID("leaf-uid") {
		t.Fatalf("leaf content was re-created (uid changed to %q); it must survive", gotLeaf.UID)
	}
	if gotLeaf.Status.Data == nil || gotLeaf.Status.Data.Artifact.Name != "vsc-durable" {
		t.Fatalf("leaf content data lost: %#v", gotLeaf.Status.Data)
	}
	if gotLeaf.Spec.SnapshotRef.Name != vsName || gotLeaf.Spec.SnapshotRef.UID != vs.GetUID() {
		t.Fatalf("leaf content snapshotRef not re-pointed to new VS handle: %#v (want name=%q uid=%q)", gotLeaf.Spec.SnapshotRef, vsName, vs.GetUID())
	}

	foundLeaf := false
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "snap"}, bound); err != nil {
		t.Fatal(err)
	}
	for _, ref := range bound.Status.ChildrenSnapshotRefs {
		if ref.APIVersion == snapshotpkg.CSISnapshotAPIVersion && ref.Kind == snapshotpkg.KindVolumeSnapshot && ref.Name == vsName {
			foundLeaf = true
		}
	}
	if !foundLeaf {
		t.Fatalf("root Snapshot childrenSnapshotRefs missing VolumeSnapshot leaf %q: %#v", vsName, bound.Status.ChildrenSnapshotRefs)
	}
}
