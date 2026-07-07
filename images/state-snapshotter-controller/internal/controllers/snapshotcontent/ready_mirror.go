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
// (content.Ready verbatim + phase=Failed bubble + barrier-2 phase=Finished gate). The binder no longer
// re-derives content.Ready. The binder retains only (a) the pre-bind Ready and the content-missing/deleting
// degradation Ready — a deleted content produces no reconcile here to mirror from, so the binder co-writes
// ContentMissing, woken by its bound-content watch — and (b) the excludedRefs / subtreeManifestsPersisted
// side-channel mirrors (not Ready; triggered by the same watch). Keeping (a)/(b) in the binder is why that
// watch is not removed.
//
// Best-effort: an owner that is gone or not yet bound, or content with no owning Snapshot (bucket
// content), is a no-op; a transient API error is returned so the content reconcile requeues and retries.
func (r *SnapshotContentController) mirrorReadyToOwnerSnapshot(ctx context.Context, contentObj *unstructured.Unstructured) error {
	apiVersion, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "apiVersion")
	kind, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "kind")
	namespace, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "namespace")
	name, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "name")
	if apiVersion == "" || kind == "" || name == "" {
		// Ownerless / bucket content: no owning Snapshot to mirror onto.
		return nil
	}
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return fmt.Errorf("parse snapshotRef.apiVersion %q: %w", apiVersion, err)
	}
	ownerGVK := gv.WithKind(kind)

	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(ownerGVK)
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, owner); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get owner Snapshot %s/%s: %w", namespace, name, err)
	}

	// Writer switch (creator -> main): only mirror once the owner has adopted THIS content. Pre-bind the
	// creator/binder owns Snapshot.Ready; a cross-binding (owner bound to a different content) or an owner
	// kind with no bind model (e.g. a VolumeSnapshot leaf handle) is likewise not ours to write.
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
	// Bubble a domain-reported terminal failure (captureState.domainSpecificController.phase=Failed) into
	// the user-facing Ready: a content mirror cannot express a domain planning/consistency failure.
	if failed, freason, fmsg := ownerDomainCaptureFailed(owner); failed {
		status = metav1.ConditionFalse
		if freason != "" {
			reason = freason
		}
		if fmsg != "" {
			message = fmsg
		}
	}
	// Barrier 2 (ADR §6.2 — "финализация Ready ТОЛЬКО после доменного phase=Finished"): on a
	// domain-capture owner, do NOT finalize a mirrored Ready=True until the domain reported
	// captureState.domainSpecificController.phase=Finished — the domain may still be running consistency
	// actions (fs freeze/unfreeze, verify) after publishing its objects. While phase is Planning/Planned the
	// aggregate is held Ready=False with a non-terminal ChildrenPending (phase=Failed is already bubbled
	// above; a non-domain owner has no phase field and is unaffected). The content reconcile re-runs on the
	// owner-status watch when the phase advances, so this converges without a dedicated wake-up.
	if status == metav1.ConditionTrue {
		if phase := ownerDomainCapturePhase(owner); phase != "" &&
			storagev1alpha1.SnapshotCapturePhase(phase) != storagev1alpha1.SnapshotCapturePhaseFinished {
			status = metav1.ConditionFalse
			reason = snapshot.ReasonChildrenPending
			message = fmt.Sprintf("waiting for domain capture to finish (phase=%s)", phase)
		}
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

// ownerDomainCapturePhase reads status.captureState.domainSpecificController.phase off the owning Snapshot.
// It is "" when the owner is not a domain-capture kind (import/static-bind/leaf handle), which the callers
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
