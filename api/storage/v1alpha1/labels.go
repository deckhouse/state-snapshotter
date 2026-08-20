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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
)

// ExcludeLabelKey is the absolute snapshot veto label. Any namespaced object carrying this label key
// (the value is ignored, matching Velero's backup.velero.io/exclude-from-backup convention) is excluded
// from EVERY snapshot, unconditionally, at every level of the snapshot tree. Recursion is intrinsic and
// free — the check is local at each enumeration point: a labeled parent's subtree is never expanded, and
// a labeled child drops out alone.
//
// This is the single exported source of truth for the veto key, reused by the core, the SDK, and domain
// controllers; there must be no hardcoded string duplicates elsewhere.
const ExcludeLabelKey = APIGroup + "/exclude"

// excludeVetoSelector is built once at package init. ExcludeLabelKey is a compile-time constant that is a
// valid label key and DoesNotExist takes no values, so the requirement cannot fail to build; a failure
// would mean the constant itself became malformed, and that must stop the process (and every test binary)
// rather than silently degrade the veto into "match everything".
var excludeVetoSelector = func() labels.Selector {
	req, err := labels.NewRequirement(ExcludeLabelKey, selection.DoesNotExist, nil)
	if err != nil {
		panic("malformed exclude veto label key " + ExcludeLabelKey + ": " + err.Error())
	}
	return labels.NewSelector().Add(*req)
}()

// ExcludeVetoSelector returns the label selector every list-based capture leg (namespace manifests, PVC
// candidates) filters with: everything EXCEPT objects carrying ExcludeLabelKey. It is deliberately NOT
// labels.Everything() — an empty selector would capture vetoed objects too. Enumeration legs that must
// also RECORD what they dropped check the label directly instead, so the drop lands in excludedRefs.
//
// The returned selector is shared and read-only (Selector.Add does not mutate its receiver), so callers
// must only match against it.
func ExcludeVetoSelector() labels.Selector { return excludeVetoSelector }

// LabelDeleteProtected is the canonical authoritative protection state for the unified snapshot tree.
// It is NOT a diagnostic marker: it is the single source of truth the
// delete-guard admission consults. Its presence with value LabelDeleteProtectedValue means the object is
// an internal node of a unified snapshot and MUST NOT be deleted by a direct user DELETE — legal removal
// happens only through root Snapshot teardown / controller GC / reclaim (exempt actors) or an explicit
// break-glass annotation. Properties: authoritative (admission reads only this), immutable for users
// (protected against UPDATE removal/change), and written exclusively by the authoritative write-path that
// introduces the node into the tree (steady state) or, once, by the versioned migration backfill for
// legacy objects. The root Snapshot deliberately does NOT carry it. Delete protection is a property of the
// snapshot protocol (tree correctness), not of an object's "importance".
//
// Scope: only the namespaced nodes a parent introduces into the tree — child snapshot CRs and the CSI
// VolumeSnapshots a domain captures. The cluster-scoped nodes of a tree (SnapshotContent, ManifestCheckpoint
// and its chunks, ObjectKeeper, CSI VolumeSnapshotContent) are deliberately left unmarked: they are not
// reachable by a namespace user, and damage there is surfaced by the tree's own readiness rather than
// prevented by admission. A write-path that creates such an object MUST NOT stamp this label; that absence
// is the contract, not an omission to be repaired later.
//
// This is the single exported source of truth for the key, reused by the core, the SDK, domain
// controllers and storage-foundation (VS/VSC write-path); there must be no hardcoded string duplicates.
const LabelDeleteProtected = APIGroup + "/delete-protected"

// LabelDeleteProtectedValue is the only meaningful value of LabelDeleteProtected. Protection is keyed on
// the exact key=value pair; any other value is treated by the guard as "not protected".
const LabelDeleteProtectedValue = "true"

// StampDeleteProtected sets the canonical delete-protected label on obj's labels (allocating the map if
// nil). Write-paths MUST call it while BUILDING the object, before Create, so the object appears in the
// API already protected — there is no guaranteed ordering between Create and a follow-up Patch, so a
// post-create patch would leave a short unprotected window. It is idempotent.
func StampDeleteProtected(obj metav1.Object) {
	l := obj.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	l[LabelDeleteProtected] = LabelDeleteProtectedValue
	obj.SetLabels(l)
}

// IsDeleteProtected reports whether obj carries the exact delete-protected key=value pair. This is the
// only predicate the delete-guard admission and the backfill inventory rely on.
func IsDeleteProtected(obj metav1.Object) bool {
	return obj.GetLabels()[LabelDeleteProtected] == LabelDeleteProtectedValue
}

// LabelProvenance records WHO created the object, and states nothing else about it. Present with value
// LabelProvenanceUnifiedSnapshot, it means the unified snapshot write-path built this object as a node of
// a snapshot tree; absent, it means the object came from elsewhere — a user, a foreign controller — and
// this module is merely an observer of it.
//
// Origin, permission to manage, and actually managing are three separate statements, and this label is
// only the first: it never by itself makes an object protected or managed. Because only a write-path can
// know an object's origin, the label MUST be stamped by the object's CREATOR into the CREATE payload, and
// MUST NOT be inferred by an observer from anything it can see about an object that already exists.
// Conversely, an object without it MUST NOT be mutated by this module at all — not even with a negative
// marker.
//
// Restoring it on an object the write-path itself already controls is not such an inference: there the
// creating parent is re-stating a fact it established when it created the object, and the label may be absent
// only because the object predates the label or someone stripped it (unlike the protection state, this one is
// not immutable at admission).
//
// The value is a keyed origin rather than a boolean so that a second origin can be expressed later
// without introducing a second label.
//
// This is the single exported source of truth for the key, reused by the core, the SDK and domain
// controllers; there must be no hardcoded string duplicates.
const LabelProvenance = APIGroup + "/provenance"

// LabelProvenanceUnifiedSnapshot is the only origin this module stamps: created by the unified snapshot
// write-path as a node of a snapshot tree. Provenance is keyed on the exact key=value pair; any other
// value means an origin this module does not recognise, which is not its own.
const LabelProvenanceUnifiedSnapshot = "unified-snapshot"

// StampProvenance sets the provenance label on obj's labels (allocating the map if nil). Write-paths MUST
// call it while BUILDING the object, in the same payload that carries the object's owner reference and its
// protection state: origin, ownership and protection of a tree node are one atomic fact, and a follow-up
// patch would leave a window in which the object exists without being recognisable as ours. It is
// idempotent.
func StampProvenance(obj metav1.Object) {
	l := obj.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	l[LabelProvenance] = LabelProvenanceUnifiedSnapshot
	obj.SetLabels(l)
}

// HasProvenance reports whether obj carries the exact provenance key=value pair — the unified-snapshot
// origin, which under this contract only the object's creator writes. It is the discriminator for the normal
// write-path, and a necessary condition for taking an object into this module's lifecycle, never a sufficient
// one: consumers pair it with the ownership shape the object is expected to have as a structural consistency
// check, and fail closed when the two disagree.
//
// Neither this label nor an owner reference is an authorization boundary — both are writable by anyone who
// can create the object. They rule out accidentally taking in an unrelated controller's object, which is what
// this module needs; enforcement against a deliberate forgery would have to come from admission or RBAC.
func HasProvenance(obj metav1.Object) bool {
	return obj.GetLabels()[LabelProvenance] == LabelProvenanceUnifiedSnapshot
}
