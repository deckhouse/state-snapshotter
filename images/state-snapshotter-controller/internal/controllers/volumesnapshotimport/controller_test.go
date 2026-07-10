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

package volumesnapshotimport

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// importSnapshotSourceRef must target the orphan PVC (not the VolumeSnapshot handle): it is published as
// status.sourceRef and the aggregator builds the dataRef source from it. The restore compiler matches
// a captured PVC manifest to its dataRef by PVC identity/UID — a VolumeSnapshot-targeted source would never
// match and the PVC would be emitted data-less (contract violation).
func TestImportSnapshotSourceRef_TargetsPVC(t *testing.T) {
	pvc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":      "bk-pvc",
			"namespace": "source-ns",
			"uid":       "pvc-uid-123",
		},
	}}

	src := importSnapshotSourceRef(pvc)

	if string(src.UID) != "pvc-uid-123" {
		t.Fatalf("Source.UID must be the PVC uid, got %q", src.UID)
	}
	if src.Kind != "PersistentVolumeClaim" {
		t.Fatalf("Source.Kind must be PersistentVolumeClaim, got %q", src.Kind)
	}
	if src.APIVersion != "v1" {
		t.Fatalf("Source.APIVersion must be v1, got %q", src.APIVersion)
	}
	if src.Name != "bk-pvc" || src.Namespace != "source-ns" {
		t.Fatalf("Source identity mismatch: %s/%s", src.Namespace, src.Name)
	}
}

// isImportModeVolumeSnapshot keys solely on the unified enum spec.mode: Import (parity with every other
// snapshot kind); capture/pre-provisioned VS (mode absent or Capture) are not ours to bind.
func TestIsImportModeVolumeSnapshot(t *testing.T) {
	cases := []struct {
		name   string
		mode   string
		source map[string]interface{}
		want   bool
	}{
		{name: "mode Import (empty source)", mode: "Import", source: map[string]interface{}{}, want: true},
		{name: "mode Capture (persistentVolumeClaimName)", mode: "Capture", source: map[string]interface{}{"persistentVolumeClaimName": "pvc-1"}, want: false},
		{name: "mode absent (CRD default Capture)", source: map[string]interface{}{"persistentVolumeClaimName": "pvc-1"}, want: false},
		{name: "pre-provisioned (volumeSnapshotContentName)", mode: "Capture", source: map[string]interface{}{"volumeSnapshotContentName": "vsc-1"}, want: false},
		{name: "no source, no mode", source: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := map[string]interface{}{}
			if tc.mode != "" {
				spec["mode"] = tc.mode
			}
			if tc.source != nil {
				spec["source"] = tc.source
			}
			vs := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
			if got := isImportModeVolumeSnapshot(vs); got != tc.want {
				t.Fatalf("isImportModeVolumeSnapshot = %v, want %v", got, tc.want)
			}
		})
	}
}

// resolveDataImportArtifact distinguishes ready (VSC produced), pending (no artifact yet), and terminal
// (a non-VSC artifact the extended-VS legacy binding cannot represent).
func TestResolveDataImportArtifact(t *testing.T) {
	newDI := func(kind, name string) *unstructured.Unstructured {
		ref := map[string]interface{}{}
		if kind != "" {
			ref["kind"] = kind
		}
		if name != "" {
			ref["name"] = name
		}
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "di-1", "namespace": "ns1"},
			"status":   map[string]interface{}{"data": map[string]interface{}{"artifact": ref}},
		}}
	}
	r := &Controller{}

	t.Run("ready VSC artifact", func(t *testing.T) {
		vscName, ready, terminal := r.resolveDataImportArtifact(newDI(snapshotpkg.KindVolumeSnapshotContent, "vsc-7"))
		if !ready || vscName != "vsc-7" || terminal != "" {
			t.Fatalf("got vsc=%q ready=%v terminal=%q, want vsc-7/true/empty", vscName, ready, terminal)
		}
	})
	t.Run("pending (no artifact name)", func(t *testing.T) {
		vscName, ready, terminal := r.resolveDataImportArtifact(newDI(snapshotpkg.KindVolumeSnapshotContent, ""))
		if ready || vscName != "" || terminal != "" {
			t.Fatalf("got vsc=%q ready=%v terminal=%q, want empty/false/empty", vscName, ready, terminal)
		}
	})
	t.Run("terminal (non-VSC artifact)", func(t *testing.T) {
		vscName, ready, terminal := r.resolveDataImportArtifact(newDI("PersistentVolume", "pv-9"))
		if ready || vscName != "" || terminal == "" {
			t.Fatalf("got vsc=%q ready=%v terminal=%q, want empty/false/non-empty", vscName, ready, terminal)
		}
	})
}
