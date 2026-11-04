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
	"context"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
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

// drainEvents returns what the fake recorder has received so far.
func drainEvents(rec *record.FakeRecorder) []string {
	var got []string
	for {
		select {
		case e := <-rec.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

func heldTestContent(uid types.UID, publishedCheckpoint string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("SnapshotContent"))
	o.SetName(projTestContent)
	o.SetUID(uid)
	_ = unstructured.SetNestedField(o.Object, publishedCheckpoint, "status", "manifestCheckpointName")
	return o
}

// A projection that refuses to move a published identity re-decides that on every pass while the divergent
// candidate is around, and a content that is not Ready requeues itself every 500ms. The refusal is recorded
// as one Warning Event, not one per pass; a refusal of another candidate, or of the same one on a content
// recreated under the same name, is a new Event.
func TestPublishedIdentityHeldIsRecordedOncePerRefusal(t *testing.T) {
	ctx := context.Background()
	owner := &unstructured.Unstructured{}
	owner.SetUID("owner-uid")
	candidate := usecase.ReconstructedManifestCheckpointName(owner.GetUID(), "")
	cl := fake.NewClientBuilder().
		WithScheme(captureLegsScheme(t)).
		WithObjects(&ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: candidate}}).
		Build()
	rec := record.NewFakeRecorder(16)
	r := &SnapshotContentController{Client: cl, Recorder: rec}

	pass := func(content *unstructured.Unstructured) []string {
		t.Helper()
		if _, err := r.projectImportManifestCheckpointName(ctx, content, owner); err != nil {
			t.Fatalf("projectImportManifestCheckpointName: %v", err)
		}
		return drainEvents(rec)
	}

	content := heldTestContent("content-uid", "mcp-published")
	got := pass(content)
	if len(got) != 1 || !strings.HasPrefix(got[0], "Warning "+EventReasonPublishedIdentityHeld+" ") {
		t.Fatalf("first refusal: Events = %q, want one Warning %s", got, EventReasonPublishedIdentityHeld)
	}
	if !strings.Contains(got[0], `"mcp-published"`) || !strings.Contains(got[0], `"`+candidate+`"`) {
		t.Fatalf("the Event should name the published checkpoint and the refused one, got %q", got[0])
	}
	for i := range 3 {
		if got := pass(content); len(got) != 0 {
			t.Fatalf("repeat %d of the same refusal: Events = %q, want none", i+1, got)
		}
	}

	if got := pass(heldTestContent("content-uid", "mcp-other")); len(got) != 1 {
		t.Fatalf("a refusal over another published name: Events = %q, want one", got)
	}

	r.heldNotes.forget(types.NamespacedName{Name: projTestContent})
	if got := pass(heldTestContent("content-uid", "mcp-other")); len(got) != 1 {
		t.Fatalf("a refusal after the content was forgotten: Events = %q, want one", got)
	}
	if got := pass(heldTestContent("recreated-uid", "mcp-other")); len(got) != 1 {
		t.Fatalf("the same refusal on a recreated content: Events = %q, want one", got)
	}
}

// The manifest and the data identity are held independently, and holding both in the same pass must not
// make them take turns at the one remembered refusal and record both on every pass.
func TestPublishedIdentityHeldKeepsTheTwoIdentitiesApart(t *testing.T) {
	ctx := context.Background()
	rec := record.NewFakeRecorder(16)
	r := &SnapshotContentController{Recorder: rec}
	content := heldTestContent("content-uid", "mcp-published")

	var all []string
	for range 3 {
		r.notePublishedIdentityHeld(ctx, content, heldLegManifests, "manifests detail")
		r.notePublishedIdentityHeld(ctx, content, heldLegData, "data detail")
		all = append(all, drainEvents(rec)...)
	}
	if len(all) != 2 ||
		!slices.ContainsFunc(all, func(e string) bool { return strings.HasSuffix(e, "manifests detail") }) ||
		!slices.ContainsFunc(all, func(e string) bool { return strings.HasSuffix(e, "data detail") }) {
		t.Fatalf("Events = %q, want one per identity", all)
	}
}

// The forgetting is wired to the pass that finds the content gone.
func TestReconcileForgetsHeldRefusalsOfAGoneContent(t *testing.T) {
	r := &SnapshotContentController{Client: fake.NewClientBuilder().WithScheme(captureLegsScheme(t)).Build()}
	key := types.NamespacedName{Name: projTestContent}
	r.heldNotes.first(key, heldLegManifests, "content-uid", "msg")

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !r.heldNotes.first(key, heldLegManifests, "content-uid", "msg") {
		t.Fatal("the refusal of a content that is gone should have been forgotten")
	}
}

func TestDescribeDataBinding(t *testing.T) {
	if got := describeDataBinding(nil); got != "a capture that produced no artifact" {
		t.Fatalf("nil binding: %q", got)
	}
	got := describeDataBinding(&storagev1alpha1.SnapshotDataBinding{
		SourceRef:   storagev1alpha1.SnapshotSubjectRef{Kind: "PersistentVolumeClaim", Namespace: "ns", Name: "pvc"},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{Kind: "VolumeSnapshotContent", Name: "vsc"},
	})
	if want := `VolumeSnapshotContent "vsc" of PersistentVolumeClaim ns/pvc`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
