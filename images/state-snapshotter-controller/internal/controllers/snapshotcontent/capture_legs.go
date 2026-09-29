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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// reconcileOwnerCaptureLegs runs the main-owned capture-leg lifecycle for the content's owning
// xxxSnapshot (main-owned commonController):
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
//   - data leg (published status.data, any domain kind): latch dataCaptured once the content
//     carries a published status.data. Native-CSI VolumeSnapshot has no VCR; a wiped domain owner
//     whose volumeCaptureRequestName did not survive is the same path. The projection performs
//     the artifact handoff first, so data-present ⇒ durable.
//   - childSubtreesManifestsPersisted: eager-declare false, then monotonically latch true, the children-only
//     aggregate onto the owner's commonController — "the subtrees of ALL declared direct children are fully
//     persisted" — so a parent aggregator reads it namespaced as the SDK manifest-exclude pre-gate. It is
//     declared false (not left nil, which would disable the pre-gate) and EXCLUDES this node's own manifests,
//     so it can flip true before this node's own MCR exists (the chicken-and-egg is removed).
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

	// Declare every applicable core leg from the durable SnapshotContent, in ONE write: a leg the durable
	// artifact proves captured goes in as true, an applicable leg it does not prove goes in as false, and
	// they land together. Splitting this in two was wrong in both directions — the pass could publish a
	// false for a leg the artifact proves true, and between the two writes the owner carried one leg
	// latched with the other not declared at all, which a domain reads as a finished capture.
	//
	// settled=false means a leg the artifact proves captured did not survive the write (a schema that
	// prunes it, or a writer that replaced the status again). Nothing is declared in that case: the legs
	// stay absent, which the domain reads as "wait", and the next pass tries again. durable is the content
	// this pass read to decide all of it (nil when every leg was already latched and nothing was read).
	durable, settled, declErr := r.declareOwnerCaptureLegsFromDurable(ctx, owner, contentObj.GetName())
	if declErr != nil {
		return false, declErr
	}
	if !settled {
		return true, nil
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
		case r.GVKRegistry.RequiresDataArtifact(ownerGVK.Kind):
			// A data-artifact kind with no request name to observe: latch off the content's published
			// status.data, because the projection publishes it only after the artifact handoff, so
			// data-present ⇒ durable. Covers native-CSI VolumeSnapshot (which never has a VCR) and a
			// wiped domain owner whose volumeCaptureRequestName did not survive the replace.
			hasData, hErr := r.contentHasPublishedData(ctx, contentName, durable)
			if hErr != nil {
				return false, hErr
			}
			if !hasData {
				requeue = true
				break
			}
			if mErr := r.setOwnerCaptureLegCaptured(ctx, owner, "dataCaptured"); mErr != nil {
				return false, mErr
			}
		default:
			// Manifest-only kind: eager-init declares no data leg at all (absent = no leg), so there is
			// nothing to latch and nothing to wait for. Reading the content here would be a round-trip
			// per pass for every namespace/root snapshot in the cluster.
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

	// childSubtreesManifestsPersisted (eager-declared false, monotonic false -> true): the subtrees of ALL
	// declared direct children are fully persisted (each child node and its descendants durably archived
	// their manifests). This is the children-only aggregate — it deliberately EXCLUDES this node's own
	// manifests (tracked by manifestCaptured), so it can flip true BEFORE this node creates its own MCR,
	// which is what makes it usable as the SDK manifest-exclude pre-gate. It is DECLARED false as soon as the
	// node reaches this step (like the eager-init capture legs) and never left nil once past barrier 1 — a
	// nil field would silently disable the SDK pre-gate (nil = pre-gate off), so declaring false is what
	// keeps the pre-gate live. Value: this content's OWN subtreeManifestsPersisted latch ("node + all
	// descendants") being true IMPLIES the children-only aggregate is true (own persisted ⊇ children
	// persisted), so it short-circuits without a child walk; otherwise it is computed directly via
	// aggregateChildrenSubtreeManifestsPersisted (same declared-vs-linked fail-closed as the content Ready
	// gate; a childless node is vacuously true, latching true on the first pass). The setter is monotonic
	// (never downgrades a latched true), so the latch never flips true over a partial child set and never
	// re-opens. Not-persisted is not requeued here: a child content status change re-drives this owner
	// content, so the false declaration is re-evaluated and flips to true when the subtree persists.
	if !ownerCommonLegCaptured(owner, "childSubtreesManifestsPersisted") {
		childSubtreesPersisted := false
		if persisted, found, _ := unstructured.NestedBool(contentObj.Object, "status", "subtreeManifestsPersisted"); found && persisted {
			childSubtreesPersisted = true
		} else {
			aggregated, _, aErr := r.aggregateChildrenSubtreeManifestsPersisted(ctx, contentObj)
			if aErr != nil {
				return false, aErr
			}
			childSubtreesPersisted = aggregated
		}
		if mErr := r.setOwnerChildSubtreesManifestsPersisted(ctx, owner, childSubtreesPersisted); mErr != nil {
			return false, mErr
		}
	}

	// subtreePlanned (main-owned, snapshot-native, monotonic): this node is
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
// lifecycle (moved off the binder). It is READ-ONLY on the SnapshotContent — enrich + VSC
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
func (r *SnapshotContentController) contentHasPublishedData(ctx context.Context, contentName string, alreadyRead *storagev1alpha1.SnapshotContent) (bool, error) {
	// alreadyRead is this pass's uncached copy from the latch restore; re-reading it would buy nothing
	// but a second round-trip on every pass of an in-flight capture.
	if alreadyRead != nil {
		return alreadyRead.Status.Data != nil, nil
	}
	content := &storagev1alpha1.SnapshotContent{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		return false, err
	}
	return content.Status.Data != nil, nil
}

// declareOwnerCaptureLegsFromDurable brings the owner's core-owned capture legs
// (status.captureState.commonController) to what the durable SnapshotContent proves, in a single status
// patch: a leg the published artifact proves captured is written true, an applicable leg it does not prove
// is declared false, and an already-declared value is never overwritten. Presence of the field declares the
// leg; absence means the core has not spoken, which every domain reads as "wait", never as "capture it".
//
// One write, not two, and that is the whole point. Restoring a latch and declaring the remaining legs used
// to be separate patches, which was wrong in both directions:
//
//   - a restore that did not stick was followed by the declaration writing false for the very leg the
//     durable artifact proves captured — the intermediate false this recovery exists to prevent, and the
//     domain's cue to capture the same manifests again against a cluster that has moved on;
//   - between the two writes the owner carried one leg latched true with the other not declared at all,
//     and a domain reading that window sees every declared leg captured and publishes its terminal phase.
//     A terminal phase is never recomputed, so the node stops there for good.
//
// settled reports whether this pass may continue. It is false when any leg this pass wrote did not come
// back with the value it was written with — a structural schema that prunes the field, or a writer that
// replaced the status again in the meantime. Part of the write may well have landed; what is guaranteed is
// only that no leg the artifact proves captured was written false, so whatever is there reads as "captured"
// or as "not declared", and never as an invitation to capture it again. The caller requeues and the next
// pass recomputes the whole declaration from the same artifact.
//
// Only a leg that went from missing-or-false to a CONFIRMED true counts as a restore: it is the one that
// increments the metric and names itself in the event. A declaration is not a restore, and neither is a
// patch the apiserver accepted and then pruned — confirmation is an uncached Get after the write.
//
// It returns the content it read so the callers that need the same durable copy this pass do not pay a
// second uncached round-trip; nil means every leg was already latched and nothing was read at all — the
// steady state of a completed content, which reconciles on every watch event of its subtree.
func (r *SnapshotContentController) declareOwnerCaptureLegsFromDurable(
	ctx context.Context,
	owner *unstructured.Unstructured,
	contentName string,
) (*storagev1alpha1.SnapshotContent, bool, error) {
	requiresData := r.GVKRegistry != nil && r.GVKRegistry.RequiresDataArtifact(owner.GetObjectKind().GroupVersionKind().Kind)
	manifestLatched := ownerCommonLegCaptured(owner, "manifestCaptured")
	dataLatched := !requiresData || ownerCommonLegCaptured(owner, "dataCaptured")
	if manifestLatched && dataLatched {
		return nil, true, nil
	}

	content := &storagev1alpha1.SnapshotContent{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		return nil, false, err
	}

	// What the durable artifact proves, decided once from one read of one content — and decided about the
	// ARTIFACT alone, never conditioned on what the owner looked like when this reconcile started. The owner
	// read above is a cache-age snapshot; the object this pass actually writes is read fresh inside the
	// retry below, and a writer can replace the status between the two. Deriving the proof from the stale
	// copy meant a leg that was latched then and gone now had no proof attached to it, and the loop below
	// would take the only other branch open to it and declare the leg false — with the checkpoint that
	// proves it captured sitting right there in the content.
	//
	// The stale copy is therefore used for exactly one thing: the fast path above, where every leg is
	// already latched and there is nothing to decide. A wipe that lands after that check costs one more
	// reconcile, not a false.
	restoreManifest := false
	if content.Status.ManifestCheckpointName != "" {
		complete, cErr := r.publishedManifestHandoffComplete(ctx, content)
		if cErr != nil {
			return content, false, cErr
		}
		restoreManifest = complete
	}
	restoreData := requiresData && publishedDataIdentityFrozen(content.Status.Data)

	legs := []struct {
		name       string
		restore    bool
		applicable bool
	}{
		{name: "manifestCaptured", restore: restoreManifest, applicable: true},
		{name: "dataCaptured", restore: restoreData, applicable: requiresData},
	}

	type legWrite struct {
		name  string
		value bool
		// wasAbsent records that the leg carried no value at all when this pass read the owner. It is what
		// separates a restore from the ordinary latch: the core declares a leg (false) before the domain may
		// create its capture request, so on a healthy node the leg is always declared before it is captured,
		// and the false -> true step is that node finishing its capture. A leg that is ABSENT beside a
		// durable artifact is the signature of a status replace, and only that is worth an event.
		wasAbsent bool
	}
	gvk := owner.GetObjectKind().GroupVersionKind()
	key := client.ObjectKey{Namespace: owner.GetNamespace(), Name: owner.GetName()}
	var written []legWrite
	patched := false
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		written = nil
		patched = false
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(gvk)
		if err := r.APIReader.Get(ctx, key, fresh); err != nil {
			return err
		}
		base := fresh.DeepCopy()
		changed := false
		for _, leg := range legs {
			_, declared, _ := unstructured.NestedBool(fresh.Object, "status", "captureState", "commonController", leg.name)
			switch {
			case leg.restore && !ownerCommonLegCaptured(fresh, leg.name):
				if err := unstructured.SetNestedField(fresh.Object, true, "status", "captureState", "commonController", leg.name); err != nil {
					return err
				}
				written = append(written, legWrite{name: leg.name, value: true, wasAbsent: !declared})
				changed = true
			case leg.applicable && !declared:
				if err := unstructured.SetNestedField(fresh.Object, false, "status", "captureState", "commonController", leg.name); err != nil {
					return err
				}
				written = append(written, legWrite{name: leg.name, value: false})
				changed = true
			}
		}
		if !changed {
			owner.Object = fresh.Object
			return nil
		}
		if err := r.Status().Patch(ctx, fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		patched = true
		owner.Object = fresh.Object
		return nil
	}); err != nil {
		return content, false, err
	}
	if !patched {
		return content, true, nil
	}

	confirmed := &unstructured.Unstructured{}
	confirmed.SetGroupVersionKind(gvk)
	if err := r.APIReader.Get(ctx, key, confirmed); err != nil {
		return content, false, err
	}
	owner.Object = confirmed.Object

	// Every leg this pass wrote has to still be there, with the value it was written with — the declared
	// falses as much as the restored trues. One patch is what this code controls; whether the apiserver and
	// the writers around it keep all of it is not, and half of this write is a state that reads as something
	// it is not: a manifest leg latched beside a data leg that is simply absent looks, to anything that does
	// not know the kind, like a node with no data line and a finished capture.
	var restored []string
	settled := true
	for _, w := range written {
		value, declared, _ := unstructured.NestedBool(confirmed.Object, "status", "captureState", "commonController", w.name)
		if !declared || value != w.value {
			settled = false
			continue
		}
		// Only a leg that was missing is a restore. A declared false that this pass turned true is the
		// ordinary latch, and counting it would make the metric a capture counter with no way to tell the
		// two apart.
		if w.value && w.wasAbsent {
			restored = append(restored, w.name)
		}
	}
	if !settled {
		// Part of the write did not survive. Report it rather than carrying on: the next pass recomputes the
		// whole declaration from the same durable artifact and writes it again, and until then the missing
		// legs are absent, which every domain reads as "wait".
		return content, false, nil
	}
	if len(restored) == 0 {
		return content, true, nil
	}
	registerSnapshotContentMetrics()
	captureStateRestoredTotal.Inc()
	msg := "restored " + strings.Join(restored, ", ")
	if r.Recorder != nil {
		r.Recorder.Event(confirmed, corev1.EventTypeNormal, EventReasonCaptureStateRestored, msg)
	}
	return content, true, nil
}

