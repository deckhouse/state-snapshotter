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

package snapshotcontent

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/manifestcapture"
	vcctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// reconcileOwnerCaptureLegs runs the main-owned capture-leg lifecycle for the content's owning
// xxxSnapshot (main-owned commonController, content-single-writer design §2/§3, decision #10):
//
//   - eager-init: declare the applicable core-owned capture legs on the owner
//     (commonController.manifestCaptured=false always; dataCaptured=false for data-artifact kinds) so
//     the domain SDK can distinguish "not started yet" from "nothing to wait for" (CoreCaptureOutcome);
//   - manifest leg: once the MCP handoff is durable (content.status.manifestCheckpointName published +
//     MCP owned by the content), latch commonController.manifestCaptured=true on the owner and REAP the
//     domain MCR — latch strictly BEFORE the delete, in the same pass (see below);
//   - data leg (VCR domains): observe the domain VCR; once the aggregator's published status.data covers
//     the VCR targets and the VSC handoff is durable, latch commonController.dataCaptured=true and reap
//     the VCR. A FAILED VCR is NOT surfaced here anymore: core makes the CONTENT terminal in
//     reconcileDataLegProjection (DataReady=VolumeCaptureFailed, decision D2), which the mirror reflects
//     onto the owning snapshot and which propagates up the content tree; the leg just does not latch;
//   - data leg (native-CSI VolumeSnapshot owners, design §11.4): no VCR — latch dataCaptured once the
//     content carries a published status.data (the projection performs the VSC handoff first);
//   - subtreeManifestsPersisted: mirror the content's monotonic recursive latch onto the owner's
//     commonController (true-only), so parent domains read the manifest-exclude pre-gate namespaced.
//
// Latch-before-reap invariant: the domain SDK, when it finds an MCR/VCR absent, does an authoritative
// UNCACHED read of the leg latch on the snapshot and re-creates the request if the latch is false
// (pkg/snapshotsdk capture.go). The latch write therefore precedes the request delete, by this same
// actor, in one pass — a latch-after-delete (or a cross-controller mirror hop) would open a window in
// which the domain re-creates a request that was just reaped (churn).
//
// The owner is resolved from content.spec.snapshotRef and the step only runs once the owner has adopted
// THIS content (status.boundSnapshotContentName == content name) and its domain reached capture
// barrier 1 (phase>=Planned) — same writer-switch and projection barrier the binder used. Ownerless /
// recycle-bin content, non-domain-capture owners, and pre-Planned owners are no-ops.
func (r *SnapshotContentController) reconcileOwnerCaptureLegs(
	ctx context.Context,
	contentObj *unstructured.Unstructured,
) (requeue bool, err error) {
	apiVersion, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "apiVersion")
	kind, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "kind")
	namespace, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "namespace")
	name, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "name")
	if apiVersion == "" || kind == "" || name == "" {
		// Ownerless / bucket content: no owning snapshot, no legs to run.
		return false, nil
	}
	gv, gvErr := schema.ParseGroupVersion(apiVersion)
	if gvErr != nil {
		return false, fmt.Errorf("parse snapshotRef.apiVersion %q: %w", apiVersion, gvErr)
	}
	ownerGVK := gv.WithKind(kind)
	if !r.isDomainCaptureKind(ownerGVK) {
		// Non-domain owners (import handles, generic kinds) have no core-owned capture legs.
		return false, nil
	}

	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(ownerGVK)
	if getErr := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, owner); getErr != nil {
		if errors.IsNotFound(getErr) {
			// Recycle-bin content: the owner is gone; nothing to latch or reap.
			return false, nil
		}
		return false, fmt.Errorf("get owner snapshot %s/%s: %w", namespace, name, getErr)
	}

	// Writer switch (creator -> main): only touch the owner once it has adopted THIS content.
	bound, _, _ := unstructured.NestedString(owner.Object, "status", "boundSnapshotContentName")
	if bound != contentObj.GetName() {
		return false, nil
	}
	// Projection barrier: before capture barrier 1 (phase>=Planned) there is nothing to init or reap —
	// same gate the binder's eagerInitCaptureLegs + request lifecycle had.
	if !ownerDomainCaptureAtLeastPlanned(owner) {
		return false, nil
	}

	// Eager-init the applicable legs (presence declares the leg; value = captured or not).
	if initErr := r.eagerInitOwnerCaptureLegs(ctx, owner); initErr != nil {
		return false, initErr
	}

	contentName := contentObj.GetName()

	// Manifest leg: latch + reap once the MCP handoff is durable.
	if !ownerCommonLegCaptured(owner, "manifestCaptured") {
		mcrName := ownerDomainCaptureStateString(owner, "manifestCaptureRequestName")
		if mcrName != "" {
			safe, sErr := manifestcapture.ManifestCaptureRequestSafeToDelete(ctx, r.APIReader,
				client.ObjectKey{Namespace: namespace, Name: mcrName}, contentName)
			if sErr != nil {
				return false, sErr
			}
			if safe {
				if mErr := r.setOwnerCaptureLegCaptured(ctx, owner, "manifestCaptured"); mErr != nil {
					return false, mErr
				}
				if dErr := r.reapManifestCaptureRequest(ctx, client.ObjectKey{Namespace: namespace, Name: mcrName}); dErr != nil {
					return false, dErr
				}
			}
		}
	}

	// Data leg.
	if !ownerCommonLegCaptured(owner, "dataCaptured") {
		vcrName := ownerDomainCaptureStateString(owner, "volumeCaptureRequestName")
		switch {
		case vcrName != "":
			done, dErr := r.observeOwnerDataLegVCR(ctx, namespace, vcrName, contentName)
			if dErr != nil {
				return false, dErr
			}
			if !done {
				// Not captured yet (pending, or a failed VCR that core surfaced as a terminal content in
				// reconcileDataLegProjection): do not latch. The content stays Ready=False (terminal or
				// pending), so the general !ready self-requeue keeps this converging.
				requeue = true
				break
			}
			vcrKey := client.ObjectKey{Namespace: namespace, Name: vcrName}
			// Pre-reap safety read stays AUTHORITATIVE (uncached APIReader): it gates the VCR Delete
			// (latch-before-reap), so it must observe the live handoff state, not a possibly-stale cache.
			safe, sErr := vcctrl.VolumeCaptureRequestSafeToDeleteWithHandoff(ctx, r.APIReader, vcrKey, contentName)
			if sErr != nil {
				return false, sErr
			}
			if !safe {
				requeue = true
				break
			}
			if mErr := r.setOwnerCaptureLegCaptured(ctx, owner, "dataCaptured"); mErr != nil {
				return false, mErr
			}
			if delErr := r.reapVolumeCaptureRequest(ctx, vcrKey); delErr != nil {
				return false, delErr
			}
		case ownerGVK.Kind == snapshot.KindVolumeSnapshot:
			// Native-CSI data leg (design §11.4): a VolumeSnapshot owner has NO VCR — the fork binds it to
			// a VSC and the data projection publishes content.status.data only after the Retain + ownerRef
			// handoff, so data-present ⇒ handoff durable. No request to reap.
			hasData, hErr := r.contentHasPublishedData(ctx, contentName)
			if hErr != nil {
				return false, hErr
			}
			if hasData {
				if mErr := r.setOwnerCaptureLegCaptured(ctx, owner, "dataCaptured"); mErr != nil {
					return false, mErr
				}
			} else {
				requeue = true
			}
		}
	}

	// subtreeManifestsPersisted mirror (true-only, monotonic): content latch -> owner commonController.
	if persisted, found, _ := unstructured.NestedBool(contentObj.Object, "status", "subtreeManifestsPersisted"); found && persisted {
		if !ownerCommonLegCaptured(owner, "subtreeManifestsPersisted") {
			if mErr := r.setOwnerCaptureLegCaptured(ctx, owner, "subtreeManifestsPersisted"); mErr != nil {
				return false, mErr
			}
		}
	}

	// subtreePlanned (main-owned, snapshot-native, monotonic; design §8.1, decision #10): this node is
	// planned (guaranteed past ownerDomainCaptureAtLeastPlanned above) AND every DIRECT child's own
	// subtreePlanned latch is set. Latch true only when the whole direct-child set is planned; while any
	// child is still pending, requeue so the 500 ms self-requeue re-evaluates as children latch bottom-up
	// (a child's own subtreePlanned write on its snapshot also wakes its bound content via the snapshot
	// status watch). The root's orphan wave reads its children's latch as the wave gate (parent_graph.go).
	if !ownerCommonLegCaptured(owner, "subtreePlanned") {
		allPlanned, spErr := r.allDirectChildrenSubtreePlanned(ctx, owner)
		if spErr != nil {
			return false, spErr
		}
		if allPlanned {
			if mErr := r.setOwnerCaptureLegCaptured(ctx, owner, "subtreePlanned"); mErr != nil {
				return false, mErr
			}
		} else {
			requeue = true
		}
	}

	return requeue, nil
}

