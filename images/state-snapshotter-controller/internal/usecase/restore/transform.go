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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/transform"
)

// transformNodeObjects compiles one generic node's captured manifests into apply-ready objects through
// the shared compilation: sanitize for restore, then bind every namespaced PVC to its orphan-PVC
// VolumeSnapshot (spec.dataSourceRef) or fail closed. Domain-owned objects never reach here — their
// subtree is delegated to the domain apiserver before this is called.
//
// It passes NO covered-PVC set, and that is the core-side invariant, not an omission: a node compiled
// here is never a domain node, so nothing in it recreates a PVC on its own. Every PVC in a node's own
// manifests must therefore be backed by a dataRefs -> VolumeSnapshot binding, and one that is not aborts
// the request instead of restoring an empty volume. Guarded by
// TestTransformNode_PVCWithoutDataRefFailsClosed.
func transformNodeObjects(node *RestoreNode, captured []unstructured.Unstructured, targetNamespace string) ([]unstructured.Unstructured, error) {
	return transform.CompileNodeManifests(transform.NodeRestoreInput{
		CapturedManifests:           captured,
		TargetNamespace:             targetNamespace,
		PVCDataBindings:             pvcDataBindings(node.DataBindings),
		VolumeSnapshotByContentName: node.VSCToVS,
	})
}

// pvcDataBindings projects this node's SnapshotContent dataRefs onto the compilation's binding view. The
// volume metadata the internal binding also carries (volume mode, filesystem type, storage class) is
// consumed by the export/index path, not by restore compilation, so it is deliberately not passed on.
func pvcDataBindings(refs []snapshot.DataBindingRef) []transform.PVCDataBinding {
	if len(refs) == 0 {
		return nil
	}
	out := make([]transform.PVCDataBinding, 0, len(refs))
	for _, ref := range refs {
		out = append(out, transform.PVCDataBinding{
			TargetUID: ref.TargetUID,
			Target:    ref.Target,
			Artifact:  ref.Artifact,
		})
	}
	return out
}
