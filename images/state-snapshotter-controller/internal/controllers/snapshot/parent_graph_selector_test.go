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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/csdregistry"
)

// recordingCoverage is a snapshotCoverageChecker stub that records every source object that reaches the
// coverage check. Because ensureParentOwnedChildGraphLayer applies the resourceSelector BEFORE calling
// coverage.IsCovered, the recorded set equals the set of objects that passed the selector. Returning
// covered=true short-circuits before child creation, isolating the selector gate from the child-snapshot
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

func TestEnsureParentOwnedChildGraphLayer_ResourceSelector(t *testing.T) {
	ns := "ns1"
	sourceGVK := schema.GroupVersionKind{Group: "demo.example.com", Version: "v1", Kind: "DemoThing"}
	sourceGVR := schema.GroupVersionResource{Group: "demo.example.com", Version: "v1", Resource: "demothings"}
	snapshotGVK := schema.GroupVersionKind{Group: "demo.example.com", Version: "v1", Kind: "DemoThingSnapshot"}
	snapshotGVR := schema.GroupVersionResource{Group: "demo.example.com", Version: "v1", Resource: "demothingsnapshots"}

	mk := func(name, group string) *unstructured.Unstructured {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(sourceGVK)
		o.SetNamespace(ns)
		o.SetName(name)
		o.SetUID(types.UID(name + "-uid"))
		if group != "" {
			o.SetLabels(map[string]string{"group": group})
		}
		return o
	}

	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{sourceGVR: sourceGVK.Kind + "List"},
		mk("thing-keep", "keep"),
		mk("thing-drop", "drop"),
		mk("thing-nolabel", ""),
	)

	r := &SnapshotReconciler{Dynamic: dyn}
	nsSnap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: ns, UID: "root-uid"}}
	mapping := csdregistry.EligibleResourceSnapshotMapping{
		SourceGVR:   sourceGVR,
		SourceGVK:   sourceGVK,
		SnapshotGVR: snapshotGVR,
		SnapshotGVK: snapshotGVK,
	}

	run := func(t *testing.T, selector labels.Selector) []string {
		t.Helper()
		cov := &recordingCoverage{}
		if _, err := r.ensureParentOwnedChildGraphLayer(context.Background(), nsSnap, mapping, cov, selector, &childGraphPlanningTimings{}); err != nil {
			t.Fatalf("ensureParentOwnedChildGraphLayer: %v", err)
		}
		sort.Strings(cov.checked)
		return cov.checked
	}

	assertSet := func(t *testing.T, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("expanded set = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("expanded set = %v, want %v", got, want)
			}
		}
	}

	t.Run("nil selector expands all top-level domain objects", func(t *testing.T) {
		assertSet(t, run(t, nil), []string{"thing-drop", "thing-keep", "thing-nolabel"})
	})

	t.Run("Everything selector expands all", func(t *testing.T) {
		assertSet(t, run(t, labels.Everything()), []string{"thing-drop", "thing-keep", "thing-nolabel"})
	})

	t.Run("matchLabels include expands only matching", func(t *testing.T) {
		sel, err := labels.Parse("group=keep")
		if err != nil {
			t.Fatalf("labels.Parse: %v", err)
		}
		assertSet(t, run(t, sel), []string{"thing-keep"})
	})

	t.Run("NotIn exclude drops only matching, keeps unlabeled", func(t *testing.T) {
		sel, err := labels.Parse("group notin (drop)")
		if err != nil {
			t.Fatalf("labels.Parse: %v", err)
		}
		assertSet(t, run(t, sel), []string{"thing-keep", "thing-nolabel"})
	})
}
