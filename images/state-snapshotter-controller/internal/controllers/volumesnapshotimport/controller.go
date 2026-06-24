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

// Package volumesnapshotimport binds IMPORT-mode generic-PVC leaves: extended CSI VolumeSnapshots whose
// spec.source.dataImportName references a DataImport (the C2 extended-VS source fork). The forked
// snapshot-controller skips these VolumeSnapshots, so this common controller is the sole binder for them.
//
// It is the generic-PVC twin of the domain data-leaf import branch in genericbinder: it materializes the
// backing cluster-scoped SnapshotContent (deletionPolicy=Delete) from the uploaded ManifestCheckpoint and
// the DataImport's produced VolumeSnapshotContent, transfers the VSC into the content (force Retain +
// ownerRef), and writes the binding onto the VolumeSnapshot — both our extended status.boundSnapshotContentName
// and the legacy CSI status.boundVolumeSnapshotContentName/readyToUse (so the VS reads as a bound, ready
// snapshot pointing at the imported VSC). SnapshotContentController owns Ready; the parent aggregates this
// leaf through its childrenSnapshotContentRefs.
package volumesnapshotimport

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotbinding"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

const (
	// importPollInterval is the polling fallback while the import is converging (upload not yet present,
	// DataImport artifact not yet produced). No DataImport/MCP watch is taken; this poll drives progress.
	importPollInterval = 5 * time.Second
	// vscRetainPolicy keeps the bound VSC durable after the per-run VolumeSnapshot is deleted.
	vscRetainPolicy = "Retain"
	// kindPersistentVolumeClaim / corePVCAPIVersion identify the orphan PVC manifest carried by an
	// imported leaf inside its reconstructed ManifestCheckpoint (the dataRef target, see importDataBinding).
	kindPersistentVolumeClaim = "PersistentVolumeClaim"
	corePVCAPIVersion         = "v1"
)

var (
	csiVolumeSnapshotGVK = schema.GroupVersionKind{
		Group:   snapshotpkg.CSISnapshotGroup,
		Version: snapshotpkg.CSISnapshotVersion,
		Kind:    snapshotpkg.KindVolumeSnapshot,
	}
	csiVolumeSnapshotContentGVK = schema.GroupVersionKind{
		Group:   snapshotpkg.CSISnapshotGroup,
		Version: snapshotpkg.CSISnapshotVersion,
		Kind:    snapshotpkg.KindVolumeSnapshotContent,
	}
	dataImportGVK = schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "DataImport"}
)

// Controller binds import-mode extended VolumeSnapshots to their materialized SnapshotContent.
type Controller struct {
	client.Client
	APIReader client.Reader
}

// AddToManager registers the import VolumeSnapshot binder. The watch is guarded by RESTMapping so a
// not-yet-installed VolumeSnapshot CRD (e.g. envtest without the extended-VS fork) degrades to "no
// controller" rather than failing manager startup; capture/domain paths are unaffected.
func AddToManager(mgr ctrl.Manager) error {
	if mapper := mgr.GetRESTMapper(); mapper != nil {
		if _, err := mapper.RESTMapping(csiVolumeSnapshotGVK.GroupKind(), csiVolumeSnapshotGVK.Version); err != nil {
			ctrl.Log.WithName("volumesnapshot-import").Info(
				"VolumeSnapshot import binder skipped (GVK not RESTMappable yet)",
				"gvk", csiVolumeSnapshotGVK.String(), "reason", err.Error())
			return nil
		}
	}
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
	return ctrl.NewControllerManagedBy(mgr).
		For(vs, builder.WithPredicates(importVolumeSnapshotPredicate())).
		Named("volumesnapshot-import").
		Complete(&Controller{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()})
}

// importVolumeSnapshotPredicate restricts the controller to extended VolumeSnapshots in import mode
// (spec.source.dataImportName set). Capture VolumeSnapshots (persistentVolumeClaimName) and plain
// pre-provisioned ones (volumeSnapshotContentName) are ignored — those are not ours to bind.
func importVolumeSnapshotPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			return false
		}
		name, _, _ := unstructured.NestedString(u.Object, "spec", "source", "dataImportName")
		return name != ""
	})
}

