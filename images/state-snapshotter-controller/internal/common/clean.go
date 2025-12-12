/*
Copyright 2025 Flant JSC

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

package common

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// CleanObjectForSnapshot removes runtime fields and shrinks metadata for snapshot storage.
// This is a unified function used by both manifest checkpoint and namespace archive services.
// TZ section 4.2-4.4: Remove metadata fields, status, and system annotations.
//
// excludeAnnotations (from ConfigMap) can be provided to customize which annotations to remove.
// If nil or empty, uses default system annotations.
//
// Returns nil if the object should not be archived at all.
func CleanObjectForSnapshot(u *unstructured.Unstructured, excludeAnnotations []string) *unstructured.Unstructured {
	if u == nil {
		return nil
	}

	kind := u.GetKind()
	ann := u.GetAnnotations()
	lbl := u.GetLabels()

	// Drop ephemeral/derived objects (already filtered by ShouldSkipObject, but double-check)
	switch kind {
	case "Pod", "ReplicaSet", "Endpoints", "EndpointSlice", "Event", "Lease":
		return nil
	}

	out := u.DeepCopy()

	// Metadata noise (TZ section 4.2)
	unstructured.RemoveNestedField(out.Object, "metadata", "managedFields")
	unstructured.RemoveNestedField(out.Object, "metadata", "resourceVersion")
	unstructured.RemoveNestedField(out.Object, "metadata", "uid")
	unstructured.RemoveNestedField(out.Object, "metadata", "generation")
	unstructured.RemoveNestedField(out.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(out.Object, "metadata", "deletionTimestamp")
	unstructured.RemoveNestedField(out.Object, "metadata", "deletionGracePeriodSeconds")
	unstructured.RemoveNestedField(out.Object, "metadata", "finalizers")
	unstructured.RemoveNestedField(out.Object, "metadata", "ownerReferences")

	// Remove system annotations (TZ section 4.4)
	// Exclude: kubectl.kubernetes.io/last-applied-configuration, deployment.kubernetes.io/*,
	// autoscaling.alpha.kubernetes.io/*, checksum/*, helm.sh/*
	// Can be customized via ConfigMap excludeAnnotations
	if len(excludeAnnotations) == 0 {
		// Default system annotations
		excludeAnnotations = []string{
			"kubectl.kubernetes.io/last-applied-configuration",
			"deployment.kubernetes.io/*",
			"autoscaling.alpha.kubernetes.io/*",
			"checksum/*",
			"helm.sh/*",
		}
	}

	keepAnn := map[string]string{}
	keepLbl := map[string]string{}
	for k, v := range ann {
		shouldExclude := false
		// Check against excludeAnnotations patterns
		for _, excludeAnn := range excludeAnnotations {
			excludeAnn = strings.TrimSpace(excludeAnn)
			if excludeAnn == "" {
				continue
			}
			// Exact match
			if excludeAnn == k {
				shouldExclude = true
				break
			}
			// Wildcard pattern (e.g., "deployment.kubernetes.io/*")
			if strings.HasSuffix(excludeAnn, "/*") {
				prefix := excludeAnn[:len(excludeAnn)-2]
				if strings.HasPrefix(k, prefix) {
					shouldExclude = true
					break
				}
			}
		}
		if shouldExclude {
			continue
		}
		// Keep only backup.deckhouse.io annotations
		if strings.HasPrefix(k, "backup.deckhouse.io/") {
			keepAnn[k] = v
		}
	}
	for k, v := range lbl {
		if k == "app" || strings.HasPrefix(k, "app.kubernetes.io/") || strings.HasPrefix(k, "backup.deckhouse.io/") {
			keepLbl[k] = v
		}
	}
	out.SetAnnotations(keepAnn)
	out.SetLabels(keepLbl)

	// Drop status unless explicitly requested (TZ section 4.3)
	preserveStatus := ann != nil && ann["backup.deckhouse.io/preserve-status"] == "true"
	if !preserveStatus {
		unstructured.RemoveNestedField(out.Object, "status")
	}

	// Kind-specific cleanup
	switch kind {
	case "Secret":
		// Do not include secret data unless explicitly requested
		if !(ann != nil && ann["backup.deckhouse.io/include-secret-data"] == "true") {
			unstructured.RemoveNestedField(out.Object, "data")
			unstructured.RemoveNestedField(out.Object, "stringData")
		}
	case "Service":
		unstructured.RemoveNestedField(out.Object, "spec", "clusterIP")
		unstructured.RemoveNestedField(out.Object, "spec", "clusterIPs")
		unstructured.RemoveNestedField(out.Object, "spec", "healthCheckNodePort")
		unstructured.RemoveNestedField(out.Object, "spec", "sessionAffinityConfig")
		unstructured.RemoveNestedField(out.Object, "spec", "internalTrafficPolicy")
	case "PersistentVolumeClaim":
		unstructured.RemoveNestedField(out.Object, "spec", "volumeName")
		unstructured.RemoveNestedField(out.Object, "status")
	case "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob":
		unstructured.RemoveNestedField(out.Object, "status")
	}

	// Ensure GVK is still present; if not, restore from source or drop
	if out.GetKind() == "" {
		out.SetKind(u.GetKind())
	}
	if out.GetAPIVersion() == "" {
		out.SetAPIVersion(u.GetAPIVersion())
	}
	if out.GetKind() == "" || out.GetAPIVersion() == "" {
		return nil
	}

	return out
}
