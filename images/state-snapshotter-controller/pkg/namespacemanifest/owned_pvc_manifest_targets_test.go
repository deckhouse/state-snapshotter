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

package namespacemanifest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

func TestAppendOwnedPVCManifestTargets_SkipsSubtreeExclude(t *testing.T) {
	base := []ManifestTarget{{APIVersion: "apps/v1", Kind: "Deployment", Name: "app"}}
	owned := []ManifestTarget{{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "PersistentVolumeClaim", Name: "data"}}
	excl := map[string]struct{}{
		ManifestTargetDedupKey("ns", owned[0]): {},
	}
	out := AppendOwnedPVCManifestTargets(base, owned, excl, "ns")
	if len(out) != 1 || out[0].Name != "app" {
		t.Fatalf("child-owned PVC must not appear on root targets: %#v", out)
	}
}

func TestAppendOwnedPVCManifestTargets_AppendsResidualPVC(t *testing.T) {
	owned := []ManifestTarget{{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "root-data"}}
	out := AppendOwnedPVCManifestTargets(nil, owned, nil, "ns")
	if len(out) != 1 || out[0].Name != "root-data" {
		t.Fatalf("expected owned PVC target, got %#v", out)
	}
}

func TestManifestTargetsFromVolumeTargets(t *testing.T) {
	out := ManifestTargetsFromVolumeTargets([]vcpkg.Target{{
		APIVersion: corev1.SchemeGroupVersion.String(),
		Kind:       "PersistentVolumeClaim",
		Name:       "pvc-a",
		Namespace:  "ns",
	}})
	if len(out) != 1 || out[0].Name != "pvc-a" {
		t.Fatalf("unexpected targets: %#v", out)
	}
}
