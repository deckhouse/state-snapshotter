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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func TestStaticBindRefMatches(t *testing.T) {
	snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns", UID: types.UID("snap-uid")}}
	gv := storagev1alpha1.SchemeGroupVersion.String()
	cases := []struct {
		name string
		ref  *storagev1alpha1.SnapshotSubjectRef
		want bool
	}{
		{"nil", nil, false},
		{"match-no-uid", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: "Snapshot", Name: "snap", Namespace: "ns"}, true},
		{"match-uid", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: "Snapshot", Name: "snap", Namespace: "ns", UID: types.UID("snap-uid")}, true},
		{"uid-mismatch", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: "Snapshot", Name: "snap", Namespace: "ns", UID: types.UID("stale")}, false},
		{"wrong-kind", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: "VolumeSnapshot", Name: "snap", Namespace: "ns"}, false},
		{"wrong-apiversion", &storagev1alpha1.SnapshotSubjectRef{APIVersion: "x/v1", Kind: "Snapshot", Name: "snap", Namespace: "ns"}, false},
		{"wrong-name", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: "Snapshot", Name: "other", Namespace: "ns"}, false},
		{"wrong-namespace", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: "Snapshot", Name: "snap", Namespace: "other"}, false},
	}
	for _, tc := range cases {
		if got := staticBindRefMatches(tc.ref, snap); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func staticBindSnapshot() *storagev1alpha1.Snapshot {
	return &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns", UID: types.UID("snap-uid")},
		Spec:       storagev1alpha1.SnapshotSpec{Source: &storagev1alpha1.SnapshotSource{SnapshotContentName: "c-pre"}},
	}
}

func readyCond(t *testing.T, conds []metav1.Condition) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(conds, snapshotpkg.ConditionReady)
}

func TestReconcileStaticBind_ContentNotFound(t *testing.T) {
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	snap := staticBindSnapshot()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	res, err := r.reconcileStaticBind(ctx, snap)
	if err != nil {
		t.Fatalf("reconcileStaticBind: %v", err)
	}
	if res.RequeueAfter != staticBindContentPollInterval {
		t.Fatalf("want RequeueAfter=%v, got %#v", staticBindContentPollInterval, res)
	}
	got := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "snap"}, got); err != nil {
		t.Fatal(err)
	}
	cond := readyCond(t, got.Status.Conditions)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != snapshotpkg.ReasonSourceContentNotFound {
		t.Fatalf("want Ready=False/SourceContentNotFound, got %#v", cond)
	}
}

func TestReconcileStaticBind_RefMismatchTerminal(t *testing.T) {
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	gv := storagev1alpha1.SchemeGroupVersion.String()
	snap := staticBindSnapshot()
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "c-pre"},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: "Snapshot", Name: "someone-else", Namespace: "ns"},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap, content).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	res, err := r.reconcileStaticBind(ctx, snap)
	if err != nil {
		t.Fatalf("reconcileStaticBind: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatalf("ref mismatch must be terminal (no requeue), got %#v", res)
	}
	got := &storagev1alpha1.Snapshot{}
	_ = cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "snap"}, got)
	cond := readyCond(t, got.Status.Conditions)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != snapshotpkg.ReasonSnapshotContentMisbound {
		t.Fatalf("want Ready=False/SnapshotContentMisbound, got %#v", cond)
	}
	if got.Status.BoundSnapshotContentName != "" {
		t.Fatalf("must not bind on mismatch, got bound=%q", got.Status.BoundSnapshotContentName)
	}
}

func TestReconcileStaticBind_HappyBindAndMirror(t *testing.T) {
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	gv := storagev1alpha1.SchemeGroupVersion.String()
	snap := staticBindSnapshot()
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "c-pre"},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: "Snapshot", Name: "snap", Namespace: "ns", UID: types.UID("snap-uid")},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap, content).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
	// Publish the bound content as Ready=True so the mirror can copy it.
	content.Status.ManifestCheckpointName = "mcp-x"
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{
		Type: snapshotpkg.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed", Message: "bound",
	})
	if err := cl.Status().Update(ctx, content); err != nil {
		t.Fatalf("seed content status: %v", err)
	}
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	// First reconcile binds and requeues.
	res, err := r.reconcileStaticBind(ctx, snap)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if !res.Requeue {
		t.Fatalf("first reconcile must requeue after binding, got %#v", res)
	}
	bound := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "snap"}, bound); err != nil {
		t.Fatal(err)
	}
	if bound.Status.BoundSnapshotContentName != "c-pre" {
		t.Fatalf("want boundSnapshotContentName=c-pre, got %q", bound.Status.BoundSnapshotContentName)
	}

	// Second reconcile mirrors the content's Ready onto the Snapshot.
	if _, err := r.reconcileStaticBind(ctx, bound); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	final := &storagev1alpha1.Snapshot{}
	_ = cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "snap"}, final)
	cond := readyCond(t, final.Status.Conditions)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("want Ready=True mirrored from content, got %#v", cond)
	}
}
