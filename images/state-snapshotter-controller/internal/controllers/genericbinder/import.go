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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
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
// IsImportMode); the shared helper also tolerates the legacy spec.source.import marker kept by the
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
//   - data leg: read DataImport.status.data.artifact -> VolumeSnapshotContent, force Retain + transfer
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

	// Content owner: an imported leaf is never a root snapshot (d8 sets child->parent ownerRefs), so its
	// SnapshotContent is owned by the parent's SnapshotContent. Wait until the parent content materializes.
	ownerRef, pending, err := controllercommon.ResolveParentSnapshotContentOwnerRef(ctx, r.Client, obj)
	if err != nil {
		return ctrl.Result{}, err
	}
	if ownerRef == nil {
		if pending {
			// Parent content not yet materialized (bottom-up convergence); poll.
			return ctrl.Result{RequeueAfter: importContentPollInterval}, nil
		}
		// No parent ownerRef at all: this is a ROOT generic import snapshot. Roots are materialized by the
		// namespace Snapshot import orchestrator (snapshot/import.go), not here; a root-capable generic kind
		// in import mode is out of scope for this binder. Stop instead of requeueing forever.
		logger.Info("generic import snapshot has no parent ownerRef (root); roots are materialized by the namespace Snapshot orchestrator, skipping",
			"snapshot", obj.GetName(), "gvk", gvk.String())
		return ctrl.Result{}, nil
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

	// Manifest leg: the reconstructed ManifestCheckpoint (keyed to the leaf UID by the upload endpoint).
	// Until d8 uploads this node there is nothing to back the content — hold (non-terminal) and poll.
	mcpName := usecase.ReconstructedManifestCheckpointName(obj.GetUID(), "")
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := r.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: importContentPollInterval}, nil
		}
		return ctrl.Result{}, err
	}
	if err := snapshotcontent.PublishSnapshotContentManifestCheckpointName(ctx, r.Client, contentName, mcpName); err != nil {
		return ctrl.Result{}, err
	}

	requeue := false

	// Children projection from the uploaded namespaced child refs (a data leaf has none). Bottom-up:
	// only resolves once children materialized their own content; poll until then.
	childRefs := parseChildrenSnapshotRefs(obj)
	if len(childRefs) > 0 {
		published, pErr := snapshotcontent.PublishSnapshotContentChildrenFromSnapshotRefs(ctx, r.Client, r.APIReader, obj.GetNamespace(), contentName, childRefs)
		if pErr != nil {
			return ctrl.Result{}, pErr
		}
		if !published {
			requeue = true
		}
	}

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
			if perr := r.patchSnapshotReadyFromContent(ctx, obj, snapshotLike, metav1.ConditionFalse, treason, tmsg); perr != nil {
				return ctrl.Result{}, perr
			}
			return ctrl.Result{}, nil
		}
		if di == nil {
			// d8 creates the DataImport alongside the leaf; it may not be visible yet. Pending, poll.
			return ctrl.Result{RequeueAfter: importContentPollInterval}, nil
		}

		done, dtreason, dtmsg, dErr := r.projectDataLegFromDataImport(ctx, obj, contentName, di)
		if dErr != nil {
			return ctrl.Result{}, dErr
		}
		if dtreason != "" {
			// Actionable import failure (e.g. unsupported artifact kind) surfaced as Ready=False; the content
			// stays pending (no dataRef), so the pure content mirror cannot express it — co-write it directly.
			if perr := r.patchSnapshotReadyFromContent(ctx, obj, snapshotLike, metav1.ConditionFalse, dtreason, dtmsg); perr != nil {
				return ctrl.Result{}, perr
			}
			return ctrl.Result{}, nil
		}
		if !done {
			requeue = true
		} else {
			// Mirror the self-contained data descriptor onto the leaf's top-level status.data for d8 export.
			// storageClassName is absent from the import content data by design, so take it from
			// DataImport.spec.storageClassName; source/artifact/size/volumeMode come from the content
			// status.data (enriched from VSC.restoreSize + DataImport.status.volumeMode).
			scOverride, _, _ := unstructured.NestedString(di.Object, "spec", "storageClassName")
			if mErr := r.mirrorLeafDataFromContent(ctx, obj, contentName, scOverride); mErr != nil {
				logger.Error(mErr, "Failed to mirror volume data to import leaf status")
			}
		}
	}

	if requeue {
		return ctrl.Result{RequeueAfter: importContentPollInterval}, nil
	}

	// Mirror the bound content's Ready (single-aggregator, INV-COND4). The content->snapshot watch wakes
	// this leaf on the Ready transition; the requeue is a missed-event fallback.
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

