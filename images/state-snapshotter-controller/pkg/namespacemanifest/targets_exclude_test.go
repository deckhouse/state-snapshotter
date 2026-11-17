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

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// withLabels sets metadata.labels on a fixture object.
func withLabels(l map[string]interface{}) func(map[string]interface{}) {
	return func(o map[string]interface{}) {
		o["metadata"].(map[string]interface{})["labels"] = l
	}
}

// TestBuildManifestCaptureTargets_ExcludeLabel pins the exclude veto of the manifest sweep: an object that
// the built-in rules would capture but that carries the exclude label (any value) is not a target and is
// returned as excluded, so the caller can record it; an object the built-in rules drop anyway is in neither
// list, even when labelled; unlabelled objects are unaffected.
func TestBuildManifestCaptureTargets_ExcludeLabel(t *testing.T) {
	entries := defaultGVRs()
	veto := map[string]interface{}{storagev1alpha1.ExcludeLabelKey: "true"}
	vetoEmpty := map[string]interface{}{storagev1alpha1.ExcludeLabelKey: ""}
	dyn := dynamicFromEntries(entries,
		obj("v1", "ConfigMap", "cm-plain", nil),
		obj("v1", "ConfigMap", "cm-other-label", withLabels(map[string]interface{}{"group": "keep"})),
		obj("v1", "ConfigMap", "cm-vetoed", withLabels(veto)),
		obj("v1", "Secret", "secret-vetoed-empty", withLabels(vetoEmpty)),
		// Dropped by the built-in rules regardless of the label: controller-owned, control-plane noise and
		// Deckhouse-managed. None of them is a user exclusion.
		obj("v1", "Pod", "pod-owned-vetoed", func(o map[string]interface{}) {
			withLabels(veto)(o)
			o["metadata"].(map[string]interface{})["ownerReferences"] = controllerOwnerRef()
		}),
		obj("v1", "ConfigMap", "kube-root-ca.crt", withLabels(veto)),
		obj("rbac.authorization.k8s.io/v1", "RoleBinding", "rb-heritage-vetoed", withLabels(map[string]interface{}{
			storagev1alpha1.ExcludeLabelKey: "true",
			"heritage":                      "deckhouse",
		})),
	)

	targets, excluded, unreadable, err := BuildManifestCaptureTargets(
		context.Background(),
		dyn,
		discoveryFromEntries(entries, nil),
		"ns1",
		nil,
	)
	if err != nil {
		t.Fatalf("BuildManifestCaptureTargets: %v", err)
	}
	if len(unreadable) != 0 {
		t.Fatalf("expected no unreadable GVRs, got %#v", unreadable)
	}

	gotTargets := targetNames(targets)
	for _, k := range []string{"ConfigMap/cm-plain", "ConfigMap/cm-other-label"} {
		if _, ok := gotTargets[k]; !ok {
			t.Errorf("unlabelled %q must stay a target, targets=%v", k, gotTargets)
		}
	}
	for _, k := range []string{"ConfigMap/cm-vetoed", "Secret/secret-vetoed-empty", "Pod/pod-owned-vetoed", "ConfigMap/kube-root-ca.crt", "RoleBinding/rb-heritage-vetoed"} {
		if _, ok := gotTargets[k]; ok {
			t.Errorf("%q must not be a target, targets=%v", k, gotTargets)
		}
	}

	// Exactly the two user exclusions, sorted by (apiVersion, kind, name), with their full identity.
	want := []ManifestTarget{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-vetoed"},
		{APIVersion: "v1", Kind: "Secret", Name: "secret-vetoed-empty"},
	}
	if len(excluded) != len(want) {
		t.Fatalf("excluded = %#v, want %#v", excluded, want)
	}
	for i := range want {
		if excluded[i] != want[i] {
			t.Fatalf("excluded = %#v, want %#v", excluded, want)
		}
	}
}

// TestBuildManifestCaptureTargets_NoExcludeLabelNoExcluded pins that a namespace without labelled objects
// returns no excluded entries at all.
func TestBuildManifestCaptureTargets_NoExcludeLabelNoExcluded(t *testing.T) {
	entries := defaultGVRs()
	dyn := dynamicFromEntries(entries, obj("v1", "ConfigMap", "cm-plain", nil))
	targets, excluded, _, err := BuildManifestCaptureTargets(context.Background(), dyn, discoveryFromEntries(entries, nil), "ns1", nil)
	if err != nil {
		t.Fatalf("BuildManifestCaptureTargets: %v", err)
	}
	if len(excluded) != 0 {
		t.Fatalf("excluded = %#v, want none", excluded)
	}
	if _, ok := targetNames(targets)["ConfigMap/cm-plain"]; !ok {
		t.Fatalf("cm-plain must be a target, targets=%v", targetNames(targets))
	}
}
