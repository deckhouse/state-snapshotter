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

func TestResolveResourceSelector_NilStillHonorsExcludeVeto(t *testing.T) {
	// A nil snapshot and a nil resourceSelector both mean "no user filtering", but the exclude veto is
	// ALWAYS ANDed on top: the resolved selector matches everything EXCEPT objects carrying
	// ExcludeLabelKey. It is therefore no longer Empty() (unlike the pre-wave4A "Everything") — that is
	// the key regression this test guards.
	for _, s := range []*Snapshot{nil, {}} {
		sel, err := s.ResolveResourceSelector()
		if err != nil {
			t.Fatalf("ResolveResourceSelector: unexpected error: %v", err)
		}
		if sel.Empty() {
			t.Fatalf("nil selector must carry the exclude veto (not be Empty), got %q", sel.String())
		}
		if !sel.Matches(labels.Set{"any": "value"}) {
			t.Fatal("nil selector must match a labeled object without the exclude veto")
		}
		if !sel.Matches(labels.Set{}) {
			t.Fatal("nil selector must match an unlabeled object")
		}
		if sel.Matches(labels.Set{ExcludeLabelKey: ""}) {
			t.Fatal("nil selector must NOT match an object carrying the exclude veto (empty value)")
		}
		if sel.Matches(labels.Set{ExcludeLabelKey: "true", "any": "value"}) {
			t.Fatal("exclude veto must win regardless of other labels or its value")
		}
	}
}

func TestResolveResourceSelector_EmptySelectorHonorsExcludeVeto(t *testing.T) {
	s := &Snapshot{Spec: SnapshotSpec{ResourceSelector: &metav1.LabelSelector{}}}
	sel, err := s.ResolveResourceSelector()
	if err != nil {
		t.Fatalf("ResolveResourceSelector: unexpected error: %v", err)
	}
	if !sel.Matches(labels.Set{}) || !sel.Matches(labels.Set{"x": "y"}) {
		t.Fatalf("empty selector must match everything without the exclude veto, got %q", sel.String())
	}
	if sel.Matches(labels.Set{ExcludeLabelKey: "any"}) {
		t.Fatal("empty selector must still exclude objects carrying the exclude veto")
	}
}

func TestResolveResourceSelector_ExcludeVetoOverridesPositiveMatch(t *testing.T) {
	// Even an object that the user selector would include is dropped when it also carries the veto.
	s := &Snapshot{Spec: SnapshotSpec{ResourceSelector: &metav1.LabelSelector{
		MatchLabels: map[string]string{"group": "keep"},
	}}}
	sel, err := s.ResolveResourceSelector()
	if err != nil {
		t.Fatalf("ResolveResourceSelector: unexpected error: %v", err)
	}
	if !sel.Matches(labels.Set{"group": "keep"}) {
		t.Fatal("included object without the veto must match")
	}
	if sel.Matches(labels.Set{"group": "keep", ExcludeLabelKey: ""}) {
		t.Fatal("exclude veto must override a positive matchLabels include")
	}
}

func TestResolveResourceSelector_MatchLabelsInclude(t *testing.T) {
	s := &Snapshot{Spec: SnapshotSpec{ResourceSelector: &metav1.LabelSelector{
		MatchLabels: map[string]string{"group": "keep"},
	}}}
	sel, err := s.ResolveResourceSelector()
	if err != nil {
		t.Fatalf("ResolveResourceSelector: unexpected error: %v", err)
	}
	if !sel.Matches(labels.Set{"group": "keep"}) {
		t.Fatal("matchLabels selector must match the included label")
	}
	if sel.Matches(labels.Set{"group": "drop"}) {
		t.Fatal("matchLabels selector must not match a different value")
	}
	if sel.Matches(labels.Set{}) {
		t.Fatal("matchLabels selector must not match an unlabeled object")
	}
}

func TestResolveResourceSelector_MatchExpressionsExclude(t *testing.T) {
	// Combined include (matchLabels) AND exclude (NotIn) in one selector: everything is ANDed.
	s := &Snapshot{Spec: SnapshotSpec{ResourceSelector: &metav1.LabelSelector{
		MatchLabels: map[string]string{"tier": "app"},
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "group", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"drop"}},
			{Key: "debug", Operator: metav1.LabelSelectorOpDoesNotExist},
		},
	}}}
	sel, err := s.ResolveResourceSelector()
	if err != nil {
		t.Fatalf("ResolveResourceSelector: unexpected error: %v", err)
	}
	// tier=app, no group, no debug -> kept (NotIn matches absent key).
	if !sel.Matches(labels.Set{"tier": "app"}) {
		t.Fatal("object with the include label and no excluded labels must match")
	}
	// tier=app, group=keep -> kept (keep is not in {drop}).
	if !sel.Matches(labels.Set{"tier": "app", "group": "keep"}) {
		t.Fatal("group=keep must match a NotIn [drop] requirement")
	}
	// tier=app, group=drop -> excluded.
	if sel.Matches(labels.Set{"tier": "app", "group": "drop"}) {
		t.Fatal("group=drop must be excluded by NotIn [drop]")
	}
	// tier=app, debug present -> excluded by DoesNotExist.
	if sel.Matches(labels.Set{"tier": "app", "debug": "true"}) {
		t.Fatal("presence of debug must be excluded by DoesNotExist")
	}
	// missing the include label -> excluded.
	if sel.Matches(labels.Set{"group": "keep"}) {
		t.Fatal("object missing the include label must not match")
	}
}

func TestResolveResourceSelector_InvalidSelectorReturnsError(t *testing.T) {
	// In/NotIn require a non-empty values set; LabelSelectorAsSelector rejects this. The resolver must
	// surface the error rather than swallow it (defensive: the CRD schema usually catches this at admission).
	s := &Snapshot{Spec: SnapshotSpec{ResourceSelector: &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "group", Operator: metav1.LabelSelectorOpIn, Values: nil},
		},
	}}}
	if _, err := s.ResolveResourceSelector(); err == nil {
		t.Fatal("expected an error for an In requirement with no values")
	}
}
