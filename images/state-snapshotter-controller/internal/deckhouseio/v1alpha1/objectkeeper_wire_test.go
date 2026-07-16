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
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// This is a DRIFT GUARD for the local mirror of the upstream deckhouse.io/v1alpha1
// ObjectKeeper API (see doc.go for source path + version). state-snapshotter reads and
// writes real ObjectKeeper CRs owned by deckhouse-controller, so the on-the-wire JSON
// shape MUST stay byte-identical to upstream. If someone changes a json tag or field
// here, this test fails loudly instead of silently breaking interop in a cluster.

// TestObjectKeeperWireShape pins the exact JSON keys/paths produced by the mirrored
// types. Keys are compared literally against the ObjectKeeper CRD schema
// (objectkeepers.deckhouse.io): spec.mode, spec.followObjectRef.{apiVersion,kind,
// namespace,name,uid}, spec.ttl.
func TestObjectKeeperWireShape(t *testing.T) {
	ttl := metav1.Duration{Duration: 90 * time.Minute}
	ok := ObjectKeeper{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "deckhouse.io/v1alpha1",
			Kind:       "ObjectKeeper",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "root-snapshot-keeper"},
		Spec: ObjectKeeperSpec{
			Mode: "FollowObjectWithTTL",
			FollowObjectRef: &FollowObjectRef{
				APIVersion: "state-snapshotter.deckhouse.io/v1alpha1",
				Kind:       "Snapshot",
				Namespace:  "d8-snapshots",
				Name:       "root",
				UID:        "11111111-2222-3333-4444-555555555555",
			},
			TTL: &ttl,
		},
		Status: ObjectKeeperStatus{
			Phase:   PhaseTracking,
			Message: "tracking",
		},
	}

	raw, err := json.Marshal(ok)
	if err != nil {
		t.Fatalf("marshal ObjectKeeper: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal into generic map: %v", err)
	}

	assertString(t, m, "apiVersion", "deckhouse.io/v1alpha1")
	assertString(t, m, "kind", "ObjectKeeper")

	spec := assertObject(t, m, "spec")
	assertString(t, spec, "mode", "FollowObjectWithTTL")
	// metav1.Duration serializes to a duration string; this pins ttl's wire type.
	assertString(t, spec, "ttl", "1h30m0s")

	ref := assertObject(t, spec, "followObjectRef")
	assertString(t, ref, "apiVersion", "state-snapshotter.deckhouse.io/v1alpha1")
	assertString(t, ref, "kind", "Snapshot")
	assertString(t, ref, "namespace", "d8-snapshots")
	assertString(t, ref, "name", "root")
	assertString(t, ref, "uid", "11111111-2222-3333-4444-555555555555")

	status := assertObject(t, m, "status")
	assertString(t, status, "phase", "Tracking")

	// Round-trip: the wire form must decode back into an equivalent value.
	var back ObjectKeeper
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if back.Spec.Mode != ok.Spec.Mode ||
		back.Spec.FollowObjectRef == nil ||
		back.Spec.FollowObjectRef.UID != ok.Spec.FollowObjectRef.UID ||
		back.Spec.TTL == nil ||
		back.Spec.TTL.Duration != ok.Spec.TTL.Duration {
		t.Fatalf("round-trip mismatch: got %+v", back)
	}
}

// TestObjectKeeperOmitEmpty pins the omitempty behavior that upstream relies on:
// followObjectRef and ttl disappear from the wire when unset, but mode is always
// present (no omitempty), matching the CRD's required spec.mode.
func TestObjectKeeperOmitEmpty(t *testing.T) {
	raw, err := json.Marshal(ObjectKeeper{Spec: ObjectKeeperSpec{Mode: "TTL"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	spec := assertObject(t, m, "spec")
	if _, ok := spec["mode"]; !ok {
		t.Errorf("spec.mode must always be present (no omitempty)")
	}
	if _, ok := spec["followObjectRef"]; ok {
		t.Errorf("spec.followObjectRef must be omitted when nil")
	}
	if _, ok := spec["ttl"]; ok {
		t.Errorf("spec.ttl must be omitted when nil")
	}
}

// TestAddToSchemeRegistersOnlyObjectKeeper verifies the mirror registers exactly
// ObjectKeeper and ObjectKeeperList under deckhouse.io/v1alpha1 — the whole reason the
// local copy exists (upstream addKnownTypes registers dozens of unrelated types).
func TestAddToSchemeRegistersOnlyObjectKeeper(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	gv := schema.GroupVersion{Group: "deckhouse.io", Version: "v1alpha1"}
	if SchemeGroupVersion != gv {
		t.Fatalf("SchemeGroupVersion = %v, want %v", SchemeGroupVersion, gv)
	}

	for _, kind := range []string{"ObjectKeeper", "ObjectKeeperList"} {
		if _, err := scheme.New(gv.WithKind(kind)); err != nil {
			t.Errorf("scheme missing %s: %v", kind, err)
		}
	}

	// Only the two ObjectKeeper kinds (plus the meta types AddToGroupVersion adds,
	// e.g. WatchEvent/List/APIGroup) should live in this group-version. Guard against
	// accidentally pulling in the upstream ModuleConfig/DeckhouseRelease/... set.
	forbidden := []string{"ModuleConfig", "Module", "ModuleSource", "DeckhouseRelease", "PackageRepository"}
	for _, kind := range forbidden {
		if _, err := scheme.New(gv.WithKind(kind)); err == nil {
			t.Errorf("scheme unexpectedly knows %s — mirror must register only ObjectKeeper types", kind)
		}
	}
}

func assertObject(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := parent[key]
	if !ok {
		t.Fatalf("missing key %q", key)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("key %q is %T, want object", key, v)
	}
	return obj
}

func assertString(t *testing.T, parent map[string]any, key, want string) {
	t.Helper()
	v, ok := parent[key]
	if !ok {
		t.Errorf("missing key %q", key)
		return
	}
	got, ok := v.(string)
	if !ok {
		t.Errorf("key %q is %T, want string", key, v)
		return
	}
	if got != want {
		t.Errorf("key %q = %q, want %q", key, got, want)
	}
}
