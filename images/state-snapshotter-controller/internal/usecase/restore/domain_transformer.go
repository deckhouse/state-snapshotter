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

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// DomainRestoreTransformer is the extension point that keeps the restore compiler domain-free
// (ADR 2026-06-10 D5). Domain packages (for example the demo controllers) implement it and register
// it into the restore Service; the generic compiler never references domain kinds or field names.
//
// It replaces the "inline demo helper" MVP idea with an in-process registered transformer: still no
// new HTTP subresource, but the domain logic lives next to its domain types, not in generic usecase.
type DomainRestoreTransformer interface {
	// CoveredPVCNames returns names of PVCs in this node that the domain object recreates on restore.
	// The compiler suppresses those PVCs and does not treat them as orphan PVCs (no VolumeSnapshot
	// leaf is expected for them). objects are this node's raw captured manifests.
	CoveredPVCNames(node *RestoreNode, objects []unstructured.Unstructured) map[string]struct{}

	// TransformObject mutates a single already-sanitized domain object in place to make it
	// restore-ready (for example, setting a disk's dataSource to its snapshot). children carries the
	// already-compiled, restore-ready objects of this node's child snapshots (post-order, bottom-up),
	// so a parent domain object can reference its restored children. It returns true if it handled the
	// object; it must ignore (return false, nil for) objects it does not own.
	TransformObject(node *RestoreNode, obj *unstructured.Unstructured, children []NodeResult) (bool, error)
}