// allDirectChildrenSubtreePlanned reports whether every DIRECT child declared on the owner's
// status.childrenSnapshotRefs carries its own commonController.subtreePlanned=true (the recursive
// planning latch). A node with no declared children is vacuously planned (its own phase>=Planned is
// already gated by the caller). A child that is not created yet, or whose latch is not yet set, makes the
// owner's subtree "not planned yet" (the caller requeues). Children are read from the owner's fresh
// status (published by capture barrier 1); each is resolved by its ref GVK in the owner's namespace.
func (r *SnapshotContentController) allDirectChildrenSubtreePlanned(ctx context.Context, owner *unstructured.Unstructured) (bool, error) {
	refs, found, err := unstructured.NestedSlice(owner.Object, "status", "childrenSnapshotRefs")
	if err != nil {
		return false, err
	}
	if !found || len(refs) == 0 {
		return true, nil
	}
	namespace := owner.GetNamespace()
	for _, raw := range refs {
		m, ok := raw.(map[string]interface{})
		if !ok {
			return false, fmt.Errorf("owner %s/%s has a malformed childrenSnapshotRefs entry %T", namespace, owner.GetName(), raw)
		}
		apiVersion, _ := m["apiVersion"].(string)
		kind, _ := m["kind"].(string)
		name, _ := m["name"].(string)
		if apiVersion == "" || kind == "" || name == "" {
			return false, fmt.Errorf("owner %s/%s has an incomplete childrenSnapshotRefs entry %v", namespace, owner.GetName(), m)
		}
		gv, gvErr := schema.ParseGroupVersion(apiVersion)
		if gvErr != nil {
			return false, gvErr
		}
		child := &unstructured.Unstructured{}
		child.SetGroupVersionKind(gv.WithKind(kind))
		if gErr := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, child); gErr != nil {
			if errors.IsNotFound(gErr) {
				// Declared but not created yet: the subtree is not planned.
				return false, nil
			}
			return false, gErr
		}
		if !ownerCommonLegCaptured(child, "subtreePlanned") {
			return false, nil
		}
	}
	return true, nil
}

