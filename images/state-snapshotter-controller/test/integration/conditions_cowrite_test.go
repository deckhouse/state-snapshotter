//go:build integration
// +build integration

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

package integration

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// These tests validate the D4a co-write mechanism in isolation against a real apiserver: two
// independent writers (e.g. core writing Ready, another writer setting a second condition such as
// ManifestsReady) mutate the SAME status.conditions array. JSON merge patch replaces the
// whole array, so a bare MergeFrom would let one writer silently drop the other's condition.
// MergeFromWithOptimisticLock adds a resourceVersion precondition: a stale write gets 409 Conflict,
// and RetryOnConflict re-reads the fresh object (already carrying the other condition) before
// re-applying only its own. We use the inert cluster-scoped TestSnapshotContent kind (it has a
// conditions schema and no controller writes to it) to avoid reconciler interference.
var _ = Describe("Integration: D4a conditions co-write", func() {
	testContentGVK := schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "TestSnapshotContent"}

	setCond := func(obj *unstructured.Unstructured, condType string) {
		conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		entry := map[string]interface{}{
			"type":               condType,
			"status":             "True",
			"reason":             "CoWriteTest",
			"message":            "co-write test",
			"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
		}
		replaced := false
		for i := range conds {
			if c, ok := conds[i].(map[string]interface{}); ok && c["type"] == condType {
				conds[i] = entry
				replaced = true
			}
		}
		if !replaced {
			conds = append(conds, entry)
		}
		Expect(unstructured.SetNestedSlice(obj.Object, conds, "status", "conditions")).To(Succeed())
	}

	hasCond := func(obj *unstructured.Unstructured, condType string) bool {
		conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		for i := range conds {
			if c, ok := conds[i].(map[string]interface{}); ok && c["type"] == condType {
				return true
			}
		}
		return false
	}

	It("optimistic-lock merge patch lets two writers co-own one conditions array", func() {
		ctx := context.Background()

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(testContentGVK)
		obj.SetGenerateName("d4a-cowrite-")
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		name := obj.GetName()
		key := client.ObjectKey{Name: name}
		DeferCleanup(func() {
			victim := &unstructured.Unstructured{}
			victim.SetGroupVersionKind(testContentGVK)
			victim.SetName(name)
			_ = k8sClient.Delete(context.Background(), victim)
		})

		// Capture a stale base for "writer B" BEFORE "writer A" mutates the object.
		stale := &unstructured.Unstructured{}
		stale.SetGroupVersionKind(testContentGVK)
		Expect(k8sClient.Get(ctx, key, stale)).To(Succeed())
		staleBase := stale.DeepCopy()

		// Writer A (core) sets Ready and advances resourceVersion.
		writerA := &unstructured.Unstructured{}
		writerA.SetGroupVersionKind(testContentGVK)
		Expect(k8sClient.Get(ctx, key, writerA)).To(Succeed())
		baseA := writerA.DeepCopy()
		setCond(writerA, snapshot.ConditionReady)
		Expect(k8sClient.Status().Patch(ctx, writerA, client.MergeFromWithOptions(baseA, client.MergeFromWithOptimisticLock{}))).To(Succeed())

		// Writer B patches ManifestsReady from the now-stale base: optimistic lock must 409.
		writerBStale := stale.DeepCopy()
		setCond(writerBStale, snapshot.ConditionManifestsReady)
		err := k8sClient.Status().Patch(ctx, writerBStale, client.MergeFromWithOptions(staleBase, client.MergeFromWithOptimisticLock{}))
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "stale optimistic-lock status patch must be rejected with 409 Conflict")

		// Writer B with RetryOnConflict re-reads the fresh object (already carrying Ready) and re-applies only its condition.
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			fresh := &unstructured.Unstructured{}
			fresh.SetGroupVersionKind(testContentGVK)
			if getErr := k8sClient.Get(ctx, key, fresh); getErr != nil {
				return getErr
			}
			base := fresh.DeepCopy()
			setCond(fresh, snapshot.ConditionManifestsReady)
			return k8sClient.Status().Patch(ctx, fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		})).To(Succeed())

		// Both conditions must survive — neither writer clobbered the other.
		final := &unstructured.Unstructured{}
		final.SetGroupVersionKind(testContentGVK)
		Expect(k8sClient.Get(ctx, key, final)).To(Succeed())
		Expect(hasCond(final, snapshot.ConditionReady)).To(BeTrue(), "Ready (writer A) must survive the ManifestsReady co-write")
		Expect(hasCond(final, snapshot.ConditionManifestsReady)).To(BeTrue(), "ManifestsReady (writer B) must be present")
	})
})