func (r *Controller) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("volumeSnapshot", req.NamespacedName)

	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
	if err := r.Get(ctx, req.NamespacedName, vs); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !vs.GetDeletionTimestamp().IsZero() {
		// Import VS deleted before its content was bound: best-effort delete the ownerless reconstructed
		// ManifestCheckpoint (per-CR upload creates it ownerless; once content is bound it is adopted + GC'd
		// with the content). Mirrors the namespace Snapshot orchestrator's pre-bind cleanup.
		if contentName, _, _ := unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName"); contentName == "" {
			if err := usecase.DeleteReconstructedManifestCheckpoint(ctx, r.Client, vs.GetUID()); err != nil {
				logger.Error(err, "Failed to delete reconstructed ManifestCheckpoint for deleted import VolumeSnapshot")
			}
		}
		return ctrl.Result{}, nil
	}
	dataImportName, _, _ := unstructured.NestedString(vs.Object, "spec", "source", "dataImportName")
	if dataImportName == "" {
		return ctrl.Result{}, nil
	}

	// Content owner: an imported VS leaf is a child of its parent snapshot (d8 sets child->parent
	// ownerRefs); its SnapshotContent is owned by the parent's SnapshotContent. Wait for the parent.
	ownerRef, pending, err := controllercommon.ResolveParentSnapshotContentOwnerRef(ctx, r.Client, vs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pending || ownerRef == nil {
		return ctrl.Result{RequeueAfter: importPollInterval}, nil
	}

	contentName, _, _ := unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName")
	if contentName == "" {
		contentName = snapshotbinding.StableContentName(vs.GetName(), vs.GetUID())
		content := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{
				Name:            contentName,
				OwnerReferences: []metav1.OwnerReference{*ownerRef},
			},
			// snapshotRef points back at this extended VolumeSnapshot (the binding subject that sets
			// status.boundSnapshotContentName on this content), enabling the restore handshake.
			Spec: controllercommon.NewSnapshotContentSpec(
				storagev1alpha1.SnapshotContentDeletionPolicyDelete,
				controllercommon.SnapshotSubjectRefFromObject(vs),
			),
		}
		if cErr := r.Create(ctx, content); cErr != nil && !errors.IsAlreadyExists(cErr) {
			return ctrl.Result{}, cErr
		}
		if bErr := r.bindBoundSnapshotContentName(ctx, req.NamespacedName, contentName); bErr != nil {
			return ctrl.Result{}, bErr
		}
		logger.Info("Materialized import SnapshotContent for extended VolumeSnapshot", "name", contentName)
		return ctrl.Result{Requeue: true}, nil
	}

	content := &storagev1alpha1.SnapshotContent{}
	if cErr := r.Get(ctx, client.ObjectKey{Name: contentName}, content); cErr != nil {
		return ctrl.Result{}, cErr
	}
	if changed, eErr := controllercommon.EnsureLifecycleOwnerRef(ctx, r.Client, content, *ownerRef); eErr != nil {
		return ctrl.Result{}, eErr
	} else if changed {
		return ctrl.Result{Requeue: true}, nil
	}

	// Manifest leg: the reconstructed ManifestCheckpoint (the PVC manifest), keyed to the VS UID by the
	// per-CR upload endpoint. Hold until it is uploaded.
	mcpName := usecase.ReconstructedManifestCheckpointName(vs.GetUID(), "")
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if mErr := r.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); mErr != nil {
		if errors.IsNotFound(mErr) {
			return ctrl.Result{RequeueAfter: importPollInterval}, nil
		}
		return ctrl.Result{}, mErr
	}
	if pErr := snapshotcontent.PublishSnapshotContentManifestCheckpointName(ctx, r.Client, contentName, mcpName); pErr != nil {
		return ctrl.Result{}, pErr
	}

	// Data leg: resolve the DataImport's produced VolumeSnapshotContent and bind it.
	vscName, ready, terminalMsg, dErr := r.resolveDataImportArtifact(ctx, vs.GetNamespace(), dataImportName)
	if dErr != nil {
		return ctrl.Result{}, dErr
	}
	if terminalMsg != "" {
		// Non-retryable import fault (e.g. a non-VolumeSnapshotContent artifact). Surface it on the VS
		// status.error (operator-visible; the forked snapshot-controller skips these VS so it is ours to
		// write) and stop polling instead of requeueing forever silently.
		if sErr := r.setVolumeSnapshotError(ctx, req.NamespacedName, terminalMsg); sErr != nil {
			return ctrl.Result{}, sErr
		}
		return ctrl.Result{}, nil
	}
	if !ready {
		return ctrl.Result{RequeueAfter: importPollInterval}, nil
	}
	if rErr := r.forceVolumeSnapshotContentRetain(ctx, vscName); rErr != nil {
		return ctrl.Result{}, rErr
	}

	// The dataRef must target the orphan PVC the leaf carries (recovered from the reconstructed
	// ManifestCheckpoint), not the VolumeSnapshot — otherwise the restore compiler cannot bind the PVC
	// to its snapshot and emits a data-less PVC. See importDataBinding.
	//
	// Resolve the PVC only once the checkpoint is Ready: ReconstructManifestCheckpoint writes its chunks
	// and flips Ready in one status update, so a not-yet-Ready (or cache-stale, empty status.chunks)
	// checkpoint is still materializing — poll like the other pending import legs instead of hard-failing
	// the PVC lookup.
	if !meta.IsStatusConditionTrue(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady) {
		return ctrl.Result{RequeueAfter: importPollInterval}, nil
	}
	pvc, pvcTerminal, pvcErr := r.resolveImportedOrphanPVC(ctx, mcp)
	if pvcErr != nil {
		return ctrl.Result{}, pvcErr
	}
	if pvcTerminal != "" {
		// Deterministic manifest fault (no PVC, multiple PVCs, or unreadable chunk content): retrying
		// will not help. Surface it on the VS status.error (as for non-retryable DataImport artifacts)
		// and stop instead of looping forever.
		if sErr := r.setVolumeSnapshotError(ctx, req.NamespacedName, pvcTerminal); sErr != nil {
			return ctrl.Result{}, sErr
		}
		return ctrl.Result{}, nil
	}

	binding := importDataBinding(pvc, vscName)
	enriched, eErr := snapshotcontent.EnrichDataBindingsWithVolumeMetadata(ctx, r.Client, r.APIReader, []storagev1alpha1.SnapshotDataBinding{binding})
	if eErr != nil {
		return ctrl.Result{}, eErr
	}
	if cErr := r.Get(ctx, client.ObjectKey{Name: contentName}, content); cErr != nil {
		return ctrl.Result{}, cErr
	}
	if hErr := snapshotcontent.EnsureVolumeSnapshotContentsOwnedByContent(ctx, r.Client, content, enriched); hErr != nil {
		// Retryable handoff; poll until the VSC is adoptable.
		return ctrl.Result{RequeueAfter: importPollInterval}, nil
	}
	if pErr := snapshotcontent.PublishSnapshotContentDataRef(ctx, r.Client, contentName, &enriched[0]); pErr != nil {
		return ctrl.Result{}, pErr
	}

	// Legacy CSI binding on the VS so it reads as a bound, ready snapshot pointing at the imported VSC.
	if lErr := r.setLegacyVolumeSnapshotBound(ctx, req.NamespacedName, vscName); lErr != nil {
		return ctrl.Result{}, lErr
	}
	return ctrl.Result{}, nil
}

