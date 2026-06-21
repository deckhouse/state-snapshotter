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

package restore

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func dataSourceRefName(pvc unstructured.Unstructured) string {
	name, _, _ := unstructured.NestedString(pvc.Object, "spec", "dataSourceRef", "name")
	return name
}

func TestTransformNode_OrphanPVCGetsDataSourceRef(t *testing.T) {
	pvc := pvcManifest("data-pvc", "source-ns", "uid-a")
	node := &RestoreNode{
		SnapshotRef:  snapshot.ObjectRef{Kind: "Snapshot", Name: "snap", Namespace: "source-ns"},
		DataBindings: []snapshot.DataBindingRef{dataBindingRef("uid-a", "data-pvc", "vsc-a")},
		VSCToVS:      map[string]string{"vsc-a": "vs-a"},
	}

	out, err := transformNodeObjects(node, []unstructured.Unstructured{pvc}, "restore-ns")
	if err != nil {
		t.Fatalf("transformNodeObjects: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 object, got %d", len(out))
	}
	if out[0].GetNamespace() != "restore-ns" {
		t.Fatalf("namespace = %q, want restore-ns", out[0].GetNamespace())
	}
	if got := dataSourceRefName(out[0]); got != "vs-a" {
		t.Fatalf("dataSourceRef.name = %q, want vs-a", got)
	}
	apiGroup, _, _ := unstructured.NestedString(out[0].Object, "spec", "dataSourceRef", "apiGroup")
	if apiGroup != csiSnapshotAPIGroup {
		t.Fatalf("dataSourceRef.apiGroup = %q, want %q", apiGroup, csiSnapshotAPIGroup)
	}
}

func TestTransformNode_OrphanPVCMissingVSLeafFailsClosed(t *testing.T) {
	pvc := pvcManifest("data-pvc", "source-ns", "uid-a")
	node := &RestoreNode{
		SnapshotRef:  snapshot.ObjectRef{Kind: "Snapshot", Name: "snap", Namespace: "source-ns"},
		DataBindings: []snapshot.DataBindingRef{dataBindingRef("uid-a", "data-pvc", "vsc-a")},
		VSCToVS:      map[string]string{}, // VS leaf not resolved
	}
	_, err := transformNodeObjects(node, []unstructured.Unstructured{pvc}, "restore-ns")
	if err == nil {
		t.Fatal("expected contract violation when VS leaf is missing")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}

func TestTransformNode_GenericPassThrough(t *testing.T) {
	cm := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": "cfg", "namespace": "source-ns"},
		"data":       map[string]interface{}{"k": "v"},
	}}
	node := &RestoreNode{SnapshotRef: snapshot.ObjectRef{Kind: "Snapshot", Name: "snap", Namespace: "source-ns"}}

	out, err := transformNodeObjects(node, []unstructured.Unstructured{cm}, "restore-ns")
	if err != nil {
		t.Fatalf("transformNodeObjects: %v", err)
	}
	if len(out) != 1 || out[0].GetKind() != "ConfigMap" {
		t.Fatalf("expected ConfigMap pass-through, got %#v", out)
	}
	if out[0].GetNamespace() != "restore-ns" {
		t.Fatalf("namespace = %q, want restore-ns", out[0].GetNamespace())
	}
}

func TestTransformNode_PVCWithoutDataRefFailsClosed(t *testing.T) {
	pvc := pvcManifest("plain-pvc", "source-ns", "uid-plain")
	node := &RestoreNode{SnapshotRef: snapshot.ObjectRef{Kind: "Snapshot", Name: "snap", Namespace: "source-ns"}, VSCToVS: map[string]string{}}

	_, err := transformNodeObjects(node, []unstructured.Unstructured{pvc}, "restore-ns")
	if err == nil {
		t.Fatal("expected error: a PVC without a data binding must not be emitted data-less")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}

func TestTransformNode_OrphanPVCNonVSCArtifactFailsClosed(t *testing.T) {
	pvc := pvcManifest("data-pvc", "source-ns", "uid-a")
	binding := dataBindingRef("uid-a", "data-pvc", "artifact-x")
	binding.Artifact.Kind = "ConfigMap" // not a VolumeSnapshotContent
	node := &RestoreNode{
		SnapshotRef:  snapshot.ObjectRef{Kind: "Snapshot", Name: "snap", Namespace: "source-ns"},
		DataBindings: []snapshot.DataBindingRef{binding},
		VSCToVS:      map[string]string{},
	}

	_, err := transformNodeObjects(node, []unstructured.Unstructured{pvc}, "restore-ns")
	if err == nil {
		t.Fatal("expected contract violation when orphan-PVC artifact is not a VolumeSnapshotContent")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}
