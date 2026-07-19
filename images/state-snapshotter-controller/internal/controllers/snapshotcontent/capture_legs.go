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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/manifestcapture"
	vcctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
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
// Recovery reap (idempotent): the latch-and-reap for each request leg is gated on the leg latch being
// false, so once the latch is written that block never runs again. If a pass crashes, requeues, or hits a
// transient API error in the window BETWEEN the latch write and the request delete, the request would be
// orphaned forever (swept only by its TTL, and a leftover VCR also blocks namespace deletion). Each
// request leg therefore ALSO reaps its request when the latch is already true (a prior pass latched but
// did not finish the delete): this is safe and NOT churn because the domain SDK suppresses request
// re-creation whenever the leg latch is true (EnsureVolumeCapture/EnsureManifestCapture), so
// latch-before-reap still holds (the latch happened in the earlier pass). The domain never clears the
// request name, so this recovery path runs on every reconcile of a completed content — it therefore
// probes existence via the CACHE first (both MCR and VCR are watched by this controller) and only a
// still-present leftover pays a Delete; an already-reaped leg is a cheap cache hit with no API round-trip.
// (The reap helpers delete by key without a pre-read: Delete needs only the GVK+name and carries no
// resourceVersion precondition, so a Get before it would add no safety — see reapVolumeCaptureRequest.)
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
	} else if mcrName := ownerDomainCaptureStateString(owner, "manifestCaptureRequestName"); mcrName != "" {
		// Recovery reap (idempotent): manifestCaptured was latched in a PRIOR pass, but the domain MCR still
		// exists. The latch-and-reap above is gated on !manifestCaptured, so once the latch is set it never
		// runs again — a crash, requeue, or transient API error in the window between the latch write and the
		// MCR delete would otherwise orphan the MCR forever (swept only by its TTL, and it also blocks
		// namespace deletion). Reaping here is safe and NOT churn: the domain SDK suppresses MCR re-creation
		// whenever manifestCaptured is true (EnsureManifestCapture), so latch-before-reap still holds (the
		// latch happened in the earlier pass).
		//
		// CACHED existence probe first: the domain never clears the MCR name, so this branch runs on every
		// reconcile of a completed content. The content controller watches ManifestCaptureRequest, so an
		// already-reaped leg is a cheap cache hit with NO API round-trip; only a still-present leftover pays
		// the Delete inside reapManifestCaptureRequest.
		key := client.ObjectKey{Namespace: namespace, Name: mcrName}
		cachedMCR := &ssv1alpha1.ManifestCaptureRequest{}
		switch err := r.Get(ctx, key, cachedMCR); {
		case errors.IsNotFound(err):
			// Already reaped: no-op.
		case err != nil:
			return false, err
		default:
			if dErr := r.reapManifestCaptureRequest(ctx, key); dErr != nil {
				return false, dErr
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
	} else if vcrName := ownerDomainCaptureStateString(owner, "volumeCaptureRequestName"); vcrName != "" {
		// Recovery reap (idempotent): dataCaptured was latched in a PRIOR pass, but the domain VCR still
		// exists. The latch-and-reap above is gated on !dataCaptured, so once the latch is set it never runs
		// again — a crash, requeue, or transient API error in the window between the latch write and the VCR
		// delete would otherwise orphan the VCR forever (swept only by its 10m TTL, and it also blocks
		// namespace deletion). Reaping here is safe and NOT churn: the domain SDK suppresses VCR re-creation
		// whenever dataCaptured is true (EnsureVolumeCapture), so latch-before-reap still holds (the latch
		// happened in the earlier pass).
		//
		// CACHED existence probe first: the domain never clears the VCR name, so this branch runs on every
		// reconcile of a completed content. The content controller event-driven-watches VCR
		// (AddVolumeCaptureRequestWatch, same cache observeOwnerDataLegVCR reads), so an already-reaped leg
		// is a cheap cache hit with NO API round-trip; only a still-present leftover pays the Delete inside
		// reapVolumeCaptureRequest.
		key := client.ObjectKey{Namespace: namespace, Name: vcrName}
		cachedVCR := &unstructured.Unstructured{}
		cachedVCR.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
		switch err := r.Get(ctx, key, cachedVCR); {
		case errors.IsNotFound(err):
			// Already reaped: no-op.
		case err != nil:
			return false, err
		default:
			if delErr := r.reapVolumeCaptureRequest(ctx, key); delErr != nil {
				return false, delErr
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

	// childrenSettled (main-owned, snapshot-native, monotonic): true once EVERY DIRECT child has gone
	// terminal — captured-OK OR failed. Unlike subtreePlanned (a success/planning latch) it counts a terminal
	// child FAILURE as settled, so it is a completeness signal ORTHOGONAL to success: a domain reads it to
	// time a barrier-2 action (e.g. fs unfreeze) that must fire even when a child data snapshot failed. A leaf
	// (no declared children) never declares the latch (nil = nothing to settle). While any direct child is
	// still non-terminal — or not created yet — the owner does not latch and requeues, so the 500 ms
	// self-requeue re-evaluates as children go terminal (a child's Ready/phase write also wakes its bound
	// content, which re-mirrors and re-drives this owner). The direct-child set is frozen at barrier 1
	// (guaranteed past ownerDomainCaptureAtLeastPlanned above), and NotFound children are fail-closed, so the
	// latch never flips true over an incomplete set.
	if !ownerCommonLegCaptured(owner, "childrenSettled") {
		settled, hasChildren, csErr := r.allDirectChildrenSettled(ctx, owner)
		if csErr != nil {
			return false, csErr
		}
		if hasChildren {
			if settled {
				if mErr := r.setOwnerCaptureLegCaptured(ctx, owner, "childrenSettled"); mErr != nil {
					return false, mErr
				}
			} else {
				requeue = true
			}
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

// allDirectChildrenSettled reports whether every DIRECT child declared on the owner's
// status.childrenSnapshotRefs has gone terminal (childSnapshotSettled). It mirrors
// allDirectChildrenSubtreePlanned: the direct-child set is frozen at capture barrier 1 (the caller only runs
// past phase>=Planned, when childrenSnapshotRefs is set-once), and a declared-but-not-yet-created child
// (NotFound) reads as not-settled (fail-closed), so childrenSettled never latches true over an incomplete
// child set — the same fail-closed-until-frozen discipline ChildrenReady uses. hasChildren=false means a leaf
// node (no declared children): the caller leaves the latch nil, since a leaf has nothing to settle. Children
// are read from the owner's fresh status; each is resolved by its ref GVK in the owner's namespace.
func (r *SnapshotContentController) allDirectChildrenSettled(ctx context.Context, owner *unstructured.Unstructured) (settled bool, hasChildren bool, err error) {
	refs, found, err := unstructured.NestedSlice(owner.Object, "status", "childrenSnapshotRefs")
	if err != nil {
		return false, false, err
	}
	if !found || len(refs) == 0 {
		return false, false, nil
	}
	namespace := owner.GetNamespace()
	for _, raw := range refs {
		m, ok := raw.(map[string]interface{})
		if !ok {
			return false, true, fmt.Errorf("owner %s/%s has a malformed childrenSnapshotRefs entry %T", namespace, owner.GetName(), raw)
		}
		apiVersion, _ := m["apiVersion"].(string)
		kind, _ := m["kind"].(string)
		name, _ := m["name"].(string)
		if apiVersion == "" || kind == "" || name == "" {
			return false, true, fmt.Errorf("owner %s/%s has an incomplete childrenSnapshotRefs entry %v", namespace, owner.GetName(), m)
		}
		gv, gvErr := schema.ParseGroupVersion(apiVersion)
		if gvErr != nil {
			return false, true, gvErr
		}
		child := &unstructured.Unstructured{}
		child.SetGroupVersionKind(gv.WithKind(kind))
		if gErr := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, child); gErr != nil {
			if errors.IsNotFound(gErr) {
				// Declared but not created yet: not settled (fail-closed).
				return false, true, nil
			}
			return false, true, gErr
		}
		if !childSnapshotSettled(child) {
			return false, true, nil
		}
	}
	return true, true, nil
}

// childSnapshotSettled reports whether a child snapshot has gone terminal for childrenSettled purposes —
// captured-OK OR failed. Terminal = child Ready==True, OR domain phase in {Finished,Failed}, OR a terminal
// Ready=False reason (IsReasonTerminal). Both terminal channels are read explicitly:
//
//   - The domain phase=Failed is read DIRECTLY off status.captureState.domainSpecificController.phase. The
//     core bubbles a domain's FREE-FORM phase=Failed reason VERBATIM onto the child Ready (ready_mirror.go),
//     and a free-form domain reason is not in TerminalReadyReasons, so IsReasonTerminal(Ready) alone would
//     miss a domain failure (e.g. a consistency-deadline reject). Reading phase catches it. phase=Finished
//     (barrier 2, domain done) is the settled-OK domain channel.
//   - The core-derived terminals a child's data-leg or subtree failure surfaces on its Ready
//     (VolumeCaptureFailed, ChildrenFailed, ...) are caught by the IsReasonTerminal channel.
//
// Ready==True is the plain captured-OK channel (e.g. a manifest-only leaf child). No observedGeneration gate
// is needed: the snapshot spec is immutable (no recapture) and the latch is monotonic.
func childSnapshotSettled(child *unstructured.Unstructured) bool {
	switch storagev1alpha1.SnapshotCapturePhase(ownerDomainCapturePhase(child)) {
	case storagev1alpha1.SnapshotCapturePhaseFinished, storagev1alpha1.SnapshotCapturePhaseFailed:
		return true
	}
	rc := usecase.CurrentReadyCondition(child)
	if rc == nil {
		return false
	}
	if rc.Status == metav1.ConditionTrue {
		return true
	}
	return rc.Status == metav1.ConditionFalse && storagev1alpha1.IsReasonTerminal(rc.Reason)
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

// reapManifestCaptureRequest deletes the domain MCR by key after its leg latch is set (latch-before-reap).
// It deletes by name WITHOUT a pre-read: the caller has already established the MCR should go (the
// authoritative uncached safety check in the latch path, or the cached existence probe in the recovery
// path), and Delete carries no resourceVersion/UID precondition — so a prior Get would neither add safety
// nor guard against a re-created same-name object, it would only be a redundant API round-trip. NotFound is
// success (already reaped).
func (r *SnapshotContentController) reapManifestCaptureRequest(ctx context.Context, key client.ObjectKey) error {
	mcr := &ssv1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
	}
	if err := r.Delete(ctx, mcr); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

// reapVolumeCaptureRequest deletes the domain VCR by key after its leg latch is set (latch-before-reap).
// Delete-by-name, no pre-read — same rationale as reapManifestCaptureRequest. NotFound is success.
func (r *SnapshotContentController) reapVolumeCaptureRequest(ctx context.Context, key client.ObjectKey) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	obj.SetNamespace(key.Namespace)
	obj.SetName(key.Name)
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
