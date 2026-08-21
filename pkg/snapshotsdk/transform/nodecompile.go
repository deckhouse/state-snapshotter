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
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Generic restore compilation of ONE snapshot node: sanitize the node's captured manifests, then bind
// every emitted PVC to the VolumeSnapshot that carries its data. It knows only generic Kubernetes/CSI
// restore (PVC -> VolumeSnapshot dataSourceRef) and no domain kinds; the single domain-supplied input is
// the set of PVCs a domain object recreates itself (Transformer.CoveredPVCNames).
//
// Every server of manifests-with-data-restoration compiles its own nodes with this function — the core
// compiler for the nodes it owns, each domain facade for the subtree delegated to it — and a client
// applies the concatenation. That is why the fail-closed rules below belong here and not to one server:
// a second implementation that drops one of them restores a PVC with no data source, which the API
// accepts and provisions as an EMPTY volume. The failure is silent at restore time and only visible when
// someone reads the data back.

const (
	pvcKind                   = "PersistentVolumeClaim"
	corePVCVersion            = "v1"
	csiSnapshotAPIGroup       = "snapshot.storage.k8s.io"
	kindVolumeSnapshot        = "VolumeSnapshot"
	kindVolumeSnapshotContent = "VolumeSnapshotContent"
)

// NodeRestoreInput is everything the generic compiler needs about ONE snapshot node. The node's identity,
// its checkpoint and its children are deliberately absent: resolving those is the caller's model (the
// core run-tree resolver, a domain facade's own recursion), while what is compiled from them is shared.
type NodeRestoreInput struct {
	// CapturedManifests are this node's own captured manifests, as stored at capture time (uid and
	// source namespace still intact — the data binding lookup needs them).
	CapturedManifests []unstructured.Unstructured
	// TargetNamespace is the namespace the output is rewritten to.
	TargetNamespace string
	// SourceNamespace is the namespace the manifests were CAPTURED in. Leave it empty when
	// CapturedManifests carry it themselves (anything read out of a checkpoint does): it is then taken
	// from each object. Set it when they do not — a facade that attaches the target namespace to
	// namespace-relative manifests before compiling must say what the source was, or in-body namespace
	// references stop being recognised as moved. See SanitizeForRestore.
	SourceNamespace string
	// PVCDataBindings are this node's own PVC -> durable artifact bindings (SnapshotContent dataRefs).
	PVCDataBindings []PVCDataBinding
	// VolumeSnapshotByContentName maps a durable VolumeSnapshotContent name to the namespaced
	// VolumeSnapshot that publishes it. It is the ONLY supported way to turn a data binding into a
	// PVC.spec.dataSourceRef: a VolumeSnapshot read out of captured manifests would not be the
	// visibility leaf the restore target can bind to.
	VolumeSnapshotByContentName map[string]string
	// DomainCoveredPVCNames are PVCs of this node that the domain object recreates itself
	// (Transformer.CoveredPVCNames). They are neither emitted nor expected to have a VolumeSnapshot: the
	// restored domain object carries their data. A caller with no domain objects in the node leaves it
	// nil, and then every PVC in the node must be backed by a data binding.
	DomainCoveredPVCNames map[string]struct{}
}

// CompileNodeManifests compiles one node's captured manifests into apply-ready objects: sanitize for
// restore, then bind every emitted namespaced PVC to its VolumeSnapshot (spec.dataSourceRef) or fail
// closed. Domain-owned objects of a delegated subtree never reach here — the delegating compiler stops at
// the domain boundary and splices the facade's already apply-ready answer.
func CompileNodeManifests(in NodeRestoreInput) ([]unstructured.Unstructured, error) {
	// Resolve orphan PVC -> VolumeSnapshot on the captured objects (uid and source namespace still
	// intact) so binding lookup is reliable before sanitization strips those fields.
	orphanVS, err := resolveOrphanPVCVolumeSnapshots(in)
	if err != nil {
		return nil, err
	}

	out := make([]unstructured.Unstructured, 0, len(in.CapturedManifests))
	for i := range in.CapturedManifests {
		sanitized, keep := SanitizeForRestore(in.CapturedManifests[i], in.SourceNamespace, in.TargetNamespace)
		if !keep {
			continue
		}

		if isCorePVC(sanitized) {
			name := sanitized.GetName()
			if _, covered := in.DomainCoveredPVCNames[name]; covered {
				continue
			}
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

// resolveOrphanPVCVolumeSnapshots maps each captured PVC of the node to the namespaced VolumeSnapshot
// that carries its data, via the node's data binding and the durable artifact's visibility leaf. A PVC the
// domain recreates itself is skipped: it has no data binding of its own, and demanding one would fail a
// perfectly valid domain node.
func resolveOrphanPVCVolumeSnapshots(in NodeRestoreInput) (map[string]string, error) {
	result := map[string]string{}
	for i := range in.CapturedManifests {
		captured := in.CapturedManifests[i]
		if !isCorePVC(captured) {
			continue
		}
		name := captured.GetName()
		if _, covered := in.DomainCoveredPVCNames[name]; covered {
			continue
		}
		binding, ok := FindDataBindingForPVC(captured, in.PVCDataBindings)
		if !ok {
			continue
		}
		// The orphan-PVC data artifact must be a durable VolumeSnapshotContent; anything else means the
		// dataRefs contract is broken, and surfacing it directly beats an indirect "no VS leaf" error.
		if binding.Artifact.Kind != kindVolumeSnapshotContent {
			return nil, fmt.Errorf(
				"%w: PVC %s/%s data artifact is %q, want %s",
				ErrContractViolation, captured.GetNamespace(), name, binding.Artifact.Kind, kindVolumeSnapshotContent,
			)
		}
		vsName := in.VolumeSnapshotByContentName[binding.Artifact.Name]
		if vsName == "" {
			return nil, fmt.Errorf(
				"%w: no VolumeSnapshot leaf for PVC %s/%s (artifact VSC %q); orphan-PVC capture must publish the VolumeSnapshot visibility leaf",
				ErrContractViolation, captured.GetNamespace(), name, binding.Artifact.Name,
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