// publishedManifestHandoffComplete reports whether content.status.manifestCheckpointName is a durable
// pointer that may be copied onto the owner's manifestCaptured latch. A Ready ManifestCheckpoint owned
// by the content is the completed-handoff case. A published name whose checkpoint is already gone is
// still durable (do not recapture). A name whose checkpoint exists but is not Ready / not owned is the
// in-flight first capture — restoring the latch then would reap a live ManifestCaptureRequest.
func (r *SnapshotContentController) publishedManifestHandoffComplete(ctx context.Context, content *storagev1alpha1.SnapshotContent) (bool, error) {
	if content == nil || content.Status.ManifestCheckpointName == "" {
		return false, nil
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: content.Status.ManifestCheckpointName}, mcp); err != nil {
		if errors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	ready := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		return false, nil
	}
	for _, ref := range mcp.OwnerReferences {
		if ref.APIVersion != storagev1alpha1.SchemeGroupVersion.String() || ref.Kind != "SnapshotContent" || ref.Name != content.Name {
			continue
		}
		if content.UID == "" || ref.UID == "" || ref.UID == content.UID {
			return true, nil
		}
	}
	return false, nil
}

// setOwnerCaptureLegCaptured monotonically flips a core-owned capture-leg success latch
// (status.captureState.commonController.<leg>) to true on the owner snapshot, under an optimistic-lock
// merge patch + conflict retry. It MUST be called before the corresponding request is reaped so the
// domain SDK (which suppresses re-creation on the latch via an authoritative uncached read) never
// re-creates it. Main owns commonController; the domain-owned request-name
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

