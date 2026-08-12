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
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Restore node transform (ADR 2026-06-10 D4/D5). Each RestoreNode's captured manifests are sanitized
// for restore output, then turned into apply-ready objects. This layer is domain-free: it only knows
// generic Kubernetes/CSI restore (PVC -> VolumeSnapshot dataSourceRef). Any domain-specific rewrite
// (e.g. a disk object pointing at its own snapshot) is owned by the out-of-process domain controller's
// aggregated apiserver, which the compiler delegates whole domain subtrees to (see DomainSubtreeRestorer
// and Service.compileNode); generic nodes processed here never contain domain-owned objects.

const (
	pvcKind                   = "PersistentVolumeClaim"
	corePVCVersion            = "v1"
	csiSnapshotAPIGroup       = "snapshot.storage.k8s.io"
	kindVolumeSnapshot        = "VolumeSnapshot"
	kindVolumeSnapshotContent = "VolumeSnapshotContent"
)

// transformNodeObjects compiles one generic node's captured manifests into apply-ready objects:
// sanitize for restore, then bind every namespaced PVC to its orphan-PVC VolumeSnapshot
// (spec.dataSourceRef) or fail closed. Domain-owned objects never reach here — their subtree is
// delegated to the domain apiserver before this is called.
func transformNodeObjects(node *RestoreNode, raw []unstructured.Unstructured, targetNamespace string) ([]unstructured.Unstructured, error) {
	// Resolve orphan PVC -> VolumeSnapshot on the raw objects (uid and source namespace still intact)
	// so binding lookup is reliable before sanitization strips those fields.
	orphanVS, err := resolveOrphanPVCVolumeSnapshots(node, raw, nil)
	if err != nil {
		return nil, err
	}

	out := make([]unstructured.Unstructured, 0, len(raw))
	for i := range raw {
		sanitized, keep := sanitizeForRestore(raw[i], targetNamespace)
		if !keep {
			continue
		}

		if isCorePVC(sanitized) {
			name := sanitized.GetName()
			vs, ok := orphanVS[name]
			if !ok {
				// Fail closed: emitting a namespaced PVC without a dataSourceRef would restore it
				// empty (silent data loss). A PVC in a generic namespace snapshot must be backed by a
				// dataRefs -> VolumeSnapshot binding (domain-owned PVCs live in delegated subtrees).
				// TODO(restore): add an explicit "stateless/empty PVC" annotation or policy to allow
				// a deliberate data-less passthrough; until then any emitted PVC must have data.
				return nil, fmt.Errorf(
					"%w: PVC %s/%s has no data binding; refusing to emit a data-less PVC",
					ErrContractViolation, sanitized.GetNamespace(), name,
				)
			}
			setPVCDataSourceRef(&sanitized, vs)
			out = append(out, sanitized)
			continue
		}

		out = append(out, sanitized)
	}
	return out, nil
}

func resolveOrphanPVCVolumeSnapshots(node *RestoreNode, raw []unstructured.Unstructured, covered map[string]struct{}) (map[string]string, error) {
	result := map[string]string{}
	for i := range raw {
		if !isCorePVC(raw[i]) {
			continue
		}
		name := raw[i].GetName()
		if _, c := covered[name]; c {
			continue
		}
		binding, ok := findDataBindingForPVC(raw[i], node.DataBindings)
		if !ok {
			continue
		}
		// The orphan-PVC data artifact must be a durable VolumeSnapshotContent; anything else means the
		// dataRefs contract is broken, and surfacing it directly beats an indirect "no VS leaf" error.
		if binding.Artifact.Kind != kindVolumeSnapshotContent {
			return nil, fmt.Errorf(
				"%w: PVC %s/%s data artifact is %q, want %s",
				ErrContractViolation, raw[i].GetNamespace(), name, binding.Artifact.Kind, kindVolumeSnapshotContent,
			)
		}
		vsName := node.VSCToVS[binding.Artifact.Name]
		if vsName == "" {
			return nil, fmt.Errorf(
				"%w: no VolumeSnapshot leaf for PVC %s/%s (artifact VSC %q); orphan-PVC capture must publish the VolumeSnapshot visibility leaf",
				ErrContractViolation, raw[i].GetNamespace(), name, binding.Artifact.Name,
			)
		}
		result[name] = vsName
	}
	return result, nil
}

func isCorePVC(obj unstructured.Unstructured) bool {
	return obj.GetKind() == pvcKind && obj.GetAPIVersion() == corePVCVersion
}

func setPVCDataSourceRef(pvc *unstructured.Unstructured, volumeSnapshotName string) {
	dataSourceRef := map[string]interface{}{
		"apiGroup": csiSnapshotAPIGroup,
		"kind":     kindVolumeSnapshot,
		"name":     volumeSnapshotName,
	}
	_ = unstructured.SetNestedMap(pvc.Object, dataSourceRef, "spec", "dataSourceRef")
}
