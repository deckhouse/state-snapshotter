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
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snaphelpers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotbinding"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// importContentPollInterval is the polling fallback cadence while an imported leaf's SnapshotContent is
// being materialized (uploaded ManifestCheckpoint not yet present, DataImport artifact not yet produced,
// or content not yet Ready). The binder takes no watch on DataImport, so this poll drives convergence.
const importContentPollInterval = 5 * time.Second

// snapshotIsImportMode reports whether a generic/domain snapshot leaf is in IMPORT mode. Our domain
// snapshot CRDs signal it with the enum spec.mode: Import (parity with Snapshot.IsImportMode / domain
// IsImportMode); the shared helper reads the same enum off the extended CSI VolumeSnapshot fork (its
// CSI-shaped VolumeSnapshot fork. An import leaf is materialized from the uploaded payload and — for
// data-artifact kinds — the matching DataImport found by reverse-lookup (DataImport.spec.targetRef),
// not from a name carried on the leaf.
func snapshotIsImportMode(obj *unstructured.Unstructured) bool {
	return usecase.IsUnstructuredImportMode(obj)
}

// reconcileGenericImport materializes the SnapshotContent that backs an import-mode generic/domain leaf
// from the out-of-band uploaded payload + the leaf's DataImport, instead of projecting a live capture
// (MCR/VCR). It is the import twin of the capture path (ensureSnapshotContentLinks) and uses the SAME
// common controllers / SnapshotContent so there is a single content materializer for capture and import:
//   - content: create the cluster-scoped SnapshotContent (deletionPolicy=Delete for import — no live
//     source to re-capture from) owned by the parent SnapshotContent, then bind boundSnapshotContentName;
//   - manifest leg: publish the reconstructed ManifestCheckpoint (manifests-and-children-refs-upload keyed
//     to the leaf UID);
//   - children: publish the content-graph edges from the uploaded namespaced child refs;
//   - data leg: read DataImport.status.data.artifactRef -> VolumeSnapshotContent, force Retain + transfer
//     ownership to the content, publish dataRef;
//   - Ready: mirror the bound content's Ready (single-aggregator), exiting ImportPending.
//
// The Step-1 domain-planning barrier is intentionally bypassed: an import leaf has no domain capture
// planning (the domain controller skips it), so there is no PlanningReady to wait on.
func (r *GenericSnapshotBinderController) reconcileGenericImport(
	ctx context.Context,
	obj *unstructured.Unstructured,
	snapshotLike snapshot.SnapshotLike,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	gvk := obj.GetObjectKind().GroupVersionKind()

	// Content owner: a non-root imported leaf's SnapshotContent is owned by the parent's SnapshotContent
	// (d8 sets child->parent ownerRefs); a ROOT import snapshot's content is owned by the root ObjectKeeper
	// exactly like the capture root. Resolve the parent ownerRef first; a nil (non-pending) result means
	// this is a root, which the binder now also creates (content-single-writer design §10, creator=binder).
	ownerRef, pending, err := controllercommon.ResolveParentSnapshotContentOwnerRef(ctx, r.Client, obj)
	if err != nil {
		return ctrl.Result{}, err
	}
	isRoot := false
	if ownerRef == nil {
		if pending {
			// Parent content not yet materialized (bottom-up convergence); poll.
			return ctrl.Result{RequeueAfter: importContentPollInterval}, nil
		}
		if !snapshot.IsRootSnapshot(obj) {
			// A non-root import leaf with no parent ownerRef and not pending is a misconfiguration (the
			// child->parent ownerRef never arrived and never will); stop instead of requeueing forever.
			logger.Info("generic import snapshot has no parent ownerRef and is not a root; skipping",
				"snapshot", obj.GetName(), "gvk", gvk.String())
			return ctrl.Result{}, nil
		}
		// Root import (content-single-writer design §10): the binder is the creator for import roots too,
		// not the namespace Snapshot orchestrator. Anchor the root content on the root ObjectKeeper (unified
		// TTL GC) exactly like the capture root; the orchestrator (reconcileImport) mirrors Ready and the
		// aggregator projects the manifest leg + children edges. The binder is the creator ONLY for the root
		// (see the isRoot early-return after create+bind below).
		isRoot = true
		objectKeeper, okRes, okErr := controllercommon.EnsureRootObjectKeeperWithTTL(ctx, r.Client, r.APIReader, r.Config, obj, gvk)
		if okErr != nil {
			return ctrl.Result{}, okErr
		}
		if okRes.Requeue || okRes.RequeueAfter > 0 {
			return okRes, nil
		}
		ref := controllercommon.RootObjectKeeperOwnerReference(objectKeeper)
		ownerRef = &ref
	}

	contentName := snapshotLike.GetStatusContentName()
	if contentName == "" {
		contentName = snapshotbinding.StableContentName(obj.GetName(), obj.GetUID())
		content := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{
				Name:            contentName,
				OwnerReferences: []metav1.OwnerReference{*ownerRef},
			},
			Spec: importSnapshotContentSpec(obj),
		}
		if err := r.Create(ctx, content); err != nil && !errors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		if err := snapshotbinding.PatchUnstructuredBoundContentName(ctx, r.Client, client.ObjectKeyFromObject(obj), gvk, contentName); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("Materialized import SnapshotContent", "name", contentName, "owner", ownerRef.Kind)
		return ctrl.Result{Requeue: true}, nil
	}

	// Keep the parent ownerRef aligned on the already-bound content (e.g. parent content recreated).
	content := &storagev1alpha1.SnapshotContent{}
	if err := r.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		return ctrl.Result{}, err
	}
	if changed, err := controllercommon.EnsureLifecycleOwnerRef(ctx, r.Client, content, *ownerRef); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		return ctrl.Result{Requeue: true}, nil
	}

	if isRoot {
		// Root import: the binder is ONLY the creator (content object + spec + root ownerRef + bind). The
		// namespace Snapshot orchestrator (reconcileImport) mirrors the bound content's Ready and the
		// aggregator projects the manifest leg + children edges — the same division of labor as the capture
		// root. The leaf-only tail below (MCP-gate wait, DataImport data leg, Ready mirror) does NOT apply to
		// the structural root (no data leg; Ready is mirrored by the orchestrator, not co-written here), so
		// return after create+bind to avoid a second writer on the root snapshot's Ready.
		return ctrl.Result{}, nil
	}

	// Manifest leg moved to the SnapshotContentController aggregator (INV-CONTENT-WRITER-1,
	// content-single-writer design §10): the aggregator is the single writer of status.manifestCheckpointName
	// for import too, projecting the reconstructed checkpoint name (keyed to the leaf UID) once the upload
	// endpoint has created it. The binder no longer publishes it; it only waits for the checkpoint to exist
	// before proceeding to the data leg (the manifest must be uploaded before the leaf can be Ready anyway).
	mcpName := usecase.ReconstructedManifestCheckpointName(obj.GetUID(), "")
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := r.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: importContentPollInterval}, nil
		}
		return ctrl.Result{}, err
	}

	// Children projection moved to the SnapshotContentController aggregator (INV-CONTENT-CHILDREN-1,
	// content-single-writer design §3.1/§3.2): the aggregator projects childrenSnapshotContentRefs from the
	// uploaded status.childrenSnapshotRefs the same way for capture and import (an import owner has no domain
	// phase, so the "planned" gate is exactly "every uploaded child snapshot has bound its content"). The
	// binder no longer publishes the child edge set; the content's mirrored Ready (gated by the aggregator's
	// ChildrenReady) keeps this leaf pending until its children are linked.

	// Data leg: only data-artifact snapshot kinds (CSD spec.requiresDataArtifact) carry a volume data leg
	// and have a matching DataImport. A structural import node (requiresDataArtifact=false, e.g. a VM
	// snapshot or root Snapshot) has only manifests + children, so it skips the data leg entirely —
	// otherwise it would poll forever for a DataImport that never exists.
	if r.GVKRegistry.RequiresDataArtifact(gvk.Kind) {
		// Reverse-lookup: the leaf carries no DataImport name; find the DataImport whose spec.targetRef
		// points at this leaf (exactly one; >=2 is fail-closed).
		di, treason, tmsg, lErr := controllercommon.FindDataImportForLeaf(ctx, r.Client, obj)
		if lErr != nil {
			return ctrl.Result{}, lErr
		}
		if treason != "" {
			if perr := r.patchSnapshotNotReadyFromContent(ctx, obj, snapshotLike, treason, tmsg); perr != nil {
				return ctrl.Result{}, perr
			}
			return ctrl.Result{}, nil
		}
		if di == nil {
			// d8 creates the DataImport alongside the leaf; it may not be visible yet. Pending, poll.
			return ctrl.Result{RequeueAfter: importContentPollInterval}, nil
		}

		// Data-leg CONTENT write moved to the SnapshotContentController aggregator (INV-CONTENT-WRITER-1,
		// content-single-writer design §10): the aggregator is the single writer of content.status.data for
		// import too (projectContentDataLegFromDataImport runs the same DataImport->VSC Retain+ownerRef
		// handoff + publish). The binder keeps ONLY the two leaf-facing jobs the aggregator cannot: surface a
		// non-retryable artifact terminal on the leaf, and mirror the aggregator-published content.status.data
		// onto the leaf's top-level status.data for d8 export.
		if _, _, dtreason, dtmsg := snapshotcontent.BuildImportDataBinding(di, obj); dtreason != "" {
			// Actionable import failure (e.g. unsupported artifact kind): the content stays pending (no
			// dataRef), so the pure content mirror cannot express it — co-write Ready=False directly.
			if perr := r.patchSnapshotNotReadyFromContent(ctx, obj, snapshotLike, dtreason, dtmsg); perr != nil {
				return ctrl.Result{}, perr
			}
			return ctrl.Result{}, nil
		}
		// Mirror the aggregator-published content.status.data onto the leaf for d8 export. It is a no-op
		// until the aggregator publishes; the !Ready poll below drives convergence (a Ready content always
		// has its data leg published, so a Ready leaf is always mirrored first). storageClassName is absent
		// from the content data by design, so take it from DataImport.spec.storageClassName;
		// source/artifact/size/volumeMode come from content.status.data.
		scOverride, _, _ := unstructured.NestedString(di.Object, "spec", "storageClassName")
		if mErr := r.mirrorLeafDataFromContent(ctx, obj, contentName, scOverride); mErr != nil {
			logger.Error(mErr, "Failed to mirror volume data to import leaf status")
		}
	}

	// Mirror the bound content's Ready (single-aggregator, INV-COND4). The content->snapshot watch wakes
	// this leaf on the Ready transition; the requeue is a missed-event fallback while the aggregator is
	// still converging the manifest/children/data legs.
	if err := r.checkConsistencyAndSetReady(ctx, snapshotLike, obj); err != nil {
		logger.Error(err, "Failed to mirror import SnapshotContent Ready")
	}
	if !snapshot.IsReady(snapshotLike) {
		return ctrl.Result{RequeueAfter: importContentPollInterval}, nil
	}
	return ctrl.Result{}, nil
}

// importSnapshotContentSpec returns the SnapshotContent spec for an imported leaf: deletionPolicy=Delete
// (capture uses Retain) and snapshotRef pointing back at the leaf snapshot itself (the binding subject
// that sets status.boundSnapshotContentName on this content). An imported tree has no live source to
// re-capture from, so deleting the import root must reclaim the materialized content+artifacts rather
// than park them in the TTL bin.
func importSnapshotContentSpec(leaf *unstructured.Unstructured) storagev1alpha1.SnapshotContentSpec {
	return controllercommon.NewSnapshotContentSpec(
		storagev1alpha1.SnapshotContentDeletionPolicyDelete,
		controllercommon.SnapshotSubjectRefFromObject(leaf),
	)
}