// projectDataLegFromDataImport resolves the (reverse-looked-up) DataImport's produced durable artifact
// (status.data.artifact), transfers VolumeSnapshotContent ownership to the SnapshotContent (force
// Retain + ownerRef), and publishes the single dataRef. Returns done=true once the dataRef is published.
// A non-empty terminalReason is an actionable, non-retryable import failure.
func (r *GenericSnapshotBinderController) projectDataLegFromDataImport(
	ctx context.Context,
	obj *unstructured.Unstructured,
	contentName string,
	di *unstructured.Unstructured,
) (done bool, terminalReason string, terminalMessage string, err error) {
	binding, ready, treason, tmsg := buildImportDataBinding(di, obj)
	if treason != "" {
		return false, treason, tmsg, nil
	}
	if !ready {
		// DataImport has not produced its artifact yet (status.data.artifact empty). Pending.
		return false, "", "", nil
	}

	content := &storagev1alpha1.SnapshotContent{}
	if cErr := r.Get(ctx, client.ObjectKey{Name: contentName}, content); cErr != nil {
		return false, "", "", cErr
	}
	// Fast-path: skip re-enriching/re-publishing only when the already-published dataRef matches both the
	// artifact and the (now source-derived) volumeMode. Comparing volumeMode too lets a content that was
	// bound before volumeMode propagation existed self-heal instead of staying stuck with an empty mode
	// that fails restore closed.
	if content.Status.Data != nil &&
		content.Status.Data.Artifact == binding.Artifact &&
		content.Status.Data.VolumeMode == binding.VolumeMode {
		return true, "", "", nil
	}

	enriched, enrichErr := snapshotcontent.EnrichDataBindingsWithVolumeMetadata(ctx, r.Client, r.APIReader, []storagev1alpha1.SnapshotDataBinding{*binding})
	if enrichErr != nil {
		return false, "", "", enrichErr
	}
	if cErr := r.Get(ctx, client.ObjectKey{Name: contentName}, content); cErr != nil {
		return false, "", "", cErr
	}
	if handoffErr := snapshotcontent.EnsureVolumeSnapshotContentsOwnedByContent(ctx, r.Client, content, enriched); handoffErr != nil {
		// Retryable handoff (e.g. VSC not yet visible / conflicted); poll without a terminal condition.
		return false, "", "", nil
	}
	if pubErr := snapshotcontent.PublishSnapshotContentDataRef(ctx, r.Client, contentName, &enriched[0]); pubErr != nil {
		return false, "", "", pubErr
	}
	return true, "", "", nil
}

// buildImportDataBinding maps a DataImport's produced artifact (status.data.artifact) into the single
// SnapshotDataBinding for the imported leaf's content. ready=false (binding nil, no terminal reason) means
// the DataImport has not produced its artifact yet. A non-empty terminalReason is a non-retryable fault.
//
// Pure function (no client) so it is unit-tested directly.
func buildImportDataBinding(di *unstructured.Unstructured, leaf *unstructured.Unstructured) (binding *storagev1alpha1.SnapshotDataBinding, ready bool, terminalReason string, terminalMessage string) {
	apiVersion, _, _ := unstructured.NestedString(di.Object, "status", "data", "artifact", "apiVersion")
	kind, _, _ := unstructured.NestedString(di.Object, "status", "data", "artifact", "kind")
	name, _, _ := unstructured.NestedString(di.Object, "status", "data", "artifact", "name")
	// uid is best-effort (DataImport fills it from the VCR artifact uid). When empty, the dataRef
	// enricher backfills it from the live VolumeSnapshotContent; when present, it is preserved.
	uid, _, _ := unstructured.NestedString(di.Object, "status", "data", "artifact", "uid")
	if apiVersion == "" || kind == "" || name == "" {
		return nil, false, "", ""
	}
	if kind != snapshot.KindVolumeSnapshotContent {
		// PV-backed (Detach) artifacts need the PersistentVolume data-readiness path (follow-up). Fail loud
		// rather than publishing a dataRef the SnapshotContent readiness cannot validate as Ready.
		return nil, false, snapshot.ReasonDataArtifactInvalid,
			fmt.Sprintf("DataImport %s produced a %q data artifact; import dataRef currently supports %s only",
				di.GetName(), kind, snapshot.KindVolumeSnapshotContent)
	}
	leafGVK := leaf.GetObjectKind().GroupVersionKind()
	// volumeMode is the one piece of source volume metadata that downstream restore strictly requires
	// (demo restore fails closed on an empty dataRef.volumeMode) and that EnrichDataBindingsWithVolumeMetadata
	// cannot recover here: the binding targets the leaf snapshot, not a live PVC, so the PVC-based enricher
	// only fills Size. DataImport republishes the original captured volumeMode into status.volumeMode (it
	// reads capacity/storageClass/volumeMode from the uploaded manifest to provision its scratch PVC), so it
	// is the authoritative source on the import side. storageClassName/accessModes/fsType are not exposed by
	// DataImport and are resolved downstream from the disk spec / defaults.
	volumeMode, _, _ := unstructured.NestedString(di.Object, "status", "volumeMode")
	return &storagev1alpha1.SnapshotDataBinding{
		// The imported leaf has no live source PVC; use the leaf identity as the binding source so the
		// data binding is stable/idempotent (size etc. are enriched from VolumeSnapshotContent.status.restoreSize).
		Source: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: leafGVK.GroupVersion().String(),
			Kind:       leafGVK.Kind,
			Namespace:  leaf.GetNamespace(),
			Name:       leaf.GetName(),
			UID:        leaf.GetUID(),
		},
		Artifact: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: apiVersion,
			Kind:       kind,
			Name:       name,
			UID:        types.UID(uid),
		},
		VolumeMode: volumeMode,
	}, true, "", ""
}