// resolveDataImportArtifact reads the leaf's DataImport and returns its produced VolumeSnapshotContent
// name. The three outcomes are distinguished:
//   - ready=true: a VolumeSnapshotContent artifact has been produced (vscName set);
//   - terminalMessage != "": a non-retryable fault — the DataImport produced a non-VSC artifact (e.g. a
//     PersistentVolume / Detach), which the extended-VS legacy binding cannot represent;
//   - otherwise (pending): the DataImport (or its artifact) is not produced yet — poll.
func (r *Controller) resolveDataImportArtifact(ctx context.Context, namespace, dataImportName string) (vscName string, ready bool, terminalMessage string, err error) {
	di := &unstructured.Unstructured{}
	di.SetGroupVersionKind(dataImportGVK)
	if gErr := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: dataImportName}, di); gErr != nil {
		if errors.IsNotFound(gErr) {
			return "", false, "", nil
		}
		return "", false, "", gErr
	}
	kind, _, _ := unstructured.NestedString(di.Object, "status", "dataArtifactRef", "kind")
	name, _, _ := unstructured.NestedString(di.Object, "status", "dataArtifactRef", "name")
	if name == "" {
		// Artifact not produced yet (kind may be set early, but without a name there is nothing to bind).
		return "", false, "", nil
	}
	if kind != snapshotpkg.KindVolumeSnapshotContent {
		return "", false, fmt.Sprintf(
			"DataImport %s/%s produced a %q data artifact; extended VolumeSnapshot import binding supports %s only",
			namespace, dataImportName, kind, snapshotpkg.KindVolumeSnapshotContent), nil
	}
	return name, true, "", nil
}

