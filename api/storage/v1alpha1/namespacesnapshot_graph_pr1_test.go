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

package v1alpha1

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNamespaceSnapshotStatus_ChildrenSnapshotRefs_JSONRoundTrip(t *testing.T) {
	ns := NamespaceSnapshot{
		Status: NamespaceSnapshotStatus{
			BoundSnapshotContentName: "parent-content",
			ChildrenSnapshotRefs: []NamespaceSnapshotChildRef{
				{APIVersion: "storage.deckhouse.io/v1alpha1", Kind: "NamespaceSnapshot", Name: "child1"},
			},
		},
	}

	data, err := json.Marshal(&ns)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out NamespaceSnapshot
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := out.Status.ChildrenSnapshotRefs; len(got) != 1 {
		t.Fatalf("ChildrenSnapshotRefs len: got %d want 1 (full %#v)", len(got), got)
	}
	if got := out.Status.ChildrenSnapshotRefs[0]; got.Name != "child1" ||
		got.APIVersion != "storage.deckhouse.io/v1alpha1" || got.Kind != "NamespaceSnapshot" {
		t.Fatalf("ChildrenSnapshotRefs[0]: got %#v", got)
	}

	if !reflect.DeepEqual(out.Status.ChildrenSnapshotRefs, ns.Status.ChildrenSnapshotRefs) {
		t.Fatalf("childrenSnapshotRefs mismatch: got %#v want %#v", out.Status.ChildrenSnapshotRefs, ns.Status.ChildrenSnapshotRefs)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	status := raw["status"].(map[string]interface{})
	refs := status["childrenSnapshotRefs"].([]interface{})
	if len(refs) != 1 {
		t.Fatalf("raw childrenSnapshotRefs len: got %d want 1", len(refs))
	}
	item := refs[0].(map[string]interface{})
	if item["name"] != "child1" ||
		item["apiVersion"] != "storage.deckhouse.io/v1alpha1" || item["kind"] != "NamespaceSnapshot" {
		t.Fatalf("expected JSON keys apiVersion/kind/name, got %#v", item)
	}
	if _, ok := item["namespace"]; ok {
		t.Fatalf("did not expect namespace key in child ref JSON: %#v", item)
	}
}

func TestSnapshotContentStatus_TargetGraphFields_JSONRoundTrip(t *testing.T) {
	sc := SnapshotContent{
		Status: SnapshotContentStatus{
			ManifestCheckpointName: "mcp-common",
			ChildrenSnapshotContentRefs: []SnapshotContentChildRef{
				{Name: "child-content-1"},
			},
			DataRef: &SnapshotDataRef{
				APIVersion: "snapshot.storage.k8s.io/v1",
				Kind:       "VolumeSnapshotContent",
				Name:       "vsc-1",
			},
		},
	}

	data, err := json.Marshal(&sc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out SnapshotContent
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := out.Status.ManifestCheckpointName; got != "mcp-common" {
		t.Fatalf("ManifestCheckpointName: got %q want mcp-common", got)
	}
	if got := out.Status.ChildrenSnapshotContentRefs; len(got) != 1 || got[0].Name != "child-content-1" {
		t.Fatalf("ChildrenSnapshotContentRefs: got %#v", got)
	}
	if out.Status.DataRef == nil || out.Status.DataRef.APIVersion != "snapshot.storage.k8s.io/v1" ||
		out.Status.DataRef.Kind != "VolumeSnapshotContent" ||
		out.Status.DataRef.Name != "vsc-1" {
		t.Fatalf("DataRef: got %#v", out.Status.DataRef)
	}
	if out.Status.DataRef.Kind == "VolumeCaptureRequest" {
		t.Fatalf("DataRef must reference a durable artifact, not an execution request: %#v", out.Status.DataRef)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	status := raw["status"].(map[string]interface{})
	if _, ok := status["manifestCheckpointName"]; !ok {
		t.Fatal("expected manifestCheckpointName JSON field")
	}
	refs := status["childrenSnapshotContentRefs"].([]interface{})
	item := refs[0].(map[string]interface{})
	if item["name"] != "child-content-1" {
		t.Fatalf("expected name-only child content ref, got %#v", item)
	}
	if _, ok := item["namespace"]; ok {
		t.Fatalf("did not expect namespace key in child content ref JSON: %#v", item)
	}
	dataRef := status["dataRef"].(map[string]interface{})
	if dataRef["apiVersion"] != "snapshot.storage.k8s.io/v1" ||
		dataRef["kind"] != "VolumeSnapshotContent" ||
		dataRef["name"] != "vsc-1" {
		t.Fatalf("expected dataRef apiVersion/kind/name artifact ref, got %#v", dataRef)
	}
	if _, ok := dataRef["namespace"]; ok {
		t.Fatalf("did not expect namespace key in cluster artifact dataRef JSON: %#v", dataRef)
	}
}

func TestNamespaceSnapshotStatus_ChildrenSnapshotRefs_OmittedWhenEmpty(t *testing.T) {
	ns := NamespaceSnapshot{
		Status: NamespaceSnapshotStatus{
			BoundSnapshotContentName: "x",
		},
	}
	if ns.Status.ChildrenSnapshotRefs != nil {
		t.Fatalf("expected nil slice by default, got %#v", ns.Status.ChildrenSnapshotRefs)
	}

	data, err := json.Marshal(ns.Status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["childrenSnapshotRefs"]; ok {
		t.Fatal("childrenSnapshotRefs must be omitted when empty")
	}
}

func TestSnapshotContentStatus_ChildrenSnapshotContentRefs_OmittedWhenEmpty(t *testing.T) {
	sc := SnapshotContent{
		Status: SnapshotContentStatus{
			ManifestCheckpointName: "mcp-only",
		},
	}
	if sc.Status.ChildrenSnapshotContentRefs != nil {
		t.Fatalf("expected nil slice by default, got %#v", sc.Status.ChildrenSnapshotContentRefs)
	}

	data, err := json.Marshal(sc.Status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["childrenSnapshotContentRefs"]; ok {
		t.Fatal("childrenSnapshotContentRefs must be omitted when empty")
	}
}
