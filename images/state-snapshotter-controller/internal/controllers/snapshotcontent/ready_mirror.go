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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// mirrorReadyToOwnerSnapshot mirrors the just-reconciled SnapshotContent.Ready onto the owning Snapshot's
// Ready condition in the SAME reconcile pass that computed content.Ready, bubbling a domain phase=Failed
// into a terminal Ready=False.
//
// w7-main-split: the SnapshotContent controller (reconcile key = SnapshotContent) is the post-bind writer
// of Snapshot.Ready. content.spec.snapshotRef resolves the owning Snapshot, and the mirror only runs once
// the owner has adopted THIS content (status.boundSnapshotContentName == content name) — the monotonic
// creator->main writer switch. Pre-bind, the creator/binder owns Snapshot.Ready. Collapsing the derive
// and the mirror into one pass is what closes the staleness window where the binder re-derived a stale
// Ready from a cross-controller hop (INV-FAIL-PROP).
//
// wave7 final-wave-1: this controller is now the SINGLE post-bind writer of the steady-state Snapshot.Ready
// (content.Ready verbatim + capture-leg terminal fold + phase=Failed bubble + barrier-2 phase=Finished
// gate). The binder no longer re-derives content.Ready. The binder retains only (a) the pre-bind Ready and
// the content-missing/deleting degradation Ready — a deleted content produces no reconcile here to mirror
// from, so the binder co-writes ContentMissing, woken by its bound-content watch — and (b) the excludedRefs
// side-channel mirror (not Ready; triggered by the same watch). Keeping (a)/(b) in the binder is why that
// watch is not removed. The childSubtreesManifestsPersisted latch is main-owned (capture_legs.go, decision #10).
//
// vcr-watch-core-terminal (decision D2): a failed data-leg VCR (or a Variant-A >1-artifact fault) is now
// made terminal on the CONTENT itself by reconcileDataLegProjection (DataReady=VolumeCaptureFailed), so
// content.Ready already carries the terminal and this mirror reflects it verbatim onto the owning Snapshot
// — no more special leg-terminal fold here. The content-level terminal also propagates up the content
// aggregation tree as ChildrenFailed (which the former snapshot-only fold could not do).
//
// Best-effort: an owner that is gone or not yet bound, or content with no owning Snapshot (bucket
// content), is a no-op; a transient API error is returned so the content reconcile requeues and retries.
func (r *SnapshotContentController) mirrorReadyToOwnerSnapshot(ctx context.Context, contentObj *unstructured.Unstructured) error {
	owner, _, ownerFound, err := r.ownerSnapshot(ctx, contentObj)
	if err != nil {
		return err
	}
	return r.mirrorReadyToOwnerSnapshotWithOwner(ctx, contentObj, owner, ownerFound)
}

