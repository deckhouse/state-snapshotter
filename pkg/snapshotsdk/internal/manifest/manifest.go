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

// Package manifest builds the ManifestCaptureRequest target set: the domain-chosen manifest targets plus
// the owned-PVC targets derived from the snapshot's own data-capture VolumeCaptureRequest.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/storagefoundation"
)

// RequestName returns the deterministic ManifestCaptureRequest name for a snapshot identity. The name is
// derivable from the snapshot alone (kind/namespace/name), so it is stable across reconciles and restarts.
func RequestName(kind, namespace, name string) string {
	sum := sha256.Sum256([]byte(kind + ":" + namespace + "/" + name))
	return "mcr-" + hex.EncodeToString(sum[:10])
}

// Targets merges the base manifest target(s) with the snapshot's single owned data-capture PVC (derived
// from the data-capture VCR), deduplicated by (apiVersion|kind|namespace|name) and sorted deterministically.
// A nil ownedPVC (manifest-only snapshot) returns the base unchanged.
func Targets(base []ssv1alpha1.ManifestTarget, ownedPVC *storagefoundation.Target, namespace string) []ssv1alpha1.ManifestTarget {
	owned := ownedPVCManifestTarget(ownedPVC)
	return appendOwnedPVCManifestTargets(base, owned, namespace)
}

func ownedPVCManifestTarget(t *storagefoundation.Target) []ssv1alpha1.ManifestTarget {
	if t == nil || t.Kind != "PersistentVolumeClaim" {
		return nil
	}
	apiVersion := t.APIVersion
	if apiVersion == "" {
		apiVersion = corev1.SchemeGroupVersion.String()
	}
	return []ssv1alpha1.ManifestTarget{{
		APIVersion: apiVersion,
		Kind:       t.Kind,
		Name:       t.Name,
	}}
}

func appendOwnedPVCManifestTargets(base, owned []ssv1alpha1.ManifestTarget, namespace string) []ssv1alpha1.ManifestTarget {
	if len(owned) == 0 {
		return append([]ssv1alpha1.ManifestTarget(nil), base...)
	}
	seen := make(map[string]struct{}, len(base)+len(owned))
	for _, t := range base {
		seen[dedupKey(namespace, t)] = struct{}{}
	}
	out := append([]ssv1alpha1.ManifestTarget(nil), base...)
	for _, t := range owned {
		k := dedupKey(namespace, t)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, t)
	}
	sortTargets(out)
	return out
}

// TargetsEqualIgnoreOrder reports set equality of two manifest target slices by canonical key
// (apiVersion, kind, name). Order is ignored and duplicates collapse (set semantics): all targets of a
// ManifestCaptureRequest share its namespace, so the triple is the canonical per-request identity. It is
// the manifest-capture analogue of the child snapshot RefsEqualIgnoreOrder drift check.
func TargetsEqualIgnoreOrder(a, b []ssv1alpha1.ManifestTarget) bool {
	sa := targetKeySet(a)
	sb := targetKeySet(b)
	if len(sa) != len(sb) {
		return false
	}
	for k := range sa {
		if _, ok := sb[k]; !ok {
			return false
		}
	}
	return true
}

func targetKeySet(targets []ssv1alpha1.ManifestTarget) map[string]struct{} {
	set := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		set[t.APIVersion+"|"+t.Kind+"|"+t.Name] = struct{}{}
	}
	return set
}

func sortTargets(targets []ssv1alpha1.ManifestTarget) {
	sort.Slice(targets, func(i, j int) bool {
		a, b := targets[i], targets[j]
		if a.APIVersion != b.APIVersion {
			return a.APIVersion < b.APIVersion
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
}

func dedupKey(namespace string, t ssv1alpha1.ManifestTarget) string {
	ns := namespace
	if ns == "" {
		ns = "_cluster"
	}
	return fmt.Sprintf("%s|%s|%s|%s", t.APIVersion, t.Kind, ns, t.Name)
}
