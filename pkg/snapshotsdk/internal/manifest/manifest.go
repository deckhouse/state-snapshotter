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

// Package manifest builds the ManifestCaptureRequest target set: the base manifest target plus the
// owned-PVC targets derived from the snapshot's own data-leg VolumeCaptureRequest.
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

// Targets merges the base manifest target with the owned-PVC targets derived from the data-leg VCR,
// deduplicated by (apiVersion|kind|namespace|name) and sorted deterministically.
func Targets(base []ssv1alpha1.ManifestTarget, ownedPVCs []storagefoundation.Target, namespace string) []ssv1alpha1.ManifestTarget {
	owned := fromVolumeTargets(ownedPVCs)
	return appendOwnedPVCManifestTargets(base, owned, namespace)
}

func fromVolumeTargets(targets []storagefoundation.Target) []ssv1alpha1.ManifestTarget {
	out := make([]ssv1alpha1.ManifestTarget, 0, len(targets))
	for _, t := range targets {
		if t.Kind != "PersistentVolumeClaim" {
			continue
		}
		apiVersion := t.APIVersion
		if apiVersion == "" {
			apiVersion = corev1.SchemeGroupVersion.String()
		}
		out = append(out, ssv1alpha1.ManifestTarget{
			APIVersion: apiVersion,
			Kind:       t.Kind,
			Name:       t.Name,
		})
	}
	sortTargets(out)
	return out
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
