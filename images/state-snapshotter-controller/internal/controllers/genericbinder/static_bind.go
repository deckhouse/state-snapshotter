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

package genericbinder

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotbinding"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// staticBindContentPollInterval is how often a static-bind domain snapshot re-checks for its referenced
// (not-yet-repointed) surviving SnapshotContent before the core restore orchestrator (re-)attaches it.
const staticBindContentPollInterval = 2 * time.Second

// snapshotIsStaticBind reports whether a domain XxxxSnapshot leaf is in StaticBind mode (spec.mode:
// StaticBind), mirroring Snapshot.IsStaticBind and the import-mode helper. A StaticBind leaf binds to an
// already-existing cluster-scoped SnapshotContent (spec.source.snapshotContentName) and runs no live
// capture: it is the recycle-bin restore path (wave4B), where the durable content survived deletion of
// its original namespaced Snapshot and is re-attached to a freshly re-created domain CR by the core.
func snapshotIsStaticBind(obj *unstructured.Unstructured) bool {
	mode, _, _ := unstructured.NestedString(obj.Object, "spec", "mode")
	return mode == string(storagev1alpha1.SnapshotModeStaticBind)
}

// reconcileGenericStaticBind implements CSI-like static (pre-provisioning) binding for a domain
// XxxxSnapshot whose spec.source.snapshotContentName references an already-existing cluster-scoped
// SnapshotContent. It is the domain twin of the core reconcileStaticBind and the capture/import twins
// (ensureSnapshotContentLinks / reconcileGenericImport): it validates the anti-spoofing handshake, binds
// status.boundSnapshotContentName, and mirrors the bound content's Ready + excludedRefs — running NO
// capture (no MCR/VCR, no children planning). The whole capture pipeline is skipped because the content
// already carries a manifestCheckpointName + dataRefs from its original capture (it is what survives in
// the TTL recycle bin).
//
// The Step-1 domain-planning barrier is intentionally bypassed: a StaticBind leaf has no domain capture
// planning (the domain controller skips capture on IsStaticBind), so there is no phase>=Planned to wait on.
func (r *GenericSnapshotBinderController) reconcileGenericStaticBind(
	ctx context.Context,
	obj *unstructured.Unstructured,
	snapshotLike snapshot.SnapshotLike,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	gvk := obj.GetObjectKind().GroupVersionKind()

	// Root static-bind snapshots (the namespace-root "Snapshot") are materialized by their own dedicated
	// reconciler (snapshot/static_bind.go: reconcileStaticBind), which validates the handshake, binds the
	// surviving content, and mirrors Ready. The binder now watches the root (wave5 domain-capture flip) but
	// must NOT double-handle its static-bind path — mirroring the import root-skip in reconcileGenericImport.
	if snapshot.IsRootSnapshot(obj) {
		logger.V(1).Info("static-bind snapshot is a root; handled by the namespace Snapshot orchestrator, skipping",
			"snapshot", obj.GetName(), "gvk", gvk.String())
		return ctrl.Result{}, nil
	}

	contentName, _, _ := unstructured.NestedString(obj.Object, "spec", "source", "snapshotContentName")
	if contentName == "" {
		// The CRD CEL guarantees StaticBind carries spec.source.snapshotContentName; treat a missing one as
		// a terminal misconfiguration surfaced on Ready rather than a nil-deref.
		if err := r.patchSnapshotReadyFromContent(ctx, obj, snapshotLike, metav1.ConditionFalse, snapshot.ReasonSnapshotContentMisbound,
			"StaticBind snapshot has empty spec.source.snapshotContentName"); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	content := &storagev1alpha1.SnapshotContent{}
	if err := r.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		if errors.IsNotFound(err) {
			// The core restore orchestrator may not have (re-)pointed the surviving content at this CR yet;
			// hold non-terminally and poll (the content->snapshot watch also wakes us on the re-point).
			if perr := r.patchSnapshotReadyFromContent(ctx, obj, snapshotLike, metav1.ConditionFalse, snapshot.ReasonSourceContentNotFound,
				fmt.Sprintf("pre-provisioned SnapshotContent %q not found", contentName)); perr != nil {
				return ctrl.Result{}, perr
			}
			return ctrl.Result{RequeueAfter: staticBindContentPollInterval}, nil
		}
		return ctrl.Result{}, err
	}

	// Anti-spoofing handshake: the content MUST point back at this domain CR (mirrors the core
	// staticBindRefMatches).
	//
	// Restore re-point (content-single-writer design §4 Slice 3 / decision #8): the binder is the creator
	// and the SOLE writer of content.spec, so when the surviving content is in the recycle bin
	// (status.parentDeleted) it re-points that content's snapshotRef onto THIS re-created CR's identity
	// here (relaxed-CEL admits a snapshotRef change only under parentDeleted), then binds on the next pass.
	// The core restore orchestrator (snapshot/static_bind.go) only re-creates the CR; it no longer writes
	// SnapshotContent.spec, so for a domain (non-root) StaticBind leaf the pre-re-point mismatch is the
	// EXPECTED first state of every restore — never a terminal fault.
	//
	// Therefore a mismatch is always handled non-terminally with a poll requeue: when parentDeleted is not
	// yet observed the re-point CEL gate is closed, so we surface Ready=False and poll until the recycle-bin
	// latch is visible (a transient stale-cache read must NOT strand the CR — it is not bound yet, so the
	// content->snapshot reverse map would not re-enqueue it, and a terminal no-requeue return would wedge
	// the restore permanently). This poll drives its own recovery without relying on a watch wake-up.
	if !genericStaticBindRefMatches(content.Spec.SnapshotRef, obj) {
		repointed, rerr := r.repointContentSnapshotRefToSelf(ctx, content.Name, obj)
		if rerr != nil {
			return ctrl.Result{}, rerr
		}
		if repointed {
			return ctrl.Result{Requeue: true}, nil
		}
		// Not re-pointed (parentDeleted not yet observed => relaxed-CEL gate closed, or a concurrent write):
		// surface Ready=False and poll until the recycle-bin latch lands and the re-point can proceed.
		if err := r.patchSnapshotReadyFromContent(ctx, obj, snapshotLike, metav1.ConditionFalse, snapshot.ReasonSnapshotContentMisbound,
			fmt.Sprintf("SnapshotContent %q spec.snapshotRef does not yet point back at %s %s/%s (awaiting recycle-bin re-point)", contentName, gvk.Kind, obj.GetNamespace(), obj.GetName())); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: staticBindContentPollInterval}, nil
	}

	// Bind once: set status.boundSnapshotContentName (idempotent). A static bind never points at the
	// deterministic capture name, so no content is created here — only the existing one is adopted.
	if snapshotLike.GetStatusContentName() != contentName {
		if err := snapshotbinding.PatchUnstructuredBoundContentName(ctx, r.Client, client.ObjectKeyFromObject(obj), gvk, contentName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Steady state: mirror the bound content's durable excludedRefs onto this domain CR and handle the
	// content-missing/deleting degradation (single-aggregator contract). The steady-state Ready mirror is
	// owned by the SnapshotContentController's single post-bind writer (wave7 final-wave-1); no capture legs
	// are touched here.
	if err := r.checkConsistencyAndSetReady(ctx, snapshotLike, obj); err != nil {
		logger.Error(err, "Failed to mirror static-bind SnapshotContent side channels")
	}
	if !snapshot.IsReady(snapshotLike) {
		return ctrl.Result{RequeueAfter: staticBindContentPollInterval}, nil
	}
	return ctrl.Result{}, nil
}

// repointContentSnapshotRefToSelf re-points a surviving recycle-bin SnapshotContent's spec.snapshotRef
// onto this re-created domain CR (restore, content-single-writer design §4 Slice 3 / decision #8). The
// binder is the sole writer of content.spec; the relaxed-CEL transition rule admits a snapshotRef change
// only once the content is in the recycle bin (status.parentDeleted), so the write is skipped (changed=
// false) until that latch is set and retried on a later reconcile. Returns changed=true when it issued the
// update. Mirrors the removed core snapshot/static_bind.go repointContentSnapshotRef for the domain-child
// leg (the orphan-leaf leg stays on the snapshot path until Block 3d).
func (r *GenericSnapshotBinderController) repointContentSnapshotRefToSelf(
	ctx context.Context,
	contentName string,
	obj *unstructured.Unstructured,
) (bool, error) {
	gvk := obj.GetObjectKind().GroupVersionKind()
	want := storagev1alpha1.SnapshotSubjectRef{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
		UID:        obj.GetUID(),
	}
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.SnapshotContent{}
		if err := r.Get(ctx, client.ObjectKey{Name: contentName}, cur); err != nil {
			return err
		}
		if cur.Spec.SnapshotRef != nil && *cur.Spec.SnapshotRef == want {
			changed = false
			return nil
		}
		if !cur.Status.ParentDeleted {
			// Not in the recycle bin yet: the CEL gate would reject the re-point. Skip; retry later.
			changed = false
			return nil
		}
		refCopy := want
		cur.Spec.SnapshotRef = &refCopy
		if err := r.Update(ctx, cur); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

// genericStaticBindRefMatches reports whether a SnapshotContent.spec.snapshotRef points back at the given
// domain snapshot CR. When the back-reference carries a UID it must equal this CR's UID (mirrors the core
// staticBindRefMatches and the CSI VolumeSnapshot<->VolumeSnapshotContent bound-UID check): after restore
// re-points the ref, the UID identifies the freshly re-created CR, so a stale content cannot bind a
// name-reused CR.
func genericStaticBindRefMatches(ref *storagev1alpha1.SnapshotSubjectRef, obj *unstructured.Unstructured) bool {
	if ref == nil {
		return false
	}
	gvk := obj.GetObjectKind().GroupVersionKind()
	if ref.UID != "" && ref.UID != obj.GetUID() {
		return false
	}
	return ref.APIVersion == gvk.GroupVersion().String() &&
		ref.Kind == gvk.Kind &&
		ref.Name == obj.GetName() &&
		ref.Namespace == obj.GetNamespace()
}