// setVolumeSnapshotError records a terminal import fault on the VolumeSnapshot status.error
// (message + time) under an optimistic-lock merge patch (D4a). These VS are skipped by the forked
// snapshot-controller, so status.error is ours to own.
func (r *Controller) setVolumeSnapshotError(ctx context.Context, key client.ObjectKey, message string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vs := &unstructured.Unstructured{}
		vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
		if err := r.Get(ctx, key, vs); err != nil {
			return err
		}
		cur, _, _ := unstructured.NestedString(vs.Object, "status", "error", "message")
		if cur == message {
			return nil
		}
		base := vs.DeepCopy()
		if err := unstructured.SetNestedField(vs.Object, message, "status", "error", "message"); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(vs.Object, time.Now().UTC().Format(time.RFC3339), "status", "error", "time"); err != nil {
			return err
		}
		return r.Status().Patch(ctx, vs, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

// importDataBinding builds the single dataRef binding for the imported orphan-PVC leaf. The binding
// TARGET is the orphan PVC the leaf carries (not the VolumeSnapshot handle): the restore compiler matches
// a captured PVC manifest to its dataRef by the PVC identity/UID (findDataBindingForPVC), so a
// VolumeSnapshot-targeted dataRef would never match and the PVC would be emitted data-less (contract
// violation). This mirrors the capture path (orphanPVCVolumeSnapshotBinding), keeping both paths' dataRef
// shape identical. Size/storageClass etc. are enriched downstream from VolumeSnapshotContent.status.restoreSize.
func importDataBinding(pvc *unstructured.Unstructured, vscName string) storagev1alpha1.SnapshotDataBinding {
	return storagev1alpha1.SnapshotDataBinding{
		TargetUID: string(pvc.GetUID()),
		Target: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: pvc.GetAPIVersion(),
			Kind:       pvc.GetKind(),
			Namespace:  pvc.GetNamespace(),
			Name:       pvc.GetName(),
			UID:        pvc.GetUID(),
		},
		Artifact: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: snapshotpkg.CSISnapshotAPIVersion,
			Kind:       snapshotpkg.KindVolumeSnapshotContent,
			Name:       vscName,
		},
	}
}

// resolveImportedOrphanPVC recovers the single orphan PVC manifest the leaf carries, decoding the
// reconstructed ManifestCheckpoint the import upload produced (the leaf's own manifest leg). The
// published dataRef must target this PVC so the restore compiler can bind it to its VolumeSnapshot
// dataSourceRef; see importDataBinding. APIReader is used (not the cached client): ManifestCheckpoint
// chunks are internal-only and not watched.
//
// The three outcomes mirror resolveDataImportArtifact:
//   - pvc != nil: the single orphan PVC was recovered;
//   - terminalMessage != "": a deterministic, non-retryable manifest fault (no PVC, multiple PVCs, or
//     unreadable/corrupt chunk content) — the caller records it on the VS status.error and stops;
//   - err != nil: a transient API read failure — the caller requeues.
func (r *Controller) resolveImportedOrphanPVC(ctx context.Context, mcp *ssv1alpha1.ManifestCheckpoint) (pvc *unstructured.Unstructured, terminalMessage string, err error) {
	objects, lErr := usecase.CollectReconstructedManifestObjects(ctx, r.APIReader, mcp)
	if lErr != nil {
		if stderrors.Is(lErr, usecase.ErrCorruptManifestChunk) {
			// Bad stored bytes (base64/gzip/JSON/checksum): retrying the same chunk cannot succeed.
			return nil, fmt.Sprintf("imported orphan-PVC leaf checkpoint %s is unreadable: %v", mcp.GetName(), lErr), nil
		}
		// Chunk fetch failure (any API/network error): transient, requeue.
		return nil, "", fmt.Errorf("load imported leaf manifests from %s: %w", mcp.GetName(), lErr)
	}
	for i := range objects {
		if objects[i].GetKind() != kindPersistentVolumeClaim || objects[i].GetAPIVersion() != corePVCAPIVersion {
			continue
		}
		if pvc != nil {
			return nil, fmt.Sprintf("imported orphan-PVC leaf checkpoint %s carries more than one PersistentVolumeClaim", mcp.GetName()), nil
		}
		obj := objects[i]
		pvc = &obj
	}
	if pvc == nil {
		return nil, fmt.Sprintf("imported orphan-PVC leaf checkpoint %s carries no PersistentVolumeClaim manifest", mcp.GetName()), nil
	}
	return pvc, "", nil
}