// setOwnerChildSubtreesManifestsPersisted writes the core-computed children-only latch
// status.captureState.commonController.childSubtreesManifestsPersisted on the owner snapshot, monotonically
// (false -> true, never back). Unlike setOwnerCaptureLegCaptured (true-only), it also DECLARES the field
// false: leaving it nil would silently disable the SDK manifest-exclude pre-gate (nil = pre-gate off), so
// the field must exist as false while the children's subtrees are still capturing. It never downgrades a
// latched true and skips a redundant patch when the value is unchanged. Sideways write under an
// optimistic-lock merge patch (main owns commonController).
func (r *SnapshotContentController) setOwnerChildSubtreesManifestsPersisted(ctx context.Context, owner *unstructured.Unstructured, persisted bool) error {
	gvk := owner.GetObjectKind().GroupVersionKind()
	key := client.ObjectKey{Namespace: owner.GetNamespace(), Name: owner.GetName()}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(gvk)
		if err := r.APIReader.Get(ctx, key, fresh); err != nil {
			return err
		}
		cur, found, _ := unstructured.NestedBool(fresh.Object, "status", "captureState", "commonController", "childSubtreesManifestsPersisted")
		if found && cur {
			// Monotonic: a latched true is never downgraded.
			return nil
		}
		if found && cur == persisted {
			// Already declared with the desired value (false): no churn.
			return nil
		}
		base := fresh.DeepCopy()
		if err := unstructured.SetNestedField(fresh.Object, persisted, "status", "captureState", "commonController", "childSubtreesManifestsPersisted"); err != nil {
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
