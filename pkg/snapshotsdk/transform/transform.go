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

// Package transform is the domain restore extension point of the snapshot SDK. A domain package implements
// Transformer next to its domain types; the generic restore compiler never references domain kinds or
// field names.
//
// # Sanctioned unstructured boundary
//
// Unlike the capture-side facade (package snapshotsdk), which is a typed, semantic API that deliberately
// hides Kubernetes transport, this package intentionally exposes unstructured.Unstructured in its public
// signatures. That is a conscious, documented exception, not a transport leak: the restore compiler
// operates over arbitrary captured manifests (ConfigMap, Secret, PVC, and unknown domain CRDs) whose Go
// types are not known at compile time, so a typed API is impossible here. The boundary is therefore:
//
//	capture  -> typed, semantic protocol (snapshotsdk.CaptureSDK)
//	restore  -> intentionally unstructured manifest transform (this package)
//
// Keep unstructured confined to this restore-transform seam; the capture facade must stay typed.
package transform

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// RestoreNode is the domain-facing view of one node in the restore run tree: the snapshot CR that owns
// the object being made restore-ready. It is deliberately minimal — a domain Transformer needs only the
// owning snapshot's identity (e.g. to point a restored object at its own snapshot via spec.dataSource).
// The core compiler's richer internal node (MCP, dataRefs, VSC->VS leaves, children, domain-boundary
// marker) is NOT part of this public contract.
type RestoreNode struct {
	// SnapshotRef is the snapshot CR that owns this node. Namespace is the run-tree namespace.
	SnapshotRef storagev1alpha1.ObjectRef
}

// NodeResult carries the apply-ready objects already compiled for a child RestoreNode, passed to a
// parent Transformer during post-order (bottom-up) traversal so a parent domain object can reference its
// restored children.
type NodeResult struct {
	Node    *RestoreNode
	Objects []unstructured.Unstructured
}

// Transformer is the domain restore extension point. A domain package implements it next to its domain
// types; the generic restore compiler never references domain kinds or field names. The demo domain
// controller (internal/controllers/demo) is the reference implementation.
type Transformer interface {
	// CoveredPVCNames returns names of PVCs in this node that the domain object recreates on restore.
	// The compiler suppresses those PVCs and does not treat them as orphan PVCs (no VolumeSnapshot leaf
	// is expected for them). objects are this node's raw captured manifests.
	CoveredPVCNames(node *RestoreNode, objects []unstructured.Unstructured) map[string]struct{}

	// TransformObject mutates a single already-sanitized domain object in place to make it
	// restore-ready (for example, setting a disk's dataSource to its snapshot). children carries the
	// already-compiled, restore-ready objects of this node's child snapshots (post-order, bottom-up), so
	// a parent domain object can reference its restored children. It returns true if it handled the
	// object; it must return (false, nil) for objects it does not own.
	//
	// children may be nil: the reference wiring (internal/domainapi) currently restores one object at a
	// time and passes nil, so an implementation must not assume children is always populated. It is part
	// of the contract for domains whose parent objects reference their restored children.
	TransformObject(node *RestoreNode, obj *unstructured.Unstructured, children []NodeResult) (bool, error)
}