// observeOwnerDataLegVCR observes the domain-created VolumeCaptureRequest to drive the main-owned VCR
// lifecycle (moved off the binder, decision #10). It is READ-ONLY on the SnapshotContent — enrich + VSC
// handoff + status.data publish live in the data-leg projection. Its ONLY job is the success latch: report
// done=true once the published status.data covers the VCR targets, so the caller may latch dataCaptured
// and reap the VCR. Otherwise it returns done=false (pending) and the caller requeues.
//
// It no longer surfaces a terminal capture failure (vcr-watch-core-terminal, decision D2): a failed VCR
// (and the Variant-A >1-artifact fault) is made terminal on the CONTENT itself by reconcileDataLegProjection
// (DataReady=VolumeCaptureFailed), which the mirror reflects onto the owning snapshot and which propagates
// up the content tree. On a failed VCR the leg simply stays not-captured (done=false) and never latches.
func (r *SnapshotContentController) observeOwnerDataLegVCR(
	ctx context.Context,
	namespace string,
	vcrName string,
	contentName string,
) (done bool, err error) {
	vcr := &unstructured.Unstructured{}
	vcr.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	// Cached read: the content controller event-driven-watches VCR (AddVolumeCaptureRequestWatch), so a VCR
	// status change enqueues this content and the informer cache is authoritative-enough for the success
	// latch. The pre-reap safety read below (VolumeCaptureRequestSafeToDeleteWithHandoff) stays uncached.
	if getErr := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: vcrName}, vcr); getErr != nil {
		if errors.IsNotFound(getErr) {
			// Domain controller has not (re)created the VCR yet; wait for it.
			return false, nil
		}
		return false, getErr
	}

	expectedTargets, parseErr := vcctrl.ParseVolumeCaptureTargets(vcr)
	if parseErr != nil {
		return false, parseErr
	}

	content := &storagev1alpha1.SnapshotContent{}
	if cErr := r.APIReader.Get(ctx, client.ObjectKey{Name: contentName}, content); cErr != nil {
		return false, cErr
	}
	if vcctrl.ContentDataRefsCoverExpectedTargets(content.DataList(), expectedTargets) {
		// The projection has published status.data covering the targets: the leg is durable.
		return true, nil
	}

	// Not covered yet: pending. A failed/inconsistent VCR is handled terminally on the content by the
	// projection, so there is nothing to latch here — just report not-captured.
	return false, nil
}

// contentHasPublishedData reports whether the content carries a published status.data binding (the
// projection publishes it only after the VSC handoff succeeds, so data-present ⇒ handoff durable).
func (r *SnapshotContentController) contentHasPublishedData(ctx context.Context, contentName string) (bool, error) {
	content := &storagev1alpha1.SnapshotContent{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		return false, err
	}
	return content.Status.Data != nil, nil
}