// mirrorReadyToOwnerSnapshotWithOwner is mirrorReadyToOwnerSnapshot with the owning Snapshot ALREADY
// resolved. Reconcile resolves the owner ONCE per pass (ownerSnapshot) and shares that same object with the
// content-side domain-phase fold (reconcileCommonSnapshotContentStatus) and with this mirror, so the hot
// path does not re-Get the owner here and both post-bind Ready writers fold from the SAME phase snapshot.
// owner==nil / !ownerFound (ownerless/bucket content, owner gone, or not yet observable) is a no-op; the
// actual Ready write still re-reads a fresh owner under an optimistic lock (patchOwnerReadyFromContent).
func (r *SnapshotContentController) mirrorReadyToOwnerSnapshotWithOwner(ctx context.Context, contentObj *unstructured.Unstructured, owner *unstructured.Unstructured, ownerFound bool) error {
	if !ownerFound || owner == nil {
		return nil
	}

	// Writer switch (creator -> main): only mirror once the owner has adopted THIS content. Pre-bind the
	// creator/binder owns Snapshot.Ready; a cross-binding (owner bound to a different content) is not ours to
	// write. Every domain owner — including the VolumeSnapshot domain kind (content-single-writer design
	// §11.6) — carries status.boundSnapshotContentName, so this one writer switch covers them all.
	bound, _, _ := unstructured.NestedString(owner.Object, "status", "boundSnapshotContentName")
	if bound != contentObj.GetName() {
		return nil
	}

	contentLike, err := snapshot.ExtractSnapshotContentLike(contentObj)
	if err != nil {
		return fmt.Errorf("extract SnapshotContentLike: %w", err)
	}
	readyCond := snapshot.GetCondition(contentLike, snapshot.ConditionReady)
	status := metav1.ConditionFalse
	reason := snapshot.ReasonContentMissing
	message := fmt.Sprintf("SnapshotContent %s has no Ready condition", contentObj.GetName())
	if readyCond != nil {
		status = readyCond.Status
		reason = readyCond.Reason
		message = readyCond.Message
	}
	// Barrier 2 (ADR §6.2 — "finalize Ready ONLY after domain phase=Finished") + domain-failure bubble.
	// The SAME shared fold is applied to the SnapshotContent's OWN Ready in
	// reconcileCommonSnapshotContentStatus (forContent=true), so both post-bind Ready writers agree and a
	// domain phase=Failed / not-yet-Finished propagates up the content-aggregation tree.
	// CURRENT (51eb6c2): forContent=false keeps the raw domain reason/message on the namespaced owner.
	// TARGET (active plan): it uses canonical DomainCaptureFailed and embeds those details in
	// Condition.Message.
	status, reason, message = applyDomainPhaseFold(owner, false, status, reason, message)
	// Fold a vanished declared child into the owner mirror (main-detected structural degradation; the
	// content's own Ready stays intact — only the namespaced user surface degrades, because d8 download
	// reads namespaced CRs). Applied LAST so a terminal ChildSnapshotLost always surfaces, while a
	// non-terminal ChildSnapshotDeleted only downgrades an owner that would otherwise be genuinely Ready=True
	// (post-Finished barrier 2) — a still-capturing (ChildrenPending) or already-terminal owner keeps its
	// reason. Runs only past barrier 1 and no-ops when the domain already reported phase=Failed (above).
	lostReason, lostMessage, lostErr := r.detectLostDeclaredChildren(ctx, owner, contentObj)
	if lostErr != nil {
		return fmt.Errorf("detect lost declared children: %w", lostErr)
	}
	switch {
	case lostReason == snapshot.ReasonChildSnapshotLost:
		status = metav1.ConditionFalse
		reason = lostReason
		message = lostMessage
	case lostReason == snapshot.ReasonChildSnapshotDeleted && status == metav1.ConditionTrue:
		status = metav1.ConditionFalse
		reason = lostReason
		message = lostMessage
	}
	return r.patchOwnerReadyFromContent(ctx, owner, status, reason, message)
}

// patchOwnerReadyFromContent mirrors the Ready condition onto the owning Snapshot, gen-stamped under an
// optimistic-lock merge patch. The domain controller co-writes captureState.domainSpecificController into
// the same status; MergeFromWithOptimisticLock turns a concurrent write into a 409 so RetryOnConflict
// re-reads the fresh object (already carrying the other writer's state) and re-applies only this
// condition, stamping observedGeneration for gen-gated readers.
func (r *SnapshotContentController) patchOwnerReadyFromContent(
	ctx context.Context,
	owner *unstructured.Unstructured,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	ownerLike, err := snapshot.ExtractSnapshotLike(owner)
	if err != nil {
		return err
	}
	// Fast path: nothing to do if the in-memory view already matches the desired condition.
	if cur := snapshot.GetCondition(ownerLike, snapshot.ConditionReady); cur != nil &&
		cur.Status == status && cur.Reason == reason && cur.Message == message &&
		cur.ObservedGeneration == owner.GetGeneration() {
		return nil
	}

	gvk := owner.GetObjectKind().GroupVersionKind()
	key := client.ObjectKey{Namespace: owner.GetNamespace(), Name: owner.GetName()}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(gvk)
		if err := r.APIReader.Get(ctx, key, fresh); err != nil {
			return err
		}
		freshLike, err := snapshot.ExtractSnapshotLike(fresh)
		if err != nil {
			return err
		}
		gen := fresh.GetGeneration()
		if cur := snapshot.GetCondition(freshLike, snapshot.ConditionReady); cur != nil &&
			cur.Status == status && cur.Reason == reason && cur.Message == message &&
			cur.ObservedGeneration == gen {
			return nil
		}
		base := fresh.DeepCopy()
		snapshot.SetCondition(freshLike, snapshot.ConditionReady, status, reason, message)
		conds := freshLike.GetStatusConditions()
		for i := range conds {
			if conds[i].Type == snapshot.ConditionReady {
				conds[i].ObservedGeneration = gen
			}
		}
		freshLike.SetStatusConditions(conds)
		snapshot.SyncConditionsToUnstructured(fresh, freshLike.GetStatusConditions())
		if err := r.Status().Patch(ctx, fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("mirror Snapshot Ready: %w", err)
		}
		return nil
	})
}

