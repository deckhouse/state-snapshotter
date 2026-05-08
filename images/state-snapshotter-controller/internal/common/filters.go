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

package common //nolint:revive // shared filters/helpers for controller paths

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

const (
	// AnnotationIncludeSecret allows an Opaque Secret to be stored without data.
	AnnotationIncludeSecret = "backup.deckhouse.io/include-secret"
	// AnnotationIncludeSecretData allows an Opaque Secret to be stored with data.
	AnnotationIncludeSecretData = "backup.deckhouse.io/include-secret-data"
)

// Constants for resource type filtering
var (
	// Ephemeral kinds that should be excluded (TZ section 4.1)
	ephemeralKinds = map[string]bool{
		"Pod":                true, // TZ: exclude ALL Pods
		"Event":              true,
		"Endpoints":          true,
		"EndpointSlice":      true,
		"Lease":              true,
		"Node":               true, // TZ: exclude Node
		"ControllerRevision": true, // TZ: exclude ControllerRevision
	}

	// Storage & virtualization objects (CSI and VD layers)
	storageKinds = map[string]bool{
		"PersistentVolume":      true,
		"PersistentVolumeClaim": true,
		"StorageClass":          true,
		"CSIDriver":             true,
		"CSINode":               true,
		"VolumeSnapshot":        true,
		"VolumeSnapshotContent": true,
		"VolumeSnapshotClass":   true,
		"VirtualDisk":           true,
		"VirtualDiskSnapshot":   true,
	}
)

// ShouldSkipObject determines whether a resource should be excluded
// from backup or restore operations. It unifies logic for both archive
// creation and restore application.
// If excludeKinds is provided (from ConfigMap), it will be checked in addition to built-in rules.
// If nil or empty, only built-in rules are applied.
func ShouldSkipObject(u *unstructured.Unstructured, excludeKinds []string) bool {
	kind := u.GetKind()
	name := u.GetName()
	labels := u.GetLabels()

	// 1) Explicit opt-out
	if labels != nil && labels["backup.deckhouse.io/exclude-from-backup"] == "true" {
		return true
	}

	// 2) System or backup service ConfigMaps
	if kind == "ConfigMap" {
		if labels != nil {
			if labels["app"] == "yaml-config" ||
				labels["app"] == "archive-structure" ||
				labels["app"] == "snapshot-archive" {
				return true
			}
		}
		if name == "backup-archiver-image" ||
			name == "kube-root-ca.crt" ||
			strings.HasPrefix(name, "kube-root-ca.") ||
			name == "istio-ca-root-cert" {
			return true
		}
	}

	// 3) Secrets are sensitive by default.
	if kind == "Secret" {
		return ShouldSkipSecretObject(u)
	}

	// 4) Temporary PVCs
	if kind == "PersistentVolumeClaim" {
		if strings.HasPrefix(name, "tmp-de-") || strings.HasPrefix(name, "de-tmp-") {
			return true
		}
	}

	// 5) Owner references — skip managed objects
	if ownerRefs := u.GetOwnerReferences(); len(ownerRefs) > 0 {
		return true
	}

	// 6) Ephemeral kinds (built-in)
	if ephemeralKinds[kind] {
		return true
	}

	// 7) ConfigMap-based excludeKinds (TZ section 7)
	// Supports exact match and wildcard patterns (e.g., "*Snapshot", "*SnapshotContent")
	if len(excludeKinds) > 0 {
		for _, excludeKind := range excludeKinds {
			excludeKind = strings.TrimSpace(excludeKind)
			if excludeKind == "" {
				continue
			}
			// Exact match
			if excludeKind == kind {
				return true
			}
			// Wildcard pattern (e.g., "*Snapshot" matches "VolumeSnapshot")
			if strings.HasPrefix(excludeKind, "*") {
				suffix := excludeKind[1:]
				if strings.HasSuffix(kind, suffix) {
					return true
				}
			}
			if strings.HasSuffix(excludeKind, "*") {
				prefix := excludeKind[:len(excludeKind)-1]
				if strings.HasPrefix(kind, prefix) {
					return true
				}
			}
		}
	}

	// 8) Deckhouse-managed system resources
	if labels != nil && labels["heritage"] == "deckhouse" {
		return true
	}

	// 9) Storage & virtualization objects (CSI and VD layers)
	if storageKinds[kind] {
		return true
	}

	// 10) Ephemeral workload layers (ReplicaSets with owner)
	// Note: Pod is already excluded in ephemeralKinds (TZ requires ALL Pods to be excluded)
	if kind == "ReplicaSet" {
		for _, or := range u.GetOwnerReferences() {
			if or.Kind == "Deployment" {
				return true
			}
		}
	}

	return false
}

// ShouldSkipSecretObject applies the ManifestCheckpoint Secret security contract.
func ShouldSkipSecretObject(u *unstructured.Unstructured) bool {
	if u == nil || u.GetKind() != "Secret" {
		return false
	}

	secretType, found, _ := unstructured.NestedString(u.Object, "type")
	if !found || secretType != "Opaque" {
		return true
	}

	annotations := u.GetAnnotations()
	if annotations[AnnotationIncludeSecret] == "true" || annotations[AnnotationIncludeSecretData] == "true" {
		return false
	}

	return true
}

// ShouldSkipObjectWithLog is a helper function that combines ShouldSkipObject
// with logging. It logs when an object is skipped and returns the same result.
// Uses default excludeKinds (nil) - ConfigMap customization is not applied here.
func ShouldSkipObjectWithLog(u *unstructured.Unstructured, log logger.LoggerInterface) bool {
	if ShouldSkipObject(u, nil) {
		log.Info("Skipping object", "kind", u.GetKind(), "name", u.GetName())
		return true
	}
	return false
}
