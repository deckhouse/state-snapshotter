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

package namespacemanifest

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/labels"
)

func labeledCM(group string) func(map[string]interface{}) {
	return func(o map[string]interface{}) {
		o["metadata"].(map[string]interface{})["labels"] = map[string]interface{}{"group": group}
	}
}

func TestBuildManifestCaptureTargets_ResourceSelector(t *testing.T) {
	entries := defaultGVRs()
	keep := obj("v1", "ConfigMap", "cm-keep", labeledCM("keep"))
	drop := obj("v1", "ConfigMap", "cm-drop", labeledCM("drop"))
	noLabel := obj("v1", "ConfigMap", "cm-nolabel", nil)
	dyn := dynamicFromEntries(entries, keep, drop, noLabel)

	build := func(selector labels.Selector) map[string]struct{} {
		t.Helper()
		targets, unreadable, err := BuildManifestCaptureTargets(
			context.Background(),
			dyn,
			discoveryFromEntries(entries, nil),
			"ns1",
			nil,
			selector,
		)
		if err != nil {
			t.Fatalf("BuildManifestCaptureTargets: %v", err)
		}
		if len(unreadable) != 0 {
			t.Fatalf("expected no unreadable GVRs, got %#v", unreadable)
		}
		return targetNames(targets)
	}

	t.Run("nil selector captures everything", func(t *testing.T) {
		got := build(nil)
		for _, k := range []string{"ConfigMap/cm-keep", "ConfigMap/cm-drop", "ConfigMap/cm-nolabel"} {
			if _, ok := got[k]; !ok {
				t.Errorf("nil selector must capture %q, targets=%v", k, got)
			}
		}
	})

	t.Run("Everything selector captures everything", func(t *testing.T) {
		got := build(labels.Everything())
		for _, k := range []string{"ConfigMap/cm-keep", "ConfigMap/cm-drop", "ConfigMap/cm-nolabel"} {
			if _, ok := got[k]; !ok {
				t.Errorf("Everything selector must capture %q, targets=%v", k, got)
			}
		}
	})

	t.Run("matchLabels include keeps only matching", func(t *testing.T) {
		sel, err := labels.Parse("group=keep")
		if err != nil {
			t.Fatalf("labels.Parse: %v", err)
		}
		got := build(sel)
		if _, ok := got["ConfigMap/cm-keep"]; !ok {
			t.Error("include selector must keep cm-keep")
		}
		for _, k := range []string{"ConfigMap/cm-drop", "ConfigMap/cm-nolabel"} {
			if _, ok := got[k]; ok {
				t.Errorf("include selector must drop %q, targets=%v", k, got)
			}
		}
	})

	t.Run("NotIn exclude drops only matching, keeps unlabeled", func(t *testing.T) {
		sel, err := labels.Parse("group notin (drop)")
		if err != nil {
			t.Fatalf("labels.Parse: %v", err)
		}
		got := build(sel)
		if _, ok := got["ConfigMap/cm-drop"]; ok {
			t.Error("NotIn (drop) must exclude cm-drop")
		}
		// Objects without the key pass a NotIn selector.
		for _, k := range []string{"ConfigMap/cm-keep", "ConfigMap/cm-nolabel"} {
			if _, ok := got[k]; !ok {
				t.Errorf("NotIn (drop) must keep %q (no/other group), targets=%v", k, got)
			}
		}
	})
}