// bindBoundSnapshotContentName writes the extended status.boundSnapshotContentName onto the VS under an
// optimistic-lock merge patch (D4a), then verifies it persisted: on an upstream (non-fork) VolumeSnapshot
// CRD the API server silently prunes this unknown field, so fail loud rather than leaving an unbindable VS.
func (r *Controller) bindBoundSnapshotContentName(ctx context.Context, key client.ObjectKey, contentName string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vs := &unstructured.Unstructured{}
		vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
		if err := r.Get(ctx, key, vs); err != nil {
			return err
		}
		cur, _, _ := unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName")
		if cur == contentName {
			return nil
		}
		base := vs.DeepCopy()
		if err := unstructured.SetNestedField(vs.Object, contentName, "status", "boundSnapshotContentName"); err != nil {
			return err
		}
		if err := r.Status().Patch(ctx, vs, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		persisted, _, perr := unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName")
		if perr != nil {
			return fmt.Errorf("read back VolumeSnapshot %s boundSnapshotContentName: %w", key, perr)
		}
		if persisted != contentName {
			return fmt.Errorf("VolumeSnapshot %s status.boundSnapshotContentName did not persist (got %q, want %q): install the storage-foundation extended-VS fork (status.boundSnapshotContentName)",
				key, persisted, contentName)
		}
		return nil
	})
}

// setLegacyVolumeSnapshotBound writes the native CSI status (boundVolumeSnapshotContentName + readyToUse)
// so the imported VS behaves like a bound, ready snapshot of the imported VSC. The forked snapshot-controller
// does not reconcile dataImportName VolumeSnapshots, so these fields are ours to own (D4a patch).
func (r *Controller) setLegacyVolumeSnapshotBound(ctx context.Context, key client.ObjectKey, vscName string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vs := &unstructured.Unstructured{}
		vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
		if err := r.Get(ctx, key, vs); err != nil {
			return err
		}
		curBound, _, _ := unstructured.NestedString(vs.Object, "status", "boundVolumeSnapshotContentName")
		curReady, _, _ := unstructured.NestedBool(vs.Object, "status", "readyToUse")
		if curBound == vscName && curReady {
			return nil
		}
		base := vs.DeepCopy()
		if err := unstructured.SetNestedField(vs.Object, vscName, "status", "boundVolumeSnapshotContentName"); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(vs.Object, true, "status", "readyToUse"); err != nil {
			return err
		}
		return r.Status().Patch(ctx, vs, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

// forceVolumeSnapshotContentRetain patches the bound VSC spec.deletionPolicy to Retain so deleting the
// per-run VolumeSnapshot/DataImport cannot drop the durable artifact before SnapshotContent owns it.
func (r *Controller) forceVolumeSnapshotContentRetain(ctx context.Context, vscName string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vsc := &unstructured.Unstructured{}
		vsc.SetGroupVersionKind(csiVolumeSnapshotContentGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: vscName}, vsc); err != nil {
			return err
		}
		policy, _, _ := unstructured.NestedString(vsc.Object, "spec", "deletionPolicy")
		if policy == vscRetainPolicy {
			return nil
		}
		base := vsc.DeepCopy()
		if err := unstructured.SetNestedField(vsc.Object, vscRetainPolicy, "spec", "deletionPolicy"); err != nil {
			return err
		}
		return r.Patch(ctx, vsc, client.MergeFrom(base))
	})
}
