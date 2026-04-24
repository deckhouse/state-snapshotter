/*
Copyright 2025 Flant JSC

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
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// countingLive starts with an empty registry; first TryRefresh installs NamespaceSnapshot mapping.
type countingLive struct {
	reg          *snapshot.GVKRegistry
	refreshCalls int32
}

func (c *countingLive) Current() *snapshot.GVKRegistry {
	return c.reg
}

func (c *countingLive) TryRefresh(context.Context) error {
	atomic.AddInt32(&c.refreshCalls, 1)
	r := snapshot.NewGVKRegistry()
	gv := "storage.deckhouse.io/v1alpha1"
	if err := r.RegisterSnapshotContentMapping("NamespaceSnapshot", gv, "NamespaceSnapshotContent", gv); err != nil {
		return err
	}
	c.reg = r
	return nil
}

func TestEnsureGVKRegistryFromLive_RefreshesOnceThenReturnsRegistry(t *testing.T) {
	ctx := context.Background()
	live := &countingLive{reg: nil}
	reg, err := EnsureGVKRegistryFromLive(ctx, live)
	if err != nil {
		t.Fatalf("EnsureGVKRegistryFromLive: %v", err)
	}
	if reg == nil {
		t.Fatal("expected non-nil registry")
	}
	if atomic.LoadInt32(&live.refreshCalls) != 1 {
		t.Fatalf("expected exactly one TryRefresh, got %d", live.refreshCalls)
	}
}

func TestResolveChildSnapshotRefToBoundContentName_NamespaceSnapshot(t *testing.T) {
	ctx := context.Background()
	scheme := rootCaptureTestScheme(t)
	child := &unstructured.Unstructured{}
	child.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "NamespaceSnapshot",
	})
	child.SetNamespace("ns1")
	child.SetName("child1")
	_ = unstructured.SetNestedField(child.Object, "nsc-child", "status", "boundSnapshotContentName")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(child).Build()
	ref := storagev1alpha1.NamespaceSnapshotChildRef{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "NamespaceSnapshot",
		Namespace:  "ns1",
		Name:       "child1",
	}
	out, err := ResolveChildSnapshotRefToBoundContentName(ctx, cl, ref, "ns1")
	if err != nil {
		t.Fatalf("ResolveChildSnapshotRefToBoundContentName: %v", err)
	}
	if out != "nsc-child" {
		t.Fatalf("bound content: got %q", out)
	}
}
