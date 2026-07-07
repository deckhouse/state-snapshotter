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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func domainStaticBindObj(mode string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "demo.state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "DemoVirtualDiskSnapshot",
		"metadata": map[string]interface{}{
			"name":      "disk-snap",
			"namespace": "project-a",
			"uid":       "domain-uid-1",
		},
		"spec": map[string]interface{}{},
	}}
	if mode != "" {
		_ = unstructured.SetNestedField(o.Object, mode, "spec", "mode")
	}
	return o
}

func TestSnapshotIsStaticBind(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want bool
	}{
		{"static-bind", string(storagev1alpha1.SnapshotModeStaticBind), true},
		{"import", string(storagev1alpha1.SnapshotModeImport), false},
		{"capture", string(storagev1alpha1.SnapshotModeCapture), false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		if got := snapshotIsStaticBind(domainStaticBindObj(tc.mode)); got != tc.want {
			t.Errorf("%s: snapshotIsStaticBind=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGenericStaticBindRefMatches(t *testing.T) {
	obj := domainStaticBindObj(string(storagev1alpha1.SnapshotModeStaticBind))
	gv := "demo.state-snapshotter.deckhouse.io/v1alpha1"
	kind := "DemoVirtualDiskSnapshot"
	uid := types.UID("domain-uid-1")
	cases := []struct {
		name string
		ref  *storagev1alpha1.SnapshotSubjectRef
		want bool
	}{
		{"nil", nil, false},
		{"match-no-uid", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: kind, Name: "disk-snap", Namespace: "project-a"}, true},
		{"match-uid", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: kind, Name: "disk-snap", Namespace: "project-a", UID: uid}, true},
		{"uid-mismatch", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: kind, Name: "disk-snap", Namespace: "project-a", UID: types.UID("stale")}, false},
		{"wrong-kind", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: "Other", Name: "disk-snap", Namespace: "project-a"}, false},
		{"wrong-apiversion", &storagev1alpha1.SnapshotSubjectRef{APIVersion: "x/v1", Kind: kind, Name: "disk-snap", Namespace: "project-a"}, false},
		{"wrong-name", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: kind, Name: "other", Namespace: "project-a"}, false},
		{"wrong-namespace", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: kind, Name: "disk-snap", Namespace: "other"}, false},
	}
	for _, tc := range cases {
		if got := genericStaticBindRefMatches(tc.ref, obj); got != tc.want {
			t.Errorf("%s: genericStaticBindRefMatches=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func repointTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	return scheme
}

// contentWithRefAndBin builds a recycle-bin SnapshotContent whose spec.snapshotRef points at the given
// (stale) domain CR identity, with status.parentDeleted set to parentDeleted.
func contentWithRefAndBin(name, refName string, refUID types.UID, parentDeleted bool) *storagev1alpha1.SnapshotContent {
	return &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("content-uid-" + name)},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: "demo.state-snapshotter.deckhouse.io/v1alpha1",
				Kind:       "DemoVirtualDiskSnapshot",
				Name:       refName,
				Namespace:  "project-a",
				UID:        refUID,
			},
		},
		Status: storagev1alpha1.SnapshotContentStatus{ParentDeleted: parentDeleted},
	}
}

// Restore re-point (content-single-writer §4 Slice 3 / decision #8): the binder is the sole writer of
// content.spec, so it re-points a recycle-bin (status.parentDeleted) content's snapshotRef onto the
// re-created domain CR's identity. It MUST NOT re-point a content that is not in the recycle bin (the
// relaxed-CEL transition rule would reject the change), and MUST be a no-op once already re-pointed.
func TestRepointContentSnapshotRefToSelf(t *testing.T) {
	obj := domainStaticBindObj(string(storagev1alpha1.SnapshotModeStaticBind)) // name=disk-snap, uid=domain-uid-1

	t.Run("recycle-bin re-points to the re-created CR", func(t *testing.T) {
		ctx := context.Background()
		scheme := repointTestScheme(t)
		content := contentWithRefAndBin("c-bin", "old-cr", "old-uid", true)
		cl := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(content).WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).Build()
		r := &GenericSnapshotBinderController{Client: cl, APIReader: cl, Scheme: scheme}

		changed, err := r.repointContentSnapshotRefToSelf(ctx, "c-bin", obj)
		if err != nil {
			t.Fatalf("repoint: %v", err)
		}
		if !changed {
			t.Fatalf("expected the recycle-bin content to be re-pointed")
		}
		got := &storagev1alpha1.SnapshotContent{}
		if err := cl.Get(ctx, client.ObjectKey{Name: "c-bin"}, got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.SnapshotRef.Name != "disk-snap" || got.Spec.SnapshotRef.UID != types.UID("domain-uid-1") {
			t.Fatalf("snapshotRef not re-pointed onto the re-created CR: %#v", got.Spec.SnapshotRef)
		}
	})

	t.Run("non-recycle-bin content is left untouched (CEL gate)", func(t *testing.T) {
		ctx := context.Background()
		scheme := repointTestScheme(t)
		content := contentWithRefAndBin("c-live", "old-cr", "old-uid", false)
		cl := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(content).WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).Build()
		r := &GenericSnapshotBinderController{Client: cl, APIReader: cl, Scheme: scheme}

		changed, err := r.repointContentSnapshotRefToSelf(ctx, "c-live", obj)
		if err != nil {
			t.Fatalf("repoint: %v", err)
		}
		if changed {
			t.Fatalf("must NOT re-point a content that is not in the recycle bin")
		}
		got := &storagev1alpha1.SnapshotContent{}
		if err := cl.Get(ctx, client.ObjectKey{Name: "c-live"}, got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.SnapshotRef.Name != "old-cr" || got.Spec.SnapshotRef.UID != types.UID("old-uid") {
			t.Fatalf("non-recycle-bin snapshotRef must be unchanged, got %#v", got.Spec.SnapshotRef)
		}
	})

	t.Run("already re-pointed is a no-op", func(t *testing.T) {
		ctx := context.Background()
		scheme := repointTestScheme(t)
		content := contentWithRefAndBin("c-done", "disk-snap", "domain-uid-1", true)
		cl := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(content).WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).Build()
		r := &GenericSnapshotBinderController{Client: cl, APIReader: cl, Scheme: scheme}

		changed, err := r.repointContentSnapshotRefToSelf(ctx, "c-done", obj)
		if err != nil {
			t.Fatalf("repoint: %v", err)
		}
		if changed {
			t.Fatalf("re-point of an already-correct ref must be a no-op")
		}
	})
}
