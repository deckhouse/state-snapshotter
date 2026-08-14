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
	"sort"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/csdregistry"
)

// recordingCoverage is a snapshotCoverageChecker stub that records every source object that reaches the
// coverage check. Because ensureParentOwnedChildGraphLayer applies the exclude veto BEFORE calling
// coverage.IsCovered, the recorded set equals the set of objects that survived the veto. Returning
// covered=true short-circuits before child creation, isolating the veto gate from the child-snapshot
// plumbing (which is unrelated to this feature and covered elsewhere).
type recordingCoverage struct {
	checked []string
}

func (c *recordingCoverage) IsCovered(_ context.Context, obj *unstructured.Unstructured) (bool, error) {
	c.checked = append(c.checked, obj.GetName())
	return true, nil
}

func (c *recordingCoverage) ObservePlannedSnapshot(context.Context, *unstructured.Unstructured, storagev1alpha1.SnapshotChildRef, *storagev1alpha1.SnapshotContentChildRef) error {
	return nil
}

// TestEnsureParentOwnedChildGraphLayer_ExcludeVeto verifies the two halves of the top-level veto, which are
// one decision and must never drift apart: a source object carrying the exclude label is (1) NOT expanded
// into a child snapshot — it never even reaches the coverage check — and (2) recorded as an explicit
// top-level drop in the returned excludedRefs, with its exact {apiVersion, kind, name} identity. Unlabeled
// siblings must be unaffected, and the veto must ignore the label's value.
func TestEnsureParentOwnedChildGraphLayer_ExcludeVeto(t *testing.T) {
	ns := "ns1"
	sourceGVK := schema.GroupVersionKind{Group: "demo.example.com", Version: "v1", Kind: "DemoThing"}
	sourceGVR := schema.GroupVersionResource{Group: "demo.example.com", Version: "v1", Resource: "demothings"}
	snapshotGVK := schema.GroupVersionKind{Group: "demo.example.com", Version: "v1", Kind: "DemoThingSnapshot"}
	snapshotGVR := schema.GroupVersionResource{Group: "demo.example.com", Version: "v1", Resource: "demothingsnapshots"}

	// vetoValue == "-" marks an object that carries no veto label at all; the two vetoed objects differ only
	// in the label VALUE, pinning that the veto is keyed on presence alone.
	mk := func(name, vetoValue string) *unstructured.Unstructured {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(sourceGVK)
		o.SetNamespace(ns)
		o.SetName(name)
		o.SetUID(types.UID(name + "-uid"))
		if vetoValue != "-" {
			o.SetLabels(map[string]string{storagev1alpha1.ExcludeLabelKey: vetoValue})
		}
		return o
	}

	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{sourceGVR: sourceGVK.Kind + "List"},
		mk("thing-keep", "-"),
		mk("thing-vetoed-empty", ""),
		mk("thing-vetoed-true", "true"),
	)

	r := &SnapshotReconciler{Dynamic: dyn}
	nsSnap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: ns, UID: "root-uid"}}
	mapping := csdregistry.EligibleResourceSnapshotMapping{
		SourceGVR:   sourceGVR,
		SourceGVK:   sourceGVK,
		SnapshotGVR: snapshotGVR,
		SnapshotGVK: snapshotGVK,
	}

	cov := &recordingCoverage{}
	refs, excluded, err := r.ensureParentOwnedChildGraphLayer(context.Background(), nsSnap, mapping, cov)
	if err != nil {
		t.Fatalf("ensureParentOwnedChildGraphLayer: %v", err)
	}

	// The unlabeled object passes the veto and reaches coverage (recorded, then short-circuited as covered).
	// Neither vetoed object ever reaches coverage.
	if len(cov.checked) != 1 || cov.checked[0] != "thing-keep" {
		t.Fatalf("coverage reached %v, want only [thing-keep]", cov.checked)
	}
	if len(refs) != 0 {
		t.Fatalf("expanded refs = %v, want none (coverage short-circuits)", refs)
	}

	gotNames := make([]string, 0, len(excluded))
	for _, e := range excluded {
		if e.Kind != sourceGVK.Kind || e.APIVersion != sourceGVK.GroupVersion().String() {
			t.Fatalf("excluded entry %+v must carry the source identity {apiVersion=%s, kind=%s}", e, sourceGVK.GroupVersion().String(), sourceGVK.Kind)
		}
		gotNames = append(gotNames, e.Name)
	}
	sort.Strings(gotNames)
	want := []string{"thing-vetoed-empty", "thing-vetoed-true"}
	if len(gotNames) != len(want) {
		t.Fatalf("excluded = %v, want %v", gotNames, want)
	}
	for i := range want {
		if gotNames[i] != want[i] {
			t.Fatalf("excluded = %v, want %v", gotNames, want)
		}
	}
	t.Logf("listed 3 source objects: 1 expanded, %d vetoed", len(gotNames))
}
