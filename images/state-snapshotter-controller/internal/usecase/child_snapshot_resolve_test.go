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

package usecase

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func TestResolveChildSnapshotRefToBoundContentName_SameNameDifferentKinds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "ns1"
	name := "same-name"
	gvkA := schema.GroupVersionKind{Group: "generic.state-snapshotter.test", Version: "v1", Kind: "FixtureDomainSnapshotA"}
	gvkB := schema.GroupVersionKind{Group: "generic.state-snapshotter.test", Version: "v1", Kind: "FixtureDomainSnapshotB"}
	a := childSnapUnstructured(ns, name, gvkA, "content-a")
	b := childSnapUnstructured(ns, name, gvkB, "content-b")
	cl := fake.NewClientBuilder().WithRuntimeObjects(a, b).Build()

	refA := storagev1alpha1.SnapshotChildRef{
		APIVersion: "generic.state-snapshotter.test/v1",
		Kind:       "FixtureDomainSnapshotA",
		Name:       name,
	}
	out, err := ResolveChildSnapshotRefToBoundContentName(ctx, cl, refA, ns)
	if err != nil || out != "content-a" {
		t.Fatalf("ref A: got %q err=%v", out, err)
	}
	refB := storagev1alpha1.SnapshotChildRef{
		APIVersion: "generic.state-snapshotter.test/v1",
		Kind:       "FixtureDomainSnapshotB",
		Name:       name,
	}
	outB, err := ResolveChildSnapshotRefToBoundContentName(ctx, cl, refB, ns)
	if err != nil || outB != "content-b" {
		t.Fatalf("ref B: got %q err=%v", outB, err)
	}
}

func TestResolveChildSnapshotRefToBoundContentName_UsesParentNamespace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	parentNS := "parent-ns"
	child := childSnapUnstructured(parentNS, "x", schema.GroupVersionKind{
		Group:   "generic.state-snapshotter.test",
		Version: "v1",
		Kind:    "FixtureDomainSnapshotA",
	}, "content-x")
	cl := fake.NewClientBuilder().WithRuntimeObjects(child).Build()
	got, err := ResolveChildSnapshotRefToBoundContentName(ctx, cl, storagev1alpha1.SnapshotChildRef{
		APIVersion: "generic.state-snapshotter.test/v1",
		Kind:       "FixtureDomainSnapshotA",
		Name:       "x",
	}, parentNS)
	if err != nil || got != "content-x" {
		t.Fatalf("got %q err=%v", got, err)
	}
}

func childSnapUnstructured(ns, name string, gvk schema.GroupVersionKind, boundContent string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	u.SetName(name)
	_ = unstructured.SetNestedField(u.Object, boundContent, "status", "boundSnapshotContentName")
	return u
}
