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

// importDataBinding must target the orphan PVC (not the VolumeSnapshot handle): the restore compiler
// matches a captured PVC manifest to its dataRef by PVC identity/UID. A VolumeSnapshot-targeted dataRef
// would never match and the PVC would be emitted data-less (contract violation).
func TestImportDataBinding_TargetsPVC(t *testing.T) {
	pvc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":      "bk-pvc",
			"namespace": "source-ns",
			"uid":       "pvc-uid-123",
		},
	}}

	b := importDataBinding(pvc, "vsc-artifact")

	if string(b.Source.UID) != "pvc-uid-123" {
		t.Fatalf("Source.UID must be the PVC uid, got %q", b.Source.UID)
	}
	if b.Source.Kind != "PersistentVolumeClaim" {
		t.Fatalf("Source.Kind must be PersistentVolumeClaim, got %q", b.Source.Kind)
	}
	if b.Source.APIVersion != "v1" {
		t.Fatalf("Source.APIVersion must be v1, got %q", b.Source.APIVersion)
	}
	if b.Source.Name != "bk-pvc" || b.Source.Namespace != "source-ns" {
		t.Fatalf("Source identity mismatch: %s/%s", b.Source.Namespace, b.Source.Name)
	}
	if string(b.Source.UID) != "pvc-uid-123" {
		t.Fatalf("Source.UID must be the PVC uid, got %q", b.Source.UID)
	}
	if b.Artifact.Kind != snapshotpkg.KindVolumeSnapshotContent || b.Artifact.Name != "vsc-artifact" {
		t.Fatalf("Artifact must point at the durable VSC, got %s/%s", b.Artifact.Kind, b.Artifact.Name)
	}
}

// isImportModeVolumeSnapshot keys solely on the unified empty marker spec.source.import: {} (parity with
// every other snapshot kind); capture/pre-provisioned VS (other source fields) are not ours to bind.
func TestIsImportModeVolumeSnapshot(t *testing.T) {
	cases := []struct {
		name   string
		source map[string]interface{}
		want   bool
	}{
		{name: "import marker present", source: map[string]interface{}{"import": map[string]interface{}{}}, want: true},
		{name: "capture (persistentVolumeClaimName)", source: map[string]interface{}{"persistentVolumeClaimName": "pvc-1"}, want: false},
		{name: "pre-provisioned (volumeSnapshotContentName)", source: map[string]interface{}{"volumeSnapshotContentName": "vsc-1"}, want: false},
		{name: "no source", source: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := map[string]interface{}{}
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
