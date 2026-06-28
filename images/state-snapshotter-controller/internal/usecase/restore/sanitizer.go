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
// happens on the read path; capture stores raw manifests as-is.

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
	stripRestoreBreakingAnnotations(out)
	unstructured.RemoveNestedField(out.Object, "status")
	out.SetNamespace(targetNamespace)
	stripKindSpecificFields(out)
	rewriteInSpecNamespaces(out, targetNamespace)
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

// restoreBreakingAnnotations are server/scheduler-managed annotations that must not survive into
// restore output: kubectl's last-applied snapshot (stale, carries the source namespace) and the
// PVC bind/scheduler annotations that pin a PVC to a specific PV/node of the source cluster.
var restoreBreakingAnnotations = []string{
	"kubectl.kubernetes.io/last-applied-configuration",
	"pv.kubernetes.io/bind-completed",
	"pv.kubernetes.io/bound-by-controller",
	"volume.kubernetes.io/selected-node",
}

func stripRestoreBreakingAnnotations(out *unstructured.Unstructured) {
	anns, found, err := unstructured.NestedMap(out.Object, "metadata", "annotations")
	if err != nil || !found || len(anns) == 0 {
		return
	}
	for _, k := range restoreBreakingAnnotations {
		delete(anns, k)
	}
	if len(anns) == 0 {
		unstructured.RemoveNestedField(out.Object, "metadata", "annotations")
		return
	}
	_ = unstructured.SetNestedMap(out.Object, anns, "metadata", "annotations")
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
			// clusterIP(s)/ipFamilies/loadBalancerIP and per-port nodePort are allocated by the
			// target cluster; keeping them causes immutable-field / conflict errors on apply.
			for _, f := range []string{"clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy", "healthCheckNodePort", "loadBalancerIP"} {
				unstructured.RemoveNestedField(out.Object, "spec", f)
			}
			stripServiceNodePorts(out)
		}
	}
}

func stripServiceNodePorts(out *unstructured.Unstructured) {
	ports, found, err := unstructured.NestedSlice(out.Object, "spec", "ports")
	if err != nil || !found {
		return
	}
	for i := range ports {
		p, ok := ports[i].(map[string]interface{})
		if !ok {
			continue
		}
		delete(p, "nodePort")
		ports[i] = p
	}
	_ = unstructured.SetNestedSlice(out.Object, ports, "spec", "ports")
}

// rewriteInSpecNamespaces rewrites namespace references that live inside spec/subjects (not
// metadata.namespace, which is already rewritten). For a namespace-wholesale restore, a RoleBinding's
// ServiceAccount subjects must follow the moved namespace, otherwise RBAC keeps pointing at the
// source namespace.
func rewriteInSpecNamespaces(out *unstructured.Unstructured, targetNamespace string) {
	if out.GetKind() != "RoleBinding" {
		return
	}
	subjects, found, err := unstructured.NestedSlice(out.Object, "subjects")
	if err != nil || !found {
		return
	}
	changed := false
	for i := range subjects {
		s, ok := subjects[i].(map[string]interface{})
		if !ok {
			continue
		}
		kind, _ := s["kind"].(string)
		if kind != "ServiceAccount" {
			continue
		}
		if _, has := s["namespace"]; !has {
			continue
		}
		s["namespace"] = targetNamespace
		subjects[i] = s
		changed = true
	}
	if changed {
		_ = unstructured.SetNestedSlice(out.Object, subjects, "subjects")
	}
}
