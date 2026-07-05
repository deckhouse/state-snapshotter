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

package genericbinder

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func importLeafObject() *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "demo.state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "DemoVirtualDiskSnapshot",
		"metadata": map[string]interface{}{
			"name":      "disk-snap",
			"namespace": "project-a",
			"uid":       "leaf-uid-1",
		},
	}}
	return o
}

func dataImportWithArtifact(apiVersion, kind, name string) *unstructured.Unstructured {
	di := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "DataImport",
		"metadata": map[string]interface{}{
			"name":      "di-1",
			"namespace": "project-a",
		},
	}}
	if name != "" || kind != "" || apiVersion != "" {
		ref := map[string]interface{}{}
		if apiVersion != "" {
			ref["apiVersion"] = apiVersion
		}
		if kind != "" {
			ref["kind"] = kind
		}
		if name != "" {
			ref["name"] = name
		}
		_ = unstructured.SetNestedMap(di.Object, ref, "status", "data", "artifact")
	}
	return di
}

// A produced VolumeSnapshotContent artifact yields a ready binding carrying the leaf identity as target
// and the VSC as the data artifact (size etc. are enriched downstream from VSC.status.restoreSize).
func TestBuildImportDataBinding_VSCReady(t *testing.T) {
	di := dataImportWithArtifact("snapshot.storage.k8s.io/v1", "VolumeSnapshotContent", "snapcontent-abc")
	// DataImport republishes the original captured volume mode into status.volumeMode; the binding must
	// carry it because the leaf-targeted dataRef cannot be enriched from a live PVC and downstream restore
	// fails closed on an empty volumeMode.
	_ = unstructured.SetNestedField(di.Object, "Block", "status", "volumeMode")
	// DataImport fills the durable artifact uid best-effort (from the VCR artifact uid); it must flow
	// through into the published dataRef.artifact.uid.
	_ = unstructured.SetNestedField(di.Object, "8d7c6b5a-4e3f-4a2b-9c1d-0f1e2d3c4b5a", "status", "data", "artifact", "uid")
	leaf := importLeafObject()

	binding, ready, reason, _ := buildImportDataBinding(di, leaf)
	if reason != "" {
		t.Fatalf("unexpected terminal reason: %q", reason)
	}
	if !ready || binding == nil {
		t.Fatalf("expected ready binding, got ready=%v binding=%v", ready, binding)
	}
	if binding.Artifact.Kind != snapshot.KindVolumeSnapshotContent || binding.Artifact.Name != "snapcontent-abc" {
		t.Fatalf("unexpected artifact: %#v", binding.Artifact)
	}
	if binding.Artifact.APIVersion != "snapshot.storage.k8s.io/v1" {
		t.Fatalf("unexpected artifact apiVersion: %q", binding.Artifact.APIVersion)
	}
	if binding.Artifact.UID != "8d7c6b5a-4e3f-4a2b-9c1d-0f1e2d3c4b5a" {
		t.Fatalf("expected artifact uid propagated from DataImport.status.data.artifact.uid, got %q", binding.Artifact.UID)
	}
	if string(binding.Source.UID) != "leaf-uid-1" {
		t.Fatalf("expected Source.UID from leaf UID, got %q", binding.Source.UID)
	}
	if binding.Source.Kind != "DemoVirtualDiskSnapshot" || binding.Source.Name != "disk-snap" || binding.Source.Namespace != "project-a" {
		t.Fatalf("unexpected source: %#v", binding.Source)
	}
	if binding.VolumeMode != "Block" {
		t.Fatalf("expected volumeMode propagated from DataImport.status.volumeMode, got %q", binding.VolumeMode)
	}
}

// Before the DataImport produces its artifact (no status.data.artifact), the binding is pending
// (not terminal) so the binder keeps polling rather than failing the import.
func TestBuildImportDataBinding_PendingWhenArtifactAbsent(t *testing.T) {
	di := dataImportWithArtifact("", "", "")
	binding, ready, reason, _ := buildImportDataBinding(di, importLeafObject())
	if ready || binding != nil || reason != "" {
		t.Fatalf("expected pending (no binding, no terminal), got ready=%v binding=%v reason=%q", ready, binding, reason)
	}
}

// A partially-written status.data.artifact (missing name) is still treated as not-yet-produced (pending).
func TestBuildImportDataBinding_PendingWhenArtifactPartial(t *testing.T) {
	di := dataImportWithArtifact("snapshot.storage.k8s.io/v1", "VolumeSnapshotContent", "")
	binding, ready, reason, _ := buildImportDataBinding(di, importLeafObject())
	if ready || binding != nil || reason != "" {
		t.Fatalf("expected pending for partial artifactRef, got ready=%v binding=%v reason=%q", ready, binding, reason)
	}
}

// A non-VSC artifact (e.g. PersistentVolume / Detach mode) is a terminal fault for the current import
// dataRef path (VSC-only); it must fail loud, not silently publish an unreadable dataRef.
func TestBuildImportDataBinding_TerminalForNonVSC(t *testing.T) {
	di := dataImportWithArtifact("v1", "PersistentVolume", "pv-xyz")
	binding, ready, reason, msg := buildImportDataBinding(di, importLeafObject())
	if ready || binding != nil {
		t.Fatalf("expected no binding for non-VSC artifact, got ready=%v binding=%v", ready, binding)
	}
	if reason != snapshot.ReasonDataArtifactInvalid {
		t.Fatalf("expected terminal reason %q, got %q (msg=%q)", snapshot.ReasonDataArtifactInvalid, reason, msg)
	}
}
