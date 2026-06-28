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

package snapshot

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// staticBindContentPollInterval is how often a static-bind Snapshot re-checks for its referenced
// (not-yet-created) pre-provisioned SnapshotContent before the import controller materializes it.
const staticBindContentPollInterval = 2 * time.Second

// reconcileStaticBind implements CSI-like static (pre-provisioning) binding for a root Snapshot whose
// spec.source.snapshotContentName references an already-existing cluster-scoped SnapshotContent.
//
// It mirrors the VolumeSnapshot <-> VolumeSnapshotContent handshake: the bind succeeds only when the
// referenced content points back at this Snapshot via spec.snapshotRef. The whole capture pipeline
// (ObjectKeeper re-own, MCR/VCR, manifest checkpoint, child graph) is skipped: the content already
// carries a manifestCheckpointName and dataRefs from the import path. The Snapshot's Ready is then a
// pure mirror of the bound content's Ready.
func (r *SnapshotReconciler) reconcileStaticBind(ctx context.Context, nsSnap *storagev1alpha1.Snapshot) (ctrl.Result, error) {
	contentName := nsSnap.Spec.Source.SnapshotContentName

	content := &storagev1alpha1.SnapshotContent{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			// The import controller may not have created the content yet; retry without a terminal failure.
			if _, ferr := r.failCapture(ctx, nsSnap, nil, snapshotpkg.ReasonSourceContentNotFound,
				fmt.Sprintf("pre-provisioned SnapshotContent %q not found", contentName)); ferr != nil {
				return ctrl.Result{}, ferr
			}
			return ctrl.Result{RequeueAfter: staticBindContentPollInterval}, nil
		}
		return ctrl.Result{}, err
	}

	// Static-binding handshake: the content MUST point back at this Snapshot. A mismatch is a permanent
	// misconfiguration (cross-binding two snapshots to one content), so fail terminally.
	if !staticBindRefMatches(content.Spec.SnapshotRef, nsSnap) {
		return r.failCapture(ctx, nsSnap, nil, snapshotpkg.ReasonSnapshotContentMisbound,
			fmt.Sprintf("SnapshotContent %q spec.snapshotRef does not point back at Snapshot %s/%s", contentName, nsSnap.Namespace, nsSnap.Name))
	}

	// Bind once: a static bind never points at the deterministic capture name, so the main reconcile's
	// expectedName reset MUST NOT run for these snapshots (the caller branches before it).
	if nsSnap.Status.BoundSnapshotContentName != contentName {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur := &storagev1alpha1.Snapshot{}
			if err := r.Client.Get(ctx, client.ObjectKey{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, cur); err != nil {
				return err
			}
			cur.Status.BoundSnapshotContentName = contentName
			cur.Status.ObservedGeneration = cur.Generation
			return r.Client.Status().Update(ctx, cur)
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// A statically-bound content has no residual/orphan-PVC wave of its own (it was materialized by the
	// import path or pre-provisioned out of band). Latch the residual gate Complete (idempotent: import
	// content already carries it; pre-provisioned content gets it here) so the aggregator's fail-closed
	// residual gate cannot hold the mirror's first Ready=True forever.
	if err := snapshotcontent.MarkResidualVolumeCaptureComplete(ctx, r.Client, content.Name, nil); err != nil {
		return ctrl.Result{}, err
	}
	// Steady state: mirror the bound content's Ready condition onto the Snapshot (single-aggregator
	// contract, INV-COND4). If the content is not Ready yet, the mirror sets a pending reason.
	if err := r.mirrorSnapshotReadyFromBoundContent(ctx, nsSnap, content, nil); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// staticBindRefMatches reports whether a SnapshotContent.spec.snapshotRef points back at nsSnap.
// When the back-reference carries a UID it must equal this Snapshot's UID: this prevents a stale
// pre-provisioned content from binding a freshly re-created Snapshot that reuses the same
// name/namespace (mirrors the CSI VolumeSnapshot<->VolumeSnapshotContent bound-UID check). A pre-
// provisioned content legitimately created before the Snapshot exists may leave the UID empty.
func staticBindRefMatches(ref *storagev1alpha1.SnapshotSubjectRef, nsSnap *storagev1alpha1.Snapshot) bool {
	if ref == nil {
		return false
	}
	if ref.UID != "" && ref.UID != nsSnap.UID {
		return false
	}
	return ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() &&
		ref.Kind == "Snapshot" &&
		ref.Name == nsSnap.Name &&
		ref.Namespace == nsSnap.Namespace
}