// eagerInitOwnerCaptureLegs declares the applicable core-owned capture legs on the owner snapshot:
// commonController.manifestCaptured=false (every capture node has a manifest leg) and, for data-artifact
// kinds, commonController.dataCaptured=false. Presence of the field declares the leg (nil = no leg); the
// leg is later monotonically flipped true by setOwnerCaptureLegCaptured. This lets the SDK distinguish
// "not started yet" from "nothing to wait for" when computing CoreCaptureOutcome. Sideways write onto the
// owner (main-owned commonController, decision #10) under an optimistic-lock merge patch: the domain
// co-writes domainSpecificController in the same status, so a concurrent write yields 409 and the retry
// re-reads.
func (r *SnapshotContentController) eagerInitOwnerCaptureLegs(ctx context.Context, owner *unstructured.Unstructured) error {
	requiresDataArtifact := r.GVKRegistry.RequiresDataArtifact(owner.GetObjectKind().GroupVersionKind().Kind)
	gvk := owner.GetObjectKind().GroupVersionKind()
	key := client.ObjectKey{Namespace: owner.GetNamespace(), Name: owner.GetName()}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(gvk)
		if err := r.APIReader.Get(ctx, key, fresh); err != nil {
			return err
		}
		base := fresh.DeepCopy()
		changed := false
		if _, found, _ := unstructured.NestedBool(fresh.Object, "status", "captureState", "commonController", "manifestCaptured"); !found {
			if err := unstructured.SetNestedField(fresh.Object, false, "status", "captureState", "commonController", "manifestCaptured"); err != nil {
				return err
			}
			changed = true
		}
		if requiresDataArtifact {
			if _, found, _ := unstructured.NestedBool(fresh.Object, "status", "captureState", "commonController", "dataCaptured"); !found {
				if err := unstructured.SetNestedField(fresh.Object, false, "status", "captureState", "commonController", "dataCaptured"); err != nil {
					return err
				}
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return r.Status().Patch(ctx, fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

// setOwnerCaptureLegCaptured monotonically flips a core-owned capture-leg success latch
// (status.captureState.commonController.<leg>) to true on the owner snapshot, under an optimistic-lock
// merge patch + conflict retry. It MUST be called before the corresponding request is reaped so the
// domain SDK (which suppresses re-creation on the latch via an authoritative uncached read) never
// re-creates it. Main owns commonController (decision #10); the domain-owned request-name
// (domainSpecificController) is never touched here (single-writer per sub-structure).
func (r *SnapshotContentController) setOwnerCaptureLegCaptured(ctx context.Context, owner *unstructured.Unstructured, leg string) error {
	gvk := owner.GetObjectKind().GroupVersionKind()
	key := client.ObjectKey{Namespace: owner.GetNamespace(), Name: owner.GetName()}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(gvk)
		if err := r.APIReader.Get(ctx, key, fresh); err != nil {
			return err
		}
		if ownerCommonLegCaptured(fresh, leg) {
			return nil
		}
		base := fresh.DeepCopy()
		if err := unstructured.SetNestedField(fresh.Object, true, "status", "captureState", "commonController", leg); err != nil {
			return err
		}
		return r.Status().Patch(ctx, fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

// reapManifestCaptureRequest deletes the domain MCR after its leg latch is set (latch-before-reap).
// NotFound is success (already reaped).
func (r *SnapshotContentController) reapManifestCaptureRequest(ctx context.Context, key client.ObjectKey) error {
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	if err := r.APIReader.Get(ctx, key, mcr); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err := r.Delete(ctx, mcr); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

// reapVolumeCaptureRequest deletes the domain VCR after its leg latch is set (latch-before-reap).
// NotFound is success (already reaped).
func (r *SnapshotContentController) reapVolumeCaptureRequest(ctx context.Context, key client.ObjectKey) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	if err := r.APIReader.Get(ctx, key, obj); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err := r.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

// ownerCommonLegCaptured reports whether the core-written capture-leg success latch
// status.captureState.commonController.<leg> is present and true on the owner snapshot.
func ownerCommonLegCaptured(obj *unstructured.Unstructured, leg string) bool {
	v, found, _ := unstructured.NestedBool(obj.Object, "status", "captureState", "commonController", leg)
	return found && v
}

// ownerDomainCaptureStateString reads a string field from the owner's
// status.captureState.domainSpecificController (the domain-written half: MCR/VCR names, phase).
func ownerDomainCaptureStateString(obj *unstructured.Unstructured, field string) string {
	v, _, _ := unstructured.NestedString(obj.Object, "status", "captureState", "domainSpecificController", field)
	return v
}

// ownerDomainCaptureAtLeastPlanned reports whether the owner's domain reached capture barrier 1
// (phase Planned or Finished): objects created and refs published, so main may init/latch/reap the
// capture legs. Failed is intentionally excluded — a failed domain never re-plans (spec immutable), and
// reaping its requests is the TTL sweeper's job, not the capture handoff's.
func ownerDomainCaptureAtLeastPlanned(obj *unstructured.Unstructured) bool {
	switch storagev1alpha1.SnapshotCapturePhase(ownerDomainCapturePhase(obj)) {
	case storagev1alpha1.SnapshotCapturePhasePlanned, storagev1alpha1.SnapshotCapturePhaseFinished:
		return true
	default:
		return false
	}
}