// applyDomainPhaseFold folds the owning Snapshot's domain capture phase
// (status.captureState.domainSpecificController.phase) into a base Ready triple. It is the SINGLE shared
// implementation of ADR §6.2 "barrier 2", used by BOTH post-bind Ready writers so they always agree:
//
//   - reconcileCommonSnapshotContentStatus applies it to the SnapshotContent's OWN Ready (forContent=true)
//     so a domain phase=Failed / not-yet-Finished propagates up the CONTENT-aggregation tree — a child
//     content that is not Ready holds (ChildrenPending) or fails (ChildrenFailed) its parent. This is the
//     domain-phase twin of the data-leg terminal that already lives on the content (VolumeCaptureFailed).
//   - mirrorReadyToOwnerSnapshot applies it to the owning Snapshot's mirrored Ready (forContent=false).
//
// CURRENT (51eb6c2) behavior only ever DOWNGRADES a Ready (never upgrades False->True):
//   - phase=Failed -> Ready=False. forContent=true uses the canonical, tree-propagating terminal reason
//     ReasonDomainCaptureFailed (the domain's free-form reason is NOT in terminalChildContentFailureReasons,
//     so a parent content would otherwise misread a failed domain child as pending); forContent=false keeps
//     the raw domain reason/message for the domain CR's user-facing Ready. The raw domain reason/message is
//     preserved in the message in both cases.
//   - Ready=True AND phase set AND phase!=Finished -> Ready=False/ChildrenPending (non-terminal): the domain
//     may still be running consistency actions (fs freeze/unfreeze, verify) after publishing its objects.
//   - phase="" (non-domain owner) or owner nil -> unchanged (verbatim). The content reconcile re-runs on the
//     owner-status watch when the phase advances, so this converges without a dedicated wake-up.
//
// TARGET (active namespace-root-MCR-before-Planned plan; not implemented here): phase=Failed uses
// ReasonDomainCaptureFailed for BOTH the content and namespaced owner; the original free-form domain
// reason/message is embedded in Condition.Message. childrenSettled then reads child Ready only and has no
// direct phase fallback.
func applyDomainPhaseFold(owner *unstructured.Unstructured, forContent bool, status metav1.ConditionStatus, reason, message string) (metav1.ConditionStatus, string, string) {
	if owner == nil {
		return status, reason, message
	}
	phase := ownerDomainCapturePhase(owner)
	if phase == "" {
		return status, reason, message
	}
	if failed, freason, fmsg := ownerDomainCaptureFailed(owner); failed {
		if forContent {
			return metav1.ConditionFalse, snapshot.ReasonDomainCaptureFailed, domainFailedMessage(freason, fmsg, message)
		}
		outReason, outMessage := reason, message
		if freason != "" {
			outReason = freason
		}
		if fmsg != "" {
			outMessage = fmsg
		}
		return metav1.ConditionFalse, outReason, outMessage
	}
	if status == metav1.ConditionTrue &&
		storagev1alpha1.SnapshotCapturePhase(phase) != storagev1alpha1.SnapshotCapturePhaseFinished {
		return metav1.ConditionFalse, snapshot.ReasonChildrenPending,
			fmt.Sprintf("waiting for domain capture to finish (phase=%s)", phase)
	}
	return status, reason, message
}

// domainFailedMessage currently composes the SnapshotContent-side message for a domain phase=Failed fold,
// preserving the domain's original reason/message (falling back to the pre-fold content message, then a
// generic). The active target reuses the same canonical message contract for the namespaced owner.
func domainFailedMessage(freason, fmsg, fallback string) string {
	switch {
	case freason != "" && fmsg != "":
		return fmt.Sprintf("domain capture failed: %s: %s", freason, fmsg)
	case freason != "":
		return "domain capture failed: " + freason
	case fmsg != "":
		return "domain capture failed: " + fmsg
	case fallback != "":
		return "domain capture failed: " + fallback
	default:
		return "domain capture failed"
	}
}

// ownerDomainCapturePhase reads status.captureState.domainSpecificController.phase off the owning Snapshot.
// It is "" when the owner is not a domain-capture kind (import/leaf handle), which the callers
// use to skip the domain barriers for non-domain owners.
func ownerDomainCapturePhase(obj *unstructured.Unstructured) string {
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "captureState", "domainSpecificController", "phase")
	return phase
}

// ownerDomainCaptureFailed reports whether the owning Snapshot's domain half reported a terminal failure
// (captureState.domainSpecificController.phase=Failed) and returns its reason/message so the core can
// bubble it into the user-facing Ready.
func ownerDomainCaptureFailed(obj *unstructured.Unstructured) (bool, string, string) {
	if storagev1alpha1.SnapshotCapturePhase(ownerDomainCapturePhase(obj)) != storagev1alpha1.SnapshotCapturePhaseFailed {
		return false, "", ""
	}
	reason, _, _ := unstructured.NestedString(obj.Object, "status", "captureState", "domainSpecificController", "reason")
	message, _, _ := unstructured.NestedString(obj.Object, "status", "captureState", "domainSpecificController", "message")
	return true, reason, message
}
