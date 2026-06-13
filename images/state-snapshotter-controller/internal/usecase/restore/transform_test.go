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

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func dataSourceRefName(pvc unstructured.Unstructured) string {
	name, _, _ := unstructured.NestedString(pvc.Object, "spec", "dataSourceRef", "name")
	return name
}

// stubTransformer is a domain transformer test double without any real domain kind. It covers the
// named PVCs and, when handleKind is set, claims (handled=true) every object of that kind, recording
// the children it was handed so tests can assert bottom-up threading.
type stubTransformer struct {
	covered      map[string]struct{}
	handleKind   string
	gotChildren  *[]NodeResult
	gotChildKind *string
}

func (s stubTransformer) CoveredPVCNames(_ *RestoreNode, _ []unstructured.Unstructured) map[string]struct{} {
	return s.covered
}

func (s stubTransformer) TransformObject(_ *RestoreNode, obj *unstructured.Unstructured, children []NodeResult) (bool, error) {
	if s.handleKind == "" || obj.GetKind() != s.handleKind {
		return false, nil
	}
	if s.gotChildren != nil {
		*s.gotChildren = children
	}
	if s.gotChildKind != nil && len(children) > 0 && len(children[0].Objects) > 0 {
		*s.gotChildKind = children[0].Objects[0].GetKind()
	}
	return true, nil
}

func TestTransformNode_OrphanPVCGetsDataSourceRef(t *testing.T) {
	pvc := pvcManifest("data-pvc", "source-ns", "uid-a")
	node := &RestoreNode{
		SnapshotRef:  snapshot.ObjectRef{Kind: "Snapshot", Name: "snap", Namespace: "source-ns"},
		DataBindings: []snapshot.DataBindingRef{dataBindingRef("uid-a", "data-pvc", "vsc-a")},
		VSCToVS:      map[string]string{"vsc-a": "vs-a"},
	}

	out, err := transformNodeObjects(node, []unstructured.Unstructured{pvc}, nil, nil, "restore-ns")
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
	_, err := transformNodeObjects(node, []unstructured.Unstructured{pvc}, nil, nil, "restore-ns")
	if err == nil {
		t.Fatal("expected contract violation when VS leaf is missing")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}

func TestTransformNode_CoveredPVCSuppressed(t *testing.T) {
	// A domain transformer covers "disk-pvc"; the node still carries a dataRef for it, but the PVC
	// must be suppressed and must NOT require a VolumeSnapshot leaf (covered PVCs are not orphans).
	pvc := pvcManifest("disk-pvc", "source-ns", "uid-disk")
	node := &RestoreNode{
		SnapshotRef:  snapshot.ObjectRef{Kind: "Snapshot", Name: "snap", Namespace: "source-ns"},
		DataBindings: []snapshot.DataBindingRef{dataBindingRef("uid-disk", "disk-pvc", "vsc-disk")},
		VSCToVS:      map[string]string{}, // intentionally empty: covered PVCs are skipped
	}
	transformers := []DomainRestoreTransformer{stubTransformer{covered: map[string]struct{}{"disk-pvc": {}}}}

	out, err := transformNodeObjects(node, []unstructured.Unstructured{pvc}, transformers, nil, "restore-ns")
	if err != nil {
		t.Fatalf("transformNodeObjects: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected covered PVC to be suppressed, got %d objects", len(out))
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

	out, err := transformNodeObjects(node, []unstructured.Unstructured{cm}, nil, nil, "restore-ns")
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

	_, err := transformNodeObjects(node, []unstructured.Unstructured{pvc}, nil, nil, "restore-ns")
	if err == nil {
		t.Fatal("expected error: a PVC without a data binding must not be emitted data-less")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}

func TestTransformNode_DomainTransformerReceivesChildren(t *testing.T) {
	vm := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "demo.deckhouse.io/v1alpha1",
		"kind":       "DemoVirtualMachine",
		"metadata":   map[string]interface{}{"name": "vm", "namespace": "source-ns"},
	}}
	restoredDisk := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "demo.deckhouse.io/v1alpha1",
		"kind":       "DemoVirtualDisk",
		"metadata":   map[string]interface{}{"name": "disk", "namespace": "restore-ns"},
	}}
	children := []NodeResult{{Objects: []unstructured.Unstructured{restoredDisk}}}

	var gotChildKind string
	transformers := []DomainRestoreTransformer{stubTransformer{handleKind: "DemoVirtualMachine", gotChildKind: &gotChildKind}}
	node := &RestoreNode{SnapshotRef: snapshot.ObjectRef{Kind: "Snapshot", Name: "snap", Namespace: "source-ns"}}

	_, err := transformNodeObjects(node, []unstructured.Unstructured{vm}, transformers, children, "restore-ns")
	if err != nil {
		t.Fatalf("transformNodeObjects: %v", err)
	}
	if gotChildKind != "DemoVirtualDisk" {
		t.Fatalf("domain transformer did not receive restored child; gotChildKind = %q", gotChildKind)
	}
}

func TestTransformNode_TwoTransformersHandleSameObjectFailsClosed(t *testing.T) {
	vm := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "demo.deckhouse.io/v1alpha1",
		"kind":       "DemoVirtualMachine",
		"metadata":   map[string]interface{}{"name": "vm", "namespace": "source-ns"},
	}}
	transformers := []DomainRestoreTransformer{
		stubTransformer{handleKind: "DemoVirtualMachine"},
		stubTransformer{handleKind: "DemoVirtualMachine"},
	}
	node := &RestoreNode{SnapshotRef: snapshot.ObjectRef{Kind: "Snapshot", Name: "snap", Namespace: "source-ns"}}

	_, err := transformNodeObjects(node, []unstructured.Unstructured{vm}, transformers, nil, "restore-ns")
	if err == nil {
		t.Fatal("expected contract violation when two transformers handle the same object")
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

	_, err := transformNodeObjects(node, []unstructured.Unstructured{pvc}, nil, nil, "restore-ns")
	if err == nil {
		t.Fatal("expected contract violation when orphan-PVC artifact is not a VolumeSnapshotContent")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}
