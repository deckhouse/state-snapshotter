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

package transform

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Read-path restore-safe sanitizer.
//
// The restore compiler emits apply-ready manifests, so it MUST strip runtime/server-managed
// fields that block `kubectl apply` and rewrite the namespace to the restore target. Sanitization
// happens on the read path; capture stores raw manifests as-is.
//
// It lives in the SDK because the guarantee it carries is a property of the WHOLE compiled tree, not of
// one server: a client applies the concatenated output of the core compiler and of every domain facade
// that compiled a delegated subtree. A facade sanitizing by its own copy of these rules is how a subtree
// silently gains a field that blocks apply, or loses one that had to be stripped.

// controlPlaneExactKinds are control-plane / snapshot-machinery kinds that MUST NOT appear in
// restore output. VS/VSC and snapshot tree nodes are transferred separately (data + tree); the
// compiler only references them, never emits them as apply manifests.
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

// SanitizeForRestore returns a restore-safe copy of obj and whether it should be emitted.
//
// keep=false when the object must be dropped from restore output:
//   - cluster-scoped objects (no namespace) — namespace restore compiler is namespaced-only (MVP);
//   - control-plane / snapshot-machinery kinds (see isControlPlaneKind).
//
// For kept (namespaced) objects it removes runtime metadata, status, kind-specific server fields,
// and rewrites metadata.namespace to targetNamespace.
//
// sourceNamespace is the namespace obj was CAPTURED in. It is load-bearing because in-body namespace
// references are rewritten SELECTIVELY: it is what separates a reference that moves along with the
// content from one that points at an identity living elsewhere (see rewriteInSpecNamespaces). Passing ""
// means "take it from obj.metadata.namespace", which is correct for manifests read straight out of a
// checkpoint — capture stamps each namespaced object with its namespace, so the value is there by
// construction. A caller whose manifests do NOT carry it — a facade that attaches the node namespace to
// namespace-relative manifests before compiling — MUST pass it explicitly: with source == target
// nothing is recognised as moved, and references that were supposed to follow the content silently stop
// following it.
func SanitizeForRestore(obj unstructured.Unstructured, sourceNamespace, targetNamespace string) (unstructured.Unstructured, bool) {
	if isControlPlaneKind(obj.GetKind()) {
		return unstructured.Unstructured{}, false
	}
	// Cluster-scoped objects carry no namespace in the captured manifest (MCP keeps the original
	// namespace for namespaced objects). MVP: drop them, like /manifests.
	if obj.GetNamespace() == "" {
		return unstructured.Unstructured{}, false
	}

	// Default to the object's own namespace: read straight out of a checkpoint that IS the captured
	// namespace, and the guard above guarantees it is non-empty. An explicit argument wins, for callers
	// whose manifests no longer carry it.
	if sourceNamespace == "" {
		sourceNamespace = obj.GetNamespace()
	}

	out := obj.DeepCopy()
	stripRuntimeMetadata(out)
	stripRestoreBreakingAnnotations(out)
	unstructured.RemoveNestedField(out.Object, "status")
	out.SetNamespace(targetNamespace)
	stripKindSpecificFields(out)
	rewriteInSpecNamespaces(out, sourceNamespace, targetNamespace)
	return *out, true
}

// stripRuntimeMetadata removes server/runtime-managed metadata that blocks `kubectl apply` in a fresh
// cluster/namespace.
//
// Finalizers are DELIBERATELY preserved (they are NOT stripped here): Class-1 machine finalizers (e.g.
// kubernetes.io/pvc-protection) are re-added by the target cluster's controllers anyway, and Class-3
// custom finalizers encode user intent that must survive a restore. The only Class-2 "wedge" finalizer
// (the self-induced transient snapshot.storage.kubernetes.io/pvc-as-source-protection) is already
// stripped at capture time, so it never reaches this read path. NB (cross-cluster import): a Class-3
// custom finalizer whose controller is absent in the target cluster leaves the restored object
// un-deletable without manual intervention — that is the intended intent-preserving semantics.
//
// ownerReferences ARE still stripped: a dangling ownerReference (its owner UID does not exist in the
// target namespace/cluster) makes the API server garbage-collect the restored object immediately — real
// data loss — so it must not survive.
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
		if out.GetAPIVersion() == corePVCVersion {
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

// rewriteInSpecNamespaces rewrites namespace references that live inside subjects (not
// metadata.namespace, which is already rewritten). For a namespace-wholesale restore, a RoleBinding's
// ServiceAccount subjects must follow the moved namespace, otherwise RBAC keeps pointing at the
// source namespace and the restored ServiceAccount is granted nothing.
//
// It rewrites ONLY subjects whose namespace is the source namespace — the ones that moved along with
// the content. A subject in any other namespace is an identity that lives elsewhere and was merely
// granted access into the source namespace; it does not move, and rewriting it retargets the grant at
// a DIFFERENT ServiceAccount (an RBAC ServiceAccount subject is identified by the namespace/name pair).
// That retarget is silent in both directions: the API server accepts a subject that does not resolve,
// so the intended grantee loses access without any error, and if the target namespace happens to hold
// a same-named ServiceAccount, that unrelated identity silently gains the role instead.
func rewriteInSpecNamespaces(out *unstructured.Unstructured, sourceNamespace, targetNamespace string) {
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
		ns, has := s["namespace"].(string)
		if !has || ns != sourceNamespace {
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
