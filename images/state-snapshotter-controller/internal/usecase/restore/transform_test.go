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

// The shared compilation rules (sanitize, orphan-PVC binding, the artifact-kind and missing-leaf
// refusals) are tested next to the code that implements them. What is core's own and tested here is which
// inputs core hands it — specifically that core claims no domain-covered PVCs.

func pvcManifest(name, namespace, uid string) unstructured.Unstructured {
	meta := map[string]interface{}{"name": name}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	if uid != "" {
		meta["uid"] = uid
	}
	return unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   meta,
	}}
}

// TestTransformNode_PVCWithoutDataRefFailsClosed guards the core-side invariant of restore compilation: a
// node compiled by core is never a domain node, so nothing in it recreates a PVC on its own, and a PVC
// with no data binding is a broken snapshot rather than a stateless volume. Emitting it would apply
// cleanly and provision an EMPTY volume — data loss discovered only when someone reads the data back — so
// the whole request is refused instead.
//
// The refusal lives in the shared compilation, but whether core reaches it is core's decision: passing a
// covered-PVC set here would suppress it. That is the mutation this test is written for.
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

// TestTransformNode_PVCDataBindingsReachCompilation proves the other half of the same wiring: core does
// pass the node's own dataRefs on, so a PVC that IS backed by a VolumeSnapshot is emitted bound rather
// than refused. Without this the test above would also pass on a core that never passes any binding at
// all — and every restore with a volume would fail closed.
func TestTransformNode_PVCDataBindingsReachCompilation(t *testing.T) {
	pvc := pvcManifest("data-pvc", "source-ns", "uid-a")
	node := &RestoreNode{
		SnapshotRef: snapshot.ObjectRef{Kind: "Snapshot", Name: "snap", Namespace: "source-ns"},
		DataBindings: []snapshot.DataBindingRef{{
			TargetUID: "uid-a",
			Target:    snapshot.ObjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: "source-ns", Name: "data-pvc", UID: "uid-a"},
			Artifact:  snapshot.ObjectRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-a"},
		}},
		VSCToVS: map[string]string{"vsc-a": "vs-a"},
	}

	out, err := transformNodeObjects(node, []unstructured.Unstructured{pvc}, "restore-ns")
	if err != nil {
		t.Fatalf("transformNodeObjects: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 object, got %d", len(out))
	}
	name, _, _ := unstructured.NestedString(out[0].Object, "spec", "dataSourceRef", "name")
	if name != "vs-a" {
		t.Fatalf("dataSourceRef.name = %q, want vs-a", name)
	}
}
