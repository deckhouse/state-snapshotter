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

	if b.TargetUID != "pvc-uid-123" {
		t.Fatalf("TargetUID must be the PVC uid, got %q", b.TargetUID)
	}
	if b.Target.Kind != "PersistentVolumeClaim" {
		t.Fatalf("Target.Kind must be PersistentVolumeClaim, got %q", b.Target.Kind)
	}
	if b.Target.APIVersion != "v1" {
		t.Fatalf("Target.APIVersion must be v1, got %q", b.Target.APIVersion)
	}
	if b.Target.Name != "bk-pvc" || b.Target.Namespace != "source-ns" {
		t.Fatalf("Target identity mismatch: %s/%s", b.Target.Namespace, b.Target.Name)
	}
	if string(b.Target.UID) != "pvc-uid-123" {
		t.Fatalf("Target.UID must be the PVC uid, got %q", b.Target.UID)
	}
	if b.Artifact.Kind != snapshotpkg.KindVolumeSnapshotContent || b.Artifact.Name != "vsc-artifact" {
		t.Fatalf("Artifact must point at the durable VSC, got %s/%s", b.Artifact.Kind, b.Artifact.Name)
	}
}
