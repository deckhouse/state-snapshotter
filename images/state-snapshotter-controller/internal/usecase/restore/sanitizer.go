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
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Read-path restore-safe sanitizer (ADR 2026-06-10 D3).
//
// The restore compiler emits apply-ready manifests, so it MUST strip runtime/server-managed
// fields that block `kubectl apply` and rewrite the namespace to the restore target. Sanitization
// happens on the read path (independent of capture-time EnableFiltering).

// controlPlaneExactKinds are control-plane / snapshot-machinery kinds that MUST NOT appear in
// restore output. VS/VSC and snapshot tree nodes are transferred separately (data + tree); the
// compiler only references them, never emits them as apply manifests (ADR 2026-06-10 D6/INV-RC11).
var controlPlaneExactKinds = map[string]struct{}{
	"Snapshot":                       {},
	"SnapshotContent":                {},
	"ManifestCheckpoint":             {},
	"ManifestCheckpointContentChunk": {},
	"ManifestCaptureRequest":         {},
	"VolumeCaptureRequest":           {},
	"VolumeRestoreRequest":           {},
	"VolumeSnapshot":                 {},
	"VolumeSnapshotContent":          {},
}

// isControlPlaneKind reports whether a kind is snapshot-machinery / control-plane and therefore
// excluded from restore output. Any *Snapshot / *SnapshotContent kind (domain snapshot CRs) is
// excluded too: in this system those are snapshot tree nodes, not restorable application objects.
func isControlPlaneKind(kind string) bool {
	if _, ok := controlPlaneExactKinds[kind]; ok {
		return true
	}
	return strings.HasSuffix(kind, "Snapshot") || strings.HasSuffix(kind, "SnapshotContent")
}

// sanitizeForRestore returns a restore-safe copy of obj and whether it should be emitted.
//
// keep=false when the object must be dropped from restore output:
//   - cluster-scoped objects (no namespace) — namespace restore compiler is namespaced-only (MVP);
//   - control-plane / snapshot-machinery kinds (see isControlPlaneKind).
//
// For kept (namespaced) objects it removes runtime metadata, status, kind-specific server fields,
// and rewrites metadata.namespace to targetNamespace.
func sanitizeForRestore(obj unstructured.Unstructured, targetNamespace string) (unstructured.Unstructured, bool) {
	if isControlPlaneKind(obj.GetKind()) {
		return unstructured.Unstructured{}, false
	}
	// Cluster-scoped objects carry no namespace in the captured manifest (MCP keeps the original
	// namespace for namespaced objects). MVP: drop them, like /manifests.
	if obj.GetNamespace() == "" {
		return unstructured.Unstructured{}, false
	}

	out := obj.DeepCopy()
	stripRuntimeMetadata(out)
	unstructured.RemoveNestedField(out.Object, "status")
	out.SetNamespace(targetNamespace)
	stripKindSpecificFields(out)
	return *out, true
}

func stripRuntimeMetadata(out *unstructured.Unstructured) {
	for _, f := range []string{
		"uid",
		"resourceVersion",
		"generation",
		"creationTimestamp",
		"deletionTimestamp",
		"deletionGracePeriodSeconds",
		"managedFields",
		"ownerReferences",
		"finalizers",
		"selfLink",
	} {
		unstructured.RemoveNestedField(out.Object, "metadata", f)
	}
}

func stripKindSpecificFields(out *unstructured.Unstructured) {
	switch out.GetKind() {
	case pvcKind:
		if out.GetAPIVersion() == "v1" {
			unstructured.RemoveNestedField(out.Object, "spec", "volumeName")
			// dataSource/dataSourceRef are re-set by the orphan-PVC restore transform; drop any
			// captured value first so a stale binding cannot leak into the output.
			unstructured.RemoveNestedField(out.Object, "spec", "dataSource")
			unstructured.RemoveNestedField(out.Object, "spec", "dataSourceRef")
		}
	case "Service":
		if out.GetAPIVersion() == "v1" {
			for _, f := range []string{"clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy", "healthCheckNodePort"} {
				unstructured.RemoveNestedField(out.Object, "spec", f)
			}
		}
	}
}
