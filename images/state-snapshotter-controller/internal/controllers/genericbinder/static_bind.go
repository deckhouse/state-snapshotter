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
	// staticBindRefMatches). A mismatch is a permanent misconfiguration (cross-binding two snapshots to one
	// content), so surface a terminal Ready=False. Restore does not weaken this: the core re-points the
	// content's snapshotRef onto THIS CR's identity (relaxed-CEL under parentDeleted) before we bind.
	if !genericStaticBindRefMatches(content.Spec.SnapshotRef, obj) {
		if err := r.patchSnapshotReadyFromContent(ctx, obj, snapshotLike, metav1.ConditionFalse, snapshot.ReasonSnapshotContentMisbound,
			fmt.Sprintf("SnapshotContent %q spec.snapshotRef does not point back at %s %s/%s", contentName, gvk.Kind, obj.GetNamespace(), obj.GetName())); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Bind once: set status.boundSnapshotContentName (idempotent). A static bind never points at the
	// deterministic capture name, so no content is created here — only the existing one is adopted.
	if snapshotLike.GetStatusContentName() != contentName {
		if err := snapshotbinding.PatchUnstructuredBoundContentName(ctx, r.Client, client.ObjectKeyFromObject(obj), gvk, contentName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Steady state: mirror the bound content's Ready condition + durable excludedRefs onto this domain CR
	// (single-aggregator contract). checkConsistencyAndSetReady already performs both the Ready mirror and
	// mirrorExcludedRefsFromContent; no capture legs are touched.
	if err := r.checkConsistencyAndSetReady(ctx, snapshotLike, obj); err != nil {
		logger.Error(err, "Failed to mirror static-bind SnapshotContent Ready")
	}
	if !snapshot.IsReady(snapshotLike) {
		return ctrl.Result{RequeueAfter: staticBindContentPollInterval}, nil
	}
	return ctrl.Result{}, nil
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
