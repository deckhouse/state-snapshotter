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

package snapshot

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snaphelpers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

// namespaceManifestSpec converts the root's namespace manifest capture targets (built by
// usecase.BuildRootNamespaceManifestCaptureTargets, the wave-barrier exclude-set builder) into the SDK's
// ManifestCaptureSpec target SET. The root's manifest leg is the whole namespace, so — unlike a
// single-object domain — it hands the SDK many targets at once (enabled by the wave5 multi-target
// ManifestCaptureSpec). The SDK owns MCR create + name publication; this is a pure shaping helper.
//
// Pure planner (PR-A, wave5 design §6.3): extracted so it can be unit-tested and later fed to
// sdk.EnsureManifestCapture (PR-B) without duplicating the target-set logic.
func namespaceManifestSpec(targets []namespacemanifest.ManifestTarget) snapshotsdk.ManifestCaptureSpec {
	specTargets := make([]snapshotsdk.ManifestTarget, 0, len(targets))
	for _, t := range targets {
		specTargets = append(specTargets, snapshotsdk.ManifestTarget{
			APIVersion: t.APIVersion,
			Kind:       t.Kind,
			Name:       t.Name,
		})
	}
	return snapshotsdk.ManifestCaptureSpec{Targets: specTargets}
}

// buildNamespaceChildSpec builds one root-owned domain child snapshot as an SDK ChildSpec: the child
// object carries only kind/name/namespace and the immutable spec.sourceRef (the single source-of-truth
// for what it captures). It deliberately does NOT stamp the owner reference — the SDK owns adoption
// (owner reference), create-or-validate, and SnapshotChildRef derivation (see snapshotsdk.ChildSpec), so
// the planner mirrors what parent_graph.go's ensureParentOwnedChildSnapshot builds at CREATE time, minus
// the owner ref the SDK now stamps.
//
// Pure planner (PR-A, wave5 design §6.2): extracted so the child-object shape is unit-testable in
// isolation and later emitted via sdk.EnsureChildren (PR-B).
func buildNamespaceChildSpec(namespace, name string, gvk schema.GroupVersionKind, src controllercommon.SnapshotSourceIdentity) snapshotsdk.ChildSpec {
	child := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": gvk.GroupVersion().String(),
			"kind":       gvk.Kind,
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"sourceRef": map[string]interface{}{
					"apiVersion": src.APIVersion,
					"kind":       src.Kind,
					"name":       src.Name,
				},
			},
		},
	}
	child.SetGroupVersionKind(gvk)
	return snapshotsdk.ChildSpec{Object: child}
}
