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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// The durable aggregate on a node = this node's OWN direct exclusions (read from the owning snapshot's
// captureState.domainSpecificController.excludedRefs) UNION the excludedRefs aggregate of every declared
// child content. Verifies the domain-input -> content-aggregate fold and the root/domain uniform read.
func TestComputeExcludedRefsAggregate_OwnUnionChildren(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)

	own := storagev1alpha1.ExcludedObjectRef{APIVersion: "demo/v1", Kind: "Disk", Name: "disk-own"}
	childEx := storagev1alpha1.ExcludedObjectRef{APIVersion: "demo/v1", Kind: "Disk", Name: "disk-child"}

	owner := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "owner"},
		Status: storagev1alpha1.SnapshotStatus{
			CaptureState: &storagev1alpha1.CaptureStateStatus{
				DomainSpecificController: &storagev1alpha1.DomainSpecificControllerCaptureState{
					ExcludedRefs: []storagev1alpha1.ExcludedObjectRef{own},
				},
			},
		},
	}
	childContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "child-content"},
		Status:     storagev1alpha1.SnapshotContentStatus{ExcludedRefs: []storagev1alpha1.ExcludedObjectRef{childEx}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, childContent).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	parent := contentWithSnapshotRef("parent-content", "", "ns1", "owner", "child-content")
	got, err := r.computeExcludedRefsAggregate(ctx, parent)
	if err != nil {
		t.Fatalf("computeExcludedRefsAggregate: %v", err)
	}
	want := []storagev1alpha1.ExcludedObjectRef{childEx, own} // sorted: disk-child < disk-own
	if !excludedObjectRefsEqualIgnoreOrder(got, want) {
		t.Fatalf("aggregate = %+v, want %+v (own UNION children)", got, want)
	}
}

// The aggregate is monotonic: a previously-published exclusion is retained even if the child content that
// contributed it is momentarily NotFound (e.g. during degradation). An immutable snapshot's exclusion set
// never legitimately shrinks.
func TestComputeExcludedRefsAggregate_MonotonicOnMissingChild(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)

	stale := storagev1alpha1.ExcludedObjectRef{APIVersion: "demo/v1", Kind: "Disk", Name: "disk-gone"}

	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	// parent already published `stale`, references a child that does not exist, and has no owning snapshot.
	parent := contentWithSnapshotRef("parent-content", "", "ns1", "missing-owner", "missing-child")
	status, _ := parent.Object["status"].(map[string]interface{})
	status["excludedRefs"] = excludedRefsToUnstructured([]storagev1alpha1.ExcludedObjectRef{stale})
	parent.Object["status"] = status

	got, err := r.computeExcludedRefsAggregate(ctx, parent)
	if err != nil {
		t.Fatalf("computeExcludedRefsAggregate: %v", err)
	}
	if !excludedObjectRefsEqualIgnoreOrder(got, []storagev1alpha1.ExcludedObjectRef{stale}) {
		t.Fatalf("aggregate = %+v, want retained {disk-gone} (monotonic)", got)
	}
}

func TestParseAndSerializeExcludedRefsRoundTrip(t *testing.T) {
	refs := []storagev1alpha1.ExcludedObjectRef{
		{APIVersion: "demo/v1", Kind: "Disk", Name: "a"},
		{APIVersion: "demo/v1", Kind: "Disk", Name: "b"},
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"excludedRefs": excludedRefsToUnstructured(refs)},
	}}
	got := parseExcludedRefs(obj, "status", "excludedRefs")
	if !excludedObjectRefsEqualIgnoreOrder(got, refs) {
		t.Fatalf("round-trip = %+v, want %+v", got, refs)
	}
	// Malformed / partial entries are skipped.
	bad := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"excludedRefs": []interface{}{
			map[string]interface{}{"apiVersion": "demo/v1", "kind": "Disk"}, // missing name
			"not-a-map",
		}},
	}}
	if got := parseExcludedRefs(bad, "status", "excludedRefs"); len(got) != 0 {
		t.Fatalf("parse of malformed entries = %+v, want empty", got)
	}
}
