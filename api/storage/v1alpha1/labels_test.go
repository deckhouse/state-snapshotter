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

package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func TestDeleteProtectedKeyValue(t *testing.T) {
	if LabelDeleteProtected != "state-snapshotter.deckhouse.io/delete-protected" {
		t.Fatalf("unexpected key: %s", LabelDeleteProtected)
	}
	if LabelDeleteProtectedValue != "true" {
		t.Fatalf("unexpected value: %s", LabelDeleteProtectedValue)
	}
}

func TestStampDeleteProtectedAllocatesAndIsIdempotent(t *testing.T) {
	obj := &metav1.ObjectMeta{} // nil labels
	StampDeleteProtected(obj)
	if !IsDeleteProtected(obj) {
		t.Fatalf("expected protected after stamp, labels=%#v", obj.GetLabels())
	}
	// Idempotent: second stamp keeps a single key with the same value and preserves siblings.
	obj.Labels["foo"] = "bar"
	StampDeleteProtected(obj)
	if !IsDeleteProtected(obj) || obj.Labels["foo"] != "bar" {
		t.Fatalf("stamp must be idempotent and preserve other labels, got %#v", obj.GetLabels())
	}
}

func TestIsDeleteProtectedRejectsOtherValues(t *testing.T) {
	obj := &metav1.ObjectMeta{Labels: map[string]string{LabelDeleteProtected: "false"}}
	if IsDeleteProtected(obj) {
		t.Fatalf("value 'false' must not count as protected")
	}
	empty := &metav1.ObjectMeta{}
	if IsDeleteProtected(empty) {
		t.Fatalf("absent label must not count as protected")
	}
}

// TestExcludeVetoSelectorMatches pins the whole truth table of the selector the list-based capture legs
// filter with. The regression it guards is the selector degenerating into "match everything" (an empty
// selector), which would silently capture every vetoed object: hence the explicit Empty() assertion
// alongside the per-object cases. The cases enumerate every shape a label set can take with respect to
// the veto key — absent, present with an empty value, present with a value, present next to unrelated
// labels — because the veto ignores the value and must win regardless of what else the object carries.
func TestExcludeVetoSelectorMatches(t *testing.T) {
	sel := ExcludeVetoSelector()
	if sel.Empty() {
		t.Fatalf("the veto selector must not be empty (an empty selector matches vetoed objects too), got %q", sel.String())
	}

	cases := []struct {
		name   string
		labels labels.Set
		want   bool
	}{
		{name: "no labels at all", labels: labels.Set{}, want: true},
		{name: "unrelated labels only", labels: labels.Set{"app": "web"}, want: true},
		{name: "veto key with an empty value", labels: labels.Set{ExcludeLabelKey: ""}, want: false},
		{name: "veto key with value true", labels: labels.Set{ExcludeLabelKey: "true"}, want: false},
		{name: "veto key with an arbitrary value", labels: labels.Set{ExcludeLabelKey: "whatever"}, want: false},
		{name: "veto key alongside unrelated labels", labels: labels.Set{ExcludeLabelKey: "true", "app": "web"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sel.Matches(tc.labels); got != tc.want {
				t.Fatalf("Matches(%v) = %v, want %v (selector %q)", tc.labels, got, tc.want, sel.String())
			}
		})
	}
	t.Logf("checked %d label sets against %q", len(cases), sel.String())
}

// TestExcludeVetoSelectorIsStableAcrossCalls guards the shared-instance contract stated on
// ExcludeVetoSelector: every caller gets the same selector, and matching through one caller cannot change
// what a later caller sees.
func TestExcludeVetoSelectorIsStableAcrossCalls(t *testing.T) {
	first := ExcludeVetoSelector()
	_ = first.Matches(labels.Set{ExcludeLabelKey: "true"})
	second := ExcludeVetoSelector()
	if first.String() != second.String() {
		t.Fatalf("selector changed between calls: %q then %q", first.String(), second.String())
	}
	if !second.Matches(labels.Set{"app": "web"}) || second.Matches(labels.Set{ExcludeLabelKey: ""}) {
		t.Fatalf("second selector no longer implements the veto, got %q", second.String())
	}
}
