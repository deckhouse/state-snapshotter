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
// generic Kubernetes/CSI restore (PVC -> VolumeSnapshot dataSourceRef) and delegates any
// domain-specific rewrite (e.g. a disk object pointing at its own snapshot) to registered
// DomainRestoreTransformer implementations living in their domain packages.

const (
	pvcKind                   = "PersistentVolumeClaim"
	corePVCVersion            = "v1"
	csiSnapshotAPIGroup       = "snapshot.storage.k8s.io"
	kindVolumeSnapshot        = "VolumeSnapshot"
	kindVolumeSnapshotContent = "VolumeSnapshotContent"
)

// transformNodeObjects compiles one node's captured manifests into apply-ready objects. children are
// the already-compiled restore-ready objects of this node's child snapshots (post-order), passed to
// domain transformers so a parent can reference its restored children.
func transformNodeObjects(node *RestoreNode, raw []unstructured.Unstructured, transformers []DomainRestoreTransformer, children []NodeResult, targetNamespace string) ([]unstructured.Unstructured, error) {
	// PVCs that a domain object will recreate on restore (e.g. a disk's data leg). They must not be
	// emitted as standalone PVCs and must not be treated as orphan PVCs (no VolumeSnapshot leaf).
	covered := coveredPVCNames(node, raw, transformers)

	// Resolve orphan PVC -> VolumeSnapshot on the raw objects (uid and source namespace still intact)
	// so binding lookup is reliable before sanitization strips those fields.
	orphanVS, err := resolveOrphanPVCVolumeSnapshots(node, raw, covered)
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
			if _, c := covered[name]; c {
				continue
			}
			if vs, ok := orphanVS[name]; ok {
				setPVCDataSourceRef(&sanitized, vs)
			}
			// Ordinary PVC without a dataRefs binding passes through sanitized.
			// TODO(restore): a PVC that is expected to be data-restored but has no dataRef should be
			// a contract violation; the MVP cannot yet distinguish "no data" from "missing data".
			out = append(out, sanitized)
			continue
		}

		handledBy := -1
		for ti, t := range transformers {
			handled, terr := t.TransformObject(node, &sanitized, children)
			if terr != nil {
				return nil, terr
			}
			if !handled {
				continue
			}
			// A single object must be owned by at most one domain transformer; two handlers racing on
			// the same object is an ambiguous restore contract.
			if handledBy >= 0 {
				return nil, fmt.Errorf(
					"%w: object %s/%s %s handled by more than one domain restore transformer",
					ErrContractViolation, sanitized.GetNamespace(), sanitized.GetName(), sanitized.GetKind(),
				)
			}
			handledBy = ti
		}
		out = append(out, sanitized)
	}
	return out, nil
}

func coveredPVCNames(node *RestoreNode, raw []unstructured.Unstructured, transformers []DomainRestoreTransformer) map[string]struct{} {
	covered := map[string]struct{}{}
	for _, t := range transformers {
		for name := range t.CoveredPVCNames(node, raw) {
			covered[name] = struct{}{}
		}
	}
	return covered
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
