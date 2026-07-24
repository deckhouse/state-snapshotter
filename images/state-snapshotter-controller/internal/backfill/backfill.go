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

// Package backfill is the ONE-SHOT, cluster-wide list-and-patch migration that stamps the authoritative
// delete-protection marker (api/storage/v1alpha1.LabelDeleteProtected) onto legacy unified-snapshot nodes
// created before the write-path started stamping it (delete-protection-contract.md §7; plan P3).
//
// It is the rollout GATE for switching the admission delete-guard from Audit to Deny: only once a full
// pass proves that every object the classifier considers ours already carries the marker is strict Deny
// safe. Two invariants make it trustworthy:
//
//   - Idempotent BY CONTRACT: running it any number of times converges — a second run patches nothing,
//     touches neither already-marked nodes nor foreign objects. This is required because operators re-run
//     it after a rollback, a partial upgrade, or a classifier fix.
//   - Provable gate: the gate is "every classifier-protected object HAS the marker" (a verify pass finds
//     zero of OUR objects without it), NOT the unprovable "no uncovered object exists anywhere".
//
// The per-Kind classifier is the ONLY place legacy provenance signals (deterministic names, controller
// ownerRefs to our kinds, the managed label) are read. Admission never reads them (§7, P9). Classifiers
// fail closed: when a signal is ambiguous for a SHARED kind (ObjectKeeper / CSI VolumeSnapshot(Content)),
// the object is treated as NOT ours so the backfill never marks a foreign object.
package backfill

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// Classifier reports whether a legacy object is a unified-snapshot node that must be delete-protected. It
// is migration-only provenance logic; it MUST fail closed (return false) on ambiguity for shared kinds.
type Classifier func(obj *unstructured.Unstructured) bool

// Target binds a protected object GVK to its classifier.
type Target struct {
	GVK    schema.GroupVersionKind
	IsOurs Classifier
}

// KindReport is the per-Kind outcome of a single pass.
type KindReport struct {
	GVK            schema.GroupVersionKind
	Skipped        bool // CRD not installed / kind not served — tolerated
	Total          int  // objects listed
	Ours           int  // classifier-protected objects
	Patched        int  // markers written this pass (0 on a verify pass)
	OursUnmarked   int  // classifier-protected objects still WITHOUT the marker after this pass
	AlreadyProtect int  // objects that already carried the marker
}

// Report aggregates one pass over all targets.
type Report struct {
	Kinds []KindReport
}

// OursUnmarkedTotal is the gate metric: the number of classifier-protected objects still lacking the
// marker. The rollout gate to Deny is OursUnmarkedTotal == 0 on a verify pass.
func (r Report) OursUnmarkedTotal() int {
	var n int
	for _, k := range r.Kinds {
		n += k.OursUnmarked
	}
	return n
}

// PatchedTotal is the number of markers written across all kinds in this pass.
func (r Report) PatchedTotal() int {
	var n int
	for _, k := range r.Kinds {
		n += k.Patched
	}
	return n
}

// Apply runs one list-and-patch pass: it stamps the marker on every classifier-protected object that lacks
// it. It is safe to call repeatedly (idempotent).
func Apply(ctx context.Context, cl client.Client, targets []Target) (Report, error) {
	return pass(ctx, cl, targets, true)
}

// Verify runs one read-only pass (no writes) and reports how many classifier-protected objects still lack
// the marker. Report.OursUnmarkedTotal() == 0 is the provable rollout gate for strict Deny.
func Verify(ctx context.Context, cl client.Client, targets []Target) (Report, error) {
	return pass(ctx, cl, targets, false)
}

func pass(ctx context.Context, cl client.Client, targets []Target, apply bool) (Report, error) {
	var report Report
	for _, t := range targets {
		kr := KindReport{GVK: t.GVK}

		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   t.GVK.Group,
			Version: t.GVK.Version,
			Kind:    t.GVK.Kind + "List",
		})
		if err := cl.List(ctx, list); err != nil {
			// A protected Kind whose CRD/API is not installed on this cluster is not an error: the
			// backfill simply has nothing to do for it. Everything else is a real failure.
			if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				kr.Skipped = true
				report.Kinds = append(report.Kinds, kr)
				continue
			}
			return report, fmt.Errorf("list %s: %w", t.GVK.Kind, err)
		}

		for i := range list.Items {
			obj := &list.Items[i]
			kr.Total++
			protected := storagev1alpha1.IsDeleteProtected(obj)
			if protected {
				kr.AlreadyProtect++
			}
			if !t.IsOurs(obj) {
				continue
			}
			kr.Ours++
			if protected {
				continue
			}
			// Classifier-protected object without the marker.
			if !apply {
				kr.OursUnmarked++
				continue
			}
			orig := obj.DeepCopy()
			storagev1alpha1.StampDeleteProtected(obj)
			if err := cl.Patch(ctx, obj, client.MergeFrom(orig)); err != nil {
				return report, fmt.Errorf("patch %s %s/%s: %w", t.GVK.Kind, obj.GetNamespace(), obj.GetName(), err)
			}
			kr.Patched++
		}
		report.Kinds = append(report.Kinds, kr)
	}
	return report, nil
}
