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

package snapshotbinding

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestStableContentName(t *testing.T) {
	got := StableContentName("snap", types.UID("12345678-1234-1234-1234-123456789abc"))
	if got != "snap-content-12345678" {
		t.Fatalf("unexpected stable content name: %q", got)
	}
}

func TestSnapshotSubjectRef(t *testing.T) {
	ref := SnapshotSubjectRef("demo.io/v1", "DemoSnapshot", "snap", "ns", types.UID("uid-1"))
	if ref.APIVersion != "demo.io/v1" || ref.Kind != "DemoSnapshot" || ref.Name != "snap" || ref.Namespace != "ns" || ref.UID != "uid-1" {
		t.Fatalf("unexpected snapshot subject ref: %#v", ref)
	}
}

func TestPatchUnstructuredBoundContentNameIsIdempotent(t *testing.T) {
	ctx := context.Background()
	gvk := schema.GroupVersionKind{Group: "demo.io", Version: "v1", Kind: "DemoSnapshot"}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName("snap")
	obj.SetNamespace("ns")
	obj.Object["status"] = map[string]interface{}{}

	c := ctrlfake.NewClientBuilder().
		WithScheme(runtime.NewScheme()).
		WithObjects(obj).
		WithStatusSubresource(obj).
		Build()

	key := types.NamespacedName{Namespace: "ns", Name: "snap"}
	if err := PatchUnstructuredBoundContentName(ctx, c, key, gvk, "content-a"); err != nil {
		t.Fatalf("patch bound content name: %v", err)
	}
	if err := PatchUnstructuredBoundContentName(ctx, c, key, gvk, "content-a"); err != nil {
		t.Fatalf("idempotent patch bound content name: %v", err)
	}

	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, key, fresh); err != nil {
		t.Fatalf("get patched object: %v", err)
	}
	got, _, err := unstructured.NestedString(fresh.Object, "status", "boundSnapshotContentName")
	if err != nil {
		t.Fatalf("read bound content name: %v", err)
	}
	if got != "content-a" {
		t.Fatalf("unexpected bound content name: %q", got)
	}
}
