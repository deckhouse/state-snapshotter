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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotbinding"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// GenericSnapshotBinderController reconciles registered generic XxxxSnapshot resources.
//
// It owns snapshot -> common SnapshotContent binding, writes status.boundSnapshotContentName,
// and publishes result refs such as SnapshotContent.status.manifestCheckpointName.
// SnapshotContentController validates those refs and owns the Ready condition.
//
// Architecture:
// - Uses dynamic client for low-level get/list operations
// - Converts to typed SnapshotLike interface for business logic
// - Centralized conditions management through pkg/snapshot/conditions
// - Creates SnapshotContent and ObjectKeeper for root snapshots
type GenericSnapshotBinderController struct {
	client.Client
	APIReader client.Reader // Required: for reading ObjectKeeper directly from API server after creation
	Scheme    *runtime.Scheme
	Config    *config.Options

	// GVKRegistry provides centralized GVK resolution
	GVKRegistry *snapshot.GVKRegistry

	// SnapshotGVKs is a list of GVKs that this controller should watch
	// This allows domain modules to register their snapshot types
	SnapshotGVKs []schema.GroupVersionKind

	watchMu                sync.RWMutex
	activeSnapshotWatchSet map[string]struct{} // snapshot GVK String() -> watch registered with manager

	// domainCaptureGVKs holds snapshot GVKs (String()) whose domain controller plans capture out-of-band
	// (creates MCR/VCR/children, publishes demo.status, owns ChildrenSnapshotReady) while this binder owns
	// all SnapshotContent work for them: children/dataRefs projection, VSC ownership handoff, MCR/VCR
	// cleanup and the domain-only capture markers (status.manifestCaptured / status.dataCaptured). Generic
	// (non-domain) kinds keep the MCP-only projection and are never in this set. Guarded by domainCaptureMu.
	domainCaptureMu   sync.RWMutex
	domainCaptureGVKs map[string]struct{}
}

// NewGenericSnapshotBinderController creates a new GenericSnapshotBinderController with validated dependencies
func NewGenericSnapshotBinderController(
	client client.Client,
	apiReader client.Reader,
	scheme *runtime.Scheme,
	cfg *config.Options,
	snapshotGVKs []schema.GroupVersionKind,
) (*GenericSnapshotBinderController, error) {
	if client == nil {
		return nil, fmt.Errorf("Client must not be nil")
	}
	if apiReader == nil {
		return nil, fmt.Errorf("APIReader must not be nil: controllers require APIReader to read ObjectKeeper after creation (UID barrier pattern)")
	}
	if scheme == nil {
		return nil, fmt.Errorf("Scheme must not be nil")
	}
	if cfg == nil {
		return nil, fmt.Errorf("Config must not be nil")
	}

	// Initialize GVK Registry and register known GVKs
	registry := snapshot.NewGVKRegistry()
	for _, gvk := range snapshotGVKs {
		if err := registry.RegisterSnapshotGVK(gvk.Kind, gvk.GroupVersion().String()); err != nil {
			return nil, fmt.Errorf("failed to register Snapshot GVK %s: %w", gvk.String(), err)
		}
		// Also register Content GVK (derived from Snapshot Kind)
		contentKind := gvk.Kind + "Content"
		contentGVK := schema.GroupVersionKind{
			Group:   gvk.Group,
			Version: gvk.Version,
			Kind:    contentKind,
		}
		if err := registry.RegisterSnapshotContentGVK(contentKind, contentGVK.GroupVersion().String()); err != nil {
			return nil, fmt.Errorf("failed to register SnapshotContent GVK %s: %w", contentGVK.String(), err)
		}
	}

	return &GenericSnapshotBinderController{
		Client:                 client,
		APIReader:              apiReader,
		Scheme:                 scheme,
		Config:                 cfg,
		GVKRegistry:            registry,
		SnapshotGVKs:           snapshotGVKs,
		activeSnapshotWatchSet: make(map[string]struct{}),
		domainCaptureGVKs:      make(map[string]struct{}),
	}, nil
}

// MarkDomainCaptureKind records that snapshot GVK is reconciled by a dedicated domain controller for
// planning (MCR/VCR/children + ChildrenSnapshotReady), while this binder owns all SnapshotContent work
// for it (children/dataRefs projection, VSC handoff, MCR/VCR cleanup, capture markers). Idempotent.
func (r *GenericSnapshotBinderController) MarkDomainCaptureKind(gvk schema.GroupVersionKind) {
	r.domainCaptureMu.Lock()
	defer r.domainCaptureMu.Unlock()
	if r.domainCaptureGVKs == nil {
		r.domainCaptureGVKs = make(map[string]struct{})
	}
	r.domainCaptureGVKs[gvk.String()] = struct{}{}
}

func (r *GenericSnapshotBinderController) isDomainCaptureKind(gvk schema.GroupVersionKind) bool {
	r.domainCaptureMu.RLock()
	defer r.domainCaptureMu.RUnlock()
	_, ok := r.domainCaptureGVKs[gvk.String()]
	return ok
}

// MarkDataBacked records whether a snapshot Kind carries a volume data leg (CSD spec.dataBacked) on the
// GVK registry, so the import path knows whether to wait for a DataImport/data artifact. Idempotent;
// guarded by watchMu (the same lock that serializes registry mutations in AddWatchForPair).
func (r *GenericSnapshotBinderController) MarkDataBacked(snapshotKind string, dataBacked bool) {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	r.GVKRegistry.MarkDataBacked(snapshotKind, dataBacked)
}

// isDomainPlanningComplete reports whether the domain controller finished planning for the snapshot's
// current generation: ChildrenSnapshotReady=True with observedGeneration == metadata.generation.
func isDomainPlanningComplete(snapshotLike snapshot.SnapshotLike) bool {
	c := snapshot.GetCondition(snapshotLike, snapshot.ConditionChildrenSnapshotReady)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == snapshotLike.GetGeneration()
}

// Reconcile processes a Snapshot resource
//
// Step 1 (Skeleton): Only create path - no deletion, no propagation
func (r *GenericSnapshotBinderController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("snapshot", req.NamespacedName)
	logger.Info("Reconciling Snapshot")

	// Get the unstructured object
	// We need to try each registered GVK to find the correct one
	// In practice, each controller instance watches a specific GVK
	obj := &unstructured.Unstructured{}
	var found bool
	var err error
	for _, gvk := range r.snapshotGVKsSnapshot() {
		obj.SetGroupVersionKind(gvk)
		err = r.Get(ctx, req.NamespacedName, obj)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			logger.Error(err, "failed to get Snapshot")
			return ctrl.Result{}, err
		}
		found = true
		break
	}

	if !found {
		logger.V(1).Info("Snapshot not found in any registered GVK, skipping")
		return ctrl.Result{}, nil
	}

	// Convert to typed interface
	snapshotLike, err := snapshot.ExtractSnapshotLike(obj)
	if err != nil {
		logger.Error(err, "failed to extract SnapshotLike interface")
		return ctrl.Result{}, err
	}

	// Step 0: Handle deletion.
	//
	// Deleting a child Snapshot OBJECT is NOT a parent-degradation signal: in the durable-content model the
	// child's SnapshotContent (and its artifacts) survives Snapshot deletion via ObjectKeeper TTL, so the
	// parent's data is intact and the parent must stay Ready=True. Real degradation (durable artifact/content
	// loss) propagates UP through SnapshotContent.ChildrenReady aggregation and is mirrored onto parent
	// Snapshots by the content->Snapshot watch (INV-COND2/INV-COND4/INV-FAIL1) — proven by
	// genericbinder_parent_degradation_content_driven_test. The previous recursive propagateReadyFalseToParent
	// (direct Status().Update walk onto ancestor Snapshots) was a pre-conditions-model remnant and has been
	// removed; the snapshot side never recomputes its own Ready/reason.
	if !obj.GetDeletionTimestamp().IsZero() {
		// Import leaf deleted before its content was bound: best-effort delete the ownerless reconstructed
		// ManifestCheckpoint (the per-CR upload creates it ownerless; once content is bound it is adopted +
		// GC'd with the content). Mirrors the namespace Snapshot orchestrator's pre-bind cleanup.
		if snapshotIsImportMode(obj) && snapshotLike.GetStatusContentName() == "" {
			if err := usecase.DeleteReconstructedManifestCheckpoint(ctx, r.Client, obj.GetUID()); err != nil {
				logger.Error(err, "Failed to delete reconstructed ManifestCheckpoint for deleted import leaf")
			}
		}
		// Remove finalizer from SnapshotContent on parent deletion (watch-driven, no reverse content ref).
		if err := r.removeSnapshotContentFinalizer(ctx, snapshotLike, obj); err != nil {
			logger.Error(err, "Failed to remove SnapshotContent finalizer on snapshot deletion")
			// Non-fatal: continue with deletion
		}
		// Snapshot is being deleted - no need to continue create-path
		return ctrl.Result{}, nil
	}

	// Import branch (C5): an import-mode leaf (spec.source.import: {}) has no live capture and no domain
	// planning, so it bypasses the Step-1 barrier below. The same common controller / SnapshotContent
	// materializes its content (manifest leg from the reconstructed ManifestCheckpoint; for dataBacked
	// kinds, the data leg from the reverse-looked-up DataImport's produced artifact) — there is no second
	// content creator.
	if snapshotIsImportMode(obj) {
		return r.reconcileGenericImport(ctx, obj, snapshotLike)
	}

	// Step 1: Barrier - wait until the domain controller finished planning (publish child snapshot
	// refs, create MCR/VCR) for the current generation.
	if !isDomainPlanningComplete(snapshotLike) {
		logger.V(1).Info("Waiting for domain controller to finish planning (ChildrenSnapshotReady)")
		return ctrl.Result{}, nil
	}

	// Idempotency is structural, not condition-based: the steps below (ensure ObjectKeeper/ownerRef,
	// create SnapshotContent only when status.boundSnapshotContentName is empty, publish result links)
	// are all idempotent and converge to the consistency/mirror step. No progress/handled marker
	// condition is needed; status.boundSnapshotContentName is the durable "content created" signal.
	contentName := snapshotLike.GetStatusContentName()

	// Step 2: Create ObjectKeeper for root snapshots first (needed for SnapshotContent ownerRef)
	var contentOwnerRef *metav1.OwnerReference
	if snapshot.IsRootSnapshot(obj) {
		var result ctrl.Result
		var err error
		objectKeeper, result, err := controllercommon.EnsureRootObjectKeeperWithTTL(ctx, r.Client, r.APIReader, r.Config, obj, obj.GetObjectKind().GroupVersionKind())
		if err != nil {
			return ctrl.Result{}, err
		}
		if result.Requeue || result.RequeueAfter > 0 {
			return result, nil
		}
		ref := controllercommon.RootObjectKeeperOwnerReference(objectKeeper)
		contentOwnerRef = &ref
	} else {
		ref, pending, err := controllercommon.ResolveParentSnapshotContentOwnerRef(ctx, r.Client, obj)
		if err != nil {
			return ctrl.Result{}, err
		}
		if pending {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		contentOwnerRef = ref
	}
	if contentName != "" && contentOwnerRef != nil {
		contentGVK, err := r.getSnapshotContentGVK(obj.GetObjectKind().GroupVersionKind())
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to resolve SnapshotContent GVK: %w", err)
		}
		contentObj := &unstructured.Unstructured{}
		contentObj.SetGroupVersionKind(contentGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: contentName}, contentObj); err == nil {
			if changed, err := controllercommon.EnsureLifecycleOwnerRef(ctx, r.Client, contentObj, *contentOwnerRef); err != nil {
				return ctrl.Result{}, err
			} else if changed {
				return ctrl.Result{Requeue: true}, nil
			}
		}
	}

	// Step 3: Create SnapshotContent if it doesn't exist
	if contentName == "" {
		// Generate deterministic name
		contentName = snapshotbinding.StableContentName(obj.GetName(), obj.GetUID())

		// Create SnapshotContent
		snapshotGVK := obj.GetObjectKind().GroupVersionKind()
		if snapshotGVK.Kind == "" {
			// Fallback: try to get Kind from obj.Object directly
			if kind, ok := obj.Object["kind"].(string); ok && kind != "" {
				snapshotGVK.Kind = kind
			} else {
				logger.Error(nil, "Cannot create SnapshotContent: Snapshot Kind is empty and cannot be determined", "obj", obj.GetName())
				return ctrl.Result{}, fmt.Errorf("cannot determine Snapshot Kind for object %s", obj.GetName())
			}
		}

		contentGVK, err := r.getSnapshotContentGVK(snapshotGVK)
		if err != nil {
			logger.Error(err, "Failed to resolve SnapshotContent GVK")
			return ctrl.Result{}, err
		}
		contentObj := &unstructured.Unstructured{}
		contentObj.SetGroupVersionKind(contentGVK)
		contentObj.SetName(contentName)
		// SnapshotContent is cluster-scoped, no namespace

		// Minimal spec: durable by default (deletionPolicy=Retain), same as the namespace path
		// (snapshot/controller.go). The spec is immutable; all data/result wiring is published
		// into content.status by the owning controller, never carried in spec. snapshotRef is the
		// required anti-spoofing back-reference to this domain XXXSnapshot (the binding subject that sets
		// status.boundSnapshotContentName on the content).
		contentObj.Object["spec"] = map[string]interface{}{
			"deletionPolicy": "Retain",
			"snapshotRef": map[string]interface{}{
				"apiVersion": snapshotGVK.GroupVersion().String(),
				"kind":       snapshotGVK.Kind,
				"namespace":  obj.GetNamespace(),
				"name":       obj.GetName(),
				"uid":        string(obj.GetUID()),
			},
		}

		if contentOwnerRef == nil {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		contentObj.SetOwnerReferences([]metav1.OwnerReference{*contentOwnerRef})

		if err := r.Create(ctx, contentObj); err != nil {
			logger.Error(err, "Failed to create SnapshotContent", "name", contentName)
			return ctrl.Result{}, err
		}
		logger.Info("Created SnapshotContent", "name", contentName, "owner", contentOwnerRef.Kind)

		if err := snapshotbinding.PatchUnstructuredBoundContentName(ctx, r.Client, req.NamespacedName, snapshotGVK, contentName); err != nil {
			logger.Error(err, "Failed to update Snapshot status.boundSnapshotContentName")
			return ctrl.Result{}, err
		}
		// Log both field names for backward compatibility with log parsers
		logger.Info("Updated Snapshot status.boundSnapshotContentName", "boundSnapshotContentName", contentName, "contentName", contentName)
		return ctrl.Result{Requeue: true}, nil
	}

	// Step 4: Populate SnapshotContent links from MCR/VCR (if present and Ready)
	if contentName != "" {
		requeue, terminalReason, terminalMessage, err := r.ensureSnapshotContentLinks(ctx, snapshotLike, obj, contentName)
		if err != nil {
			logger.Error(err, "Failed to ensure SnapshotContent links")
			return ctrl.Result{}, err
		}
		if terminalReason != "" {
			// Actionable capture failure (e.g. data-leg VolumeCaptureRequest failed) surfaced as a
			// Ready=False on the snapshot. The bound SnapshotContent stays pending (no dataRefs), so the
			// pure content mirror could not express this terminal reason; the binder co-writes it directly.
			if perr := r.patchSnapshotReadyFromContent(ctx, obj, snapshotLike, metav1.ConditionFalse, terminalReason, terminalMessage); perr != nil {
				return ctrl.Result{}, perr
			}
			return ctrl.Result{}, nil
		}
		if requeue {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		// Mirror the captured volume metadata (storageClassName/size/volumeMode) from the bound content's
		// dataRef onto the data leaf snapshot status, so d8 reads it on export. dataBacked leaves only —
		// manifest-only kinds have no dataRef. No-op until the content publishes a dataRef.
		if r.GVKRegistry.IsDataBacked(obj.GetObjectKind().GroupVersionKind().Kind) {
			if err := r.mirrorLeafVolumeMetadataFromContent(ctx, obj, contentName, ""); err != nil {
				logger.Error(err, "Failed to mirror captured volume metadata to leaf status")
			}
		}
	}

	// Step 5: Check consistency and set Ready condition
	// Only check if SnapshotContent already exists (has been processed by SnapshotContentController)
	// This avoids checking consistency in a "half-assembled" state where SnapshotContent
	// might not have finalizer or Ready condition yet. The bound SnapshotContent change wakes this
	// Snapshot event-driven via the reverse watch (registerSnapshotWatch -> mapBoundContentToSnapshots),
	// so the 5s requeue below is only a fallback safety net, not the primary Ready-mirror driver.
	if snapshotLike.GetStatusContentName() != "" {
		if err := r.checkConsistencyAndSetReady(ctx, snapshotLike, obj); err != nil {
			logger.Error(err, "Failed to check consistency after creating SnapshotContent")
			// Non-fatal: will retry on next reconcile
		}
		if !snapshot.IsReady(snapshotLike) {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	logger.Info("Snapshot reconciliation completed (create path)")
	return ctrl.Result{}, nil
}

// ensureSnapshotContentLinks publishes generic result refs into the bound SnapshotContent.
// SnapshotContentController must not read live Snapshot or MCR objects; it only validates
// refs already persisted on SnapshotContent.status.
//
// For generic kinds it publishes only the manifest checkpoint from the snapshot's MCR. For domain
// capture kinds (see MarkDomainCaptureKind) it additionally projects children + dataRefs, performs the
// VolumeSnapshotContent ownership handoff, cleans up the domain MCR/VCR after a durable handoff, and
// stamps the domain-only capture markers (status.manifestCaptured / status.dataCaptured) the domain
// controller reads to stop re-creating its requests. A non-empty terminalReason is an actionable
// capture failure to surface as Ready=False.
func (r *GenericSnapshotBinderController) ensureSnapshotContentLinks(
	ctx context.Context,
	_ snapshot.SnapshotLike,
	obj *unstructured.Unstructured,
	contentName string,
) (requeue bool, terminalReason string, terminalMessage string, err error) {
	mcrName, _, err := unstructured.NestedString(obj.Object, "status", "manifestCaptureRequestName")
	if err != nil {
		return false, "", "", err
	}
	if mcrName != "" {
		mcr := &ssv1alpha1.ManifestCaptureRequest{}
		if getErr := r.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: mcrName}, mcr); getErr != nil {
			if errors.IsNotFound(getErr) {
				requeue = true
			} else {
				return false, "", "", getErr
			}
		} else if mcr.Status.CheckpointName == "" {
			requeue = true
		} else if pubErr := snapshotcontent.PublishSnapshotContentManifestCheckpointName(ctx, r.Client, contentName, mcr.Status.CheckpointName); pubErr != nil {
			return false, "", "", pubErr
		}
	}

	if !r.isDomainCaptureKind(obj.GetObjectKind().GroupVersionKind()) {
		return requeue, "", "", nil
	}

	domainRequeue, treason, tmsg, derr := r.ensureDomainContentLinks(ctx, obj, contentName, mcrName)
	if derr != nil {
		return false, "", "", derr
	}
	return requeue || domainRequeue, treason, tmsg, nil
}

func (r *GenericSnapshotBinderController) removeSnapshotContentFinalizer(
	ctx context.Context,
	snapshotLike snapshot.SnapshotLike,
	obj *unstructured.Unstructured,
) error {
	contentName := snapshotLike.GetStatusContentName()
	if contentName == "" && obj.GetUID() != "" {
		// Fallback to deterministic name to avoid race when status not yet set.
		// UID is available only after the Snapshot is persisted.
		contentName = snapshotbinding.StableContentName(obj.GetName(), obj.GetUID())
	}
	if contentName == "" {
		return nil
	}

	contentGVK, err := r.getSnapshotContentGVK(obj.GetObjectKind().GroupVersionKind())
	if err != nil {
		return fmt.Errorf("failed to resolve SnapshotContent GVK: %w", err)
	}

	contentObj := &unstructured.Unstructured{}
	contentObj.SetGroupVersionKind(contentGVK)
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: contentName}, contentObj); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	updated := false
	annotations := contentObj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if annotations[snapshot.AnnotationParentDeleted] != "true" {
		annotations[snapshot.AnnotationParentDeleted] = "true"
		contentObj.SetAnnotations(annotations)
		updated = true
	}
	if snapshot.RemoveFinalizer(contentObj, snapshot.FinalizerParentProtect) {
		updated = true
		log.FromContext(ctx).Info("Removed finalizer from SnapshotContent after Snapshot deletion", "content", contentName)
	}
	if updated {
		if err := r.Update(ctx, contentObj); err != nil {
			return err
		}
	}

	return nil
}

// ensureObjectKeeper creates or gets ObjectKeeper for root snapshot
// Returns ObjectKeeper, ctrl.Result (for requeue), and error
// contentName is optional - used only for updating ownerRef if ObjectKeeper already exists
func (r *GenericSnapshotBinderController) ensureObjectKeeper(
	ctx context.Context,
	_ snapshot.SnapshotLike,
	obj *unstructured.Unstructured,
	contentName string,
) (*deckhousev1alpha1.ObjectKeeper, ctrl.Result, error) {
	logger := log.FromContext(ctx)
	retainerName := snapshot.GenerateObjectKeeperName(obj.GetKind(), obj.GetName())

	objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
	err := r.Get(ctx, client.ObjectKey{Name: retainerName}, objectKeeper)

	switch {
	case errors.IsNotFound(err):
		// Root snapshot: ObjectKeeper always FollowObjectWithTTL on the snapshot; TTL from config (env or default).
		wantSpec := r.desiredUnifiedRootObjectKeeperSpec(obj)
		objectKeeper = &deckhousev1alpha1.ObjectKeeper{
			TypeMeta: metav1.TypeMeta{
				APIVersion: controllercommon.DeckhouseAPIVersion,
				Kind:       controllercommon.KindObjectKeeper,
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: retainerName,
			},
			Spec: wantSpec,
		}

		if err := r.Create(ctx, objectKeeper); err != nil {
			return nil, ctrl.Result{}, fmt.Errorf("failed to create ObjectKeeper: %w", err)
		}
		logger.Info("Created ObjectKeeper", "name", retainerName)

		// UID barrier: Re-read ObjectKeeper via APIReader to get UID
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: retainerName}, objectKeeper); err != nil {
			return nil, ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
		}

		// If SnapshotContent already exists, update its ownerRef to ObjectKeeper
		if contentName != "" {
			contentGVK, err := r.getSnapshotContentGVK(obj.GetObjectKind().GroupVersionKind())
			if err != nil {
				return nil, ctrl.Result{}, fmt.Errorf("failed to resolve SnapshotContent GVK: %w", err)
			}
			contentObj := &unstructured.Unstructured{}
			contentObj.SetGroupVersionKind(contentGVK)
			if err := r.Get(ctx, client.ObjectKey{Name: contentName}, contentObj); err == nil {
				// Update ownerRef: SnapshotContent is retained by ObjectKeeper.
				contentObj.SetOwnerReferences([]metav1.OwnerReference{
					{
						APIVersion: controllercommon.DeckhouseAPIVersion,
						Kind:       controllercommon.KindObjectKeeper,
						Name:       retainerName,
						UID:        objectKeeper.UID,
						Controller: func() *bool { b := true; return &b }(),
					},
				})

				if err := r.Update(ctx, contentObj); err != nil {
					logger.Error(err, "Failed to update SnapshotContent ownerRef to ObjectKeeper", "contentName", contentName)
					// Non-fatal: will be retried on next reconcile
				} else {
					logger.Info("Updated SnapshotContent ownerRef to ObjectKeeper", "contentName", contentName, "objectKeeper", retainerName)
				}
			}
		}

		return objectKeeper, ctrl.Result{}, nil

	case err != nil:
		return nil, ctrl.Result{}, fmt.Errorf("failed to get ObjectKeeper: %w", err)

	default:
		// ObjectKeeper exists — same snapshot UID; align spec (mode/TTL/followRef) with current config.
		if objectKeeper.Spec.FollowObjectRef == nil {
			return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s has no FollowObjectRef", retainerName)
		}
		if objectKeeper.Spec.FollowObjectRef.UID != string(obj.GetUID()) {
			return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s belongs to another Snapshot (UID mismatch)", retainerName)
		}
		wantSpec := r.desiredUnifiedRootObjectKeeperSpec(obj)
		if !genericBinderObjectKeeperSpecMatches(&wantSpec, objectKeeper) {
			objectKeeper.Spec = wantSpec
			if err := r.Update(ctx, objectKeeper); err != nil {
				return nil, ctrl.Result{}, fmt.Errorf("update ObjectKeeper %s (spec drift): %w", retainerName, err)
			}
			logger.Info("updated unified root ObjectKeeper spec (TTL/mode drift)", "name", retainerName)
			if err := r.APIReader.Get(ctx, client.ObjectKey{Name: retainerName}, objectKeeper); err != nil {
				return nil, ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
			}
		}
		logger.V(1).Info("ObjectKeeper already exists", "name", retainerName)
		return objectKeeper, ctrl.Result{}, nil
	}
}

func (r *GenericSnapshotBinderController) desiredUnifiedRootObjectKeeperSpec(obj *unstructured.Unstructured) deckhousev1alpha1.ObjectKeeperSpec {
	gvk := obj.GetObjectKind().GroupVersionKind()
	ttl := config.DefaultSnapshotRootOKTTL
	if r.Config != nil && r.Config.SnapshotRootOKTTL > 0 {
		ttl = r.Config.SnapshotRootOKTTL
	}
	return deckhousev1alpha1.ObjectKeeperSpec{
		Mode: controllercommon.ObjectKeeperModeFollowObjectWithTTL,
		FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
			APIVersion: gvk.GroupVersion().String(),
			Kind:       gvk.Kind,
			Namespace:  obj.GetNamespace(),
			Name:       obj.GetName(),
			UID:        string(obj.GetUID()),
		},
		TTL: &metav1.Duration{Duration: ttl},
	}
}

func genericBinderObjectKeeperSpecMatches(want *deckhousev1alpha1.ObjectKeeperSpec, got *deckhousev1alpha1.ObjectKeeper) bool {
	if got.Spec.Mode != want.Mode {
		return false
	}
	if got.Spec.FollowObjectRef == nil || want.FollowObjectRef == nil {
		return false
	}
	fr, w := got.Spec.FollowObjectRef, want.FollowObjectRef
	if fr.APIVersion != w.APIVersion || fr.Kind != w.Kind || fr.Name != w.Name || fr.Namespace != w.Namespace || fr.UID != w.UID {
		return false
	}
	if got.Spec.TTL == nil || want.TTL == nil || got.Spec.TTL.Duration != want.TTL.Duration {
		return false
	}
	return true
}

// checkConsistencyAndSetReady mirrors the bound SnapshotContent Ready condition.
// GenericSnapshotBinderController does not aggregate children; SnapshotContent is
// the single source of truth for final readiness.
func (r *GenericSnapshotBinderController) checkConsistencyAndSetReady(
	ctx context.Context,
	snapshotLike snapshot.SnapshotLike,
	obj *unstructured.Unstructured,
) error {
	logger := log.FromContext(ctx)
	contentName := snapshotLike.GetStatusContentName()
	// ChildrenSnapshotReady is intentionally NOT written here. This function is a pure Ready mirror and is only
	// reached after the Step-1 barrier (isDomainPlanningComplete) has already confirmed ChildrenSnapshotReady=True
	// for the current generation. ChildrenSnapshotReady is owned exclusively by the domain/namespace controller that
	// plans the snapshot; the common layer (this binder and the shared binding helpers) only waits on the
	// barrier and MUST NOT self-publish it (Slice 2). Re-asserting it here previously clobbered that owner:
	// with observedGeneration=0 it deadlocked this very barrier (mirror never re-ran), and stamping the
	// current generation would instead overwrite the domain controller's ChildrenSnapshotReady.
	if contentName == "" {
		return r.patchSnapshotReadyFromContent(ctx, obj, snapshotLike, metav1.ConditionFalse, snapshot.ReasonContentMissing, "SnapshotContent is not bound")
	}

	contentGVK, err := r.getSnapshotContentGVK(obj.GetObjectKind().GroupVersionKind())
	if err != nil {
		return fmt.Errorf("failed to resolve SnapshotContent GVK: %w", err)
	}
	contentObj := &unstructured.Unstructured{}
	contentObj.SetGroupVersionKind(contentGVK)
	contentKey := client.ObjectKey{Name: contentName}

	if err := r.APIReader.Get(ctx, contentKey, contentObj); err != nil {
		if errors.IsNotFound(err) {
			return r.patchSnapshotReadyFromContent(ctx, obj, snapshotLike, metav1.ConditionFalse, snapshot.ReasonContentMissing, fmt.Sprintf("SnapshotContent %s not found", contentName))
		}
		return fmt.Errorf("failed to get SnapshotContent: %w", err)
	}

	if !contentObj.GetDeletionTimestamp().IsZero() {
		return r.patchSnapshotReadyFromContent(ctx, obj, snapshotLike, metav1.ConditionFalse, snapshot.ReasonDeleting, fmt.Sprintf("SnapshotContent %s is being deleted", contentName))
	}

	contentLike, err := snapshot.ExtractSnapshotContentLike(contentObj)
	if err != nil {
		return fmt.Errorf("failed to extract SnapshotContentLike: %w", err)
	}
	readyCond := snapshot.GetCondition(contentLike, snapshot.ConditionReady)
	status := metav1.ConditionFalse
	reason := snapshot.ReasonContentMissing
	message := fmt.Sprintf("SnapshotContent %s has no Ready condition", contentName)
	if readyCond != nil {
		status = readyCond.Status
		reason = readyCond.Reason
		message = readyCond.Message
	}
	logger.V(1).Info("Mirroring SnapshotContent Ready", "content", contentName, "status", status, "reason", reason)
	if err := r.patchSnapshotReadyFromContent(ctx, obj, snapshotLike, status, reason, message); err != nil {
		return err
	}

	// Mirror the ManifestsArchived subtree latch (NOT part of the Ready formula). The RBAC hook reads it
	// off the root Snapshot to drop the transient capture RoleBinding once the subtree is archived. The
	// content-side latch never re-opens, so this verbatim mirror is also monotone. If the content has no
	// ManifestsArchived condition yet, skip (absent == still capturing for downstream readers).
	if archivedCond := snapshot.GetCondition(contentLike, snapshot.ConditionManifestsArchived); archivedCond != nil {
		logger.V(1).Info("Mirroring SnapshotContent ManifestsArchived", "content", contentName, "status", archivedCond.Status, "reason", archivedCond.Reason)
		return r.patchSnapshotConditionFromContent(ctx, obj, snapshotLike, snapshot.ConditionManifestsArchived,
			archivedCond.Status, archivedCond.Reason, archivedCond.Message)
	}
	return nil
}

func (r *GenericSnapshotBinderController) patchSnapshotReadyFromContent(
	ctx context.Context,
	obj *unstructured.Unstructured,
	snapshotLike snapshot.SnapshotLike,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	return r.patchSnapshotConditionFromContent(ctx, obj, snapshotLike, snapshot.ConditionReady, status, reason, message)
}

// patchSnapshotConditionFromContent mirrors a single condition type (Ready or ManifestsArchived) from
// the bound SnapshotContent onto the Snapshot, gen-stamped under an optimistic-lock merge patch.
//
// D4a: read-modify-write only the target condition. The demo domain controller co-writes
// ChildrenSnapshotReady (and an early validation Ready=False) into the same conditions array; a bare
// Status().Update / MergeFrom would replace the whole list and could silently drop the other writer's
// entry. MergeFromWithOptimisticLock turns a concurrent write into a 409 so RetryOnConflict re-reads the
// fresh object (already carrying the other condition) and re-applies only this condition, stamping
// observedGeneration for gen-gated readers (INV-DOMAIN-GEN).
func (r *GenericSnapshotBinderController) patchSnapshotConditionFromContent(
	ctx context.Context,
	obj *unstructured.Unstructured,
	snapshotLike snapshot.SnapshotLike,
	condType string,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	// Fast path: nothing to do if the in-memory view already matches the desired condition.
	if cur := snapshot.GetCondition(snapshotLike, condType); cur != nil &&
		cur.Status == status && cur.Reason == reason && cur.Message == message &&
		cur.ObservedGeneration == obj.GetGeneration() {
		return nil
	}

	gvk := obj.GetObjectKind().GroupVersionKind()
	key := client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(gvk)
		if err := r.Get(ctx, key, fresh); err != nil {
			return err
		}
		freshLike, err := snapshot.ExtractSnapshotLike(fresh)
		if err != nil {
			return err
		}
		gen := fresh.GetGeneration()
		if cur := snapshot.GetCondition(freshLike, condType); cur != nil &&
			cur.Status == status && cur.Reason == reason && cur.Message == message &&
			cur.ObservedGeneration == gen {
			return nil
		}
		base := fresh.DeepCopy()
		snapshot.SetCondition(freshLike, condType, status, reason, message)
		conds := freshLike.GetStatusConditions()
		for i := range conds {
			if conds[i].Type == condType {
				conds[i].ObservedGeneration = gen
			}
		}
		freshLike.SetStatusConditions(conds)
		snapshot.SyncConditionsToUnstructured(fresh, freshLike.GetStatusConditions())
		if err := r.Status().Patch(ctx, fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("failed to mirror SnapshotContent %s: %w", condType, err)
		}
		return nil
	})
}

// checkChildSnapshotExists checks if a child Snapshot exists
// Uses APIReader for read-after-write consistency
func (r *GenericSnapshotBinderController) checkChildSnapshotExists(ctx context.Context, childRef *snapshot.ObjectRef) (bool, error) {
	if childRef == nil {
		return false, nil
	}

	// Resolve child Snapshot GVK through registry
	childGVK, err := r.GVKRegistry.ResolveSnapshotGVK(childRef.Kind)
	if err != nil {
		// Fallback: try to find matching GVK from registered list
		// This handles cases where child snapshot type is not yet registered
		logger := log.FromContext(ctx)
		logger.V(1).Info("Child GVK not found in registry, trying registered GVKs", "kind", childRef.Kind)

		// Try to find a matching GVK by Kind
		for _, gvk := range r.snapshotGVKsSnapshot() {
			if gvk.Kind == childRef.Kind {
				childGVK = gvk
				break
			}
		}

		// If still not found, return error
		if childGVK.Kind == "" {
			return false, fmt.Errorf("child Snapshot GVK not found for kind %s: %w", childRef.Kind, err)
		}
	}

	childObj := &unstructured.Unstructured{}
	childObj.SetGroupVersionKind(childGVK)
	childKey := client.ObjectKey{
		Name:      childRef.Name,
		Namespace: childRef.Namespace,
	}

	getErr := r.APIReader.Get(ctx, childKey, childObj)
	if errors.IsNotFound(getErr) {
		return false, nil
	}
	if getErr != nil {
		return false, fmt.Errorf("failed to get child Snapshot: %w", getErr)
	}

	return true, nil
}

// getSnapshotContentGVK derives SnapshotContent GVK from Snapshot GVK using registry
// Example: virtualization.deckhouse.io/v1alpha1.VirtualMachineSnapshot -> virtualization.deckhouse.io/v1alpha1.VirtualMachineSnapshotContent
func (r *GenericSnapshotBinderController) getSnapshotContentGVK(snapshotGVK schema.GroupVersionKind) (schema.GroupVersionKind, error) {
	return r.GVKRegistry.ResolveSnapshotContentGVK(snapshotGVK.Kind)
}

func (r *GenericSnapshotBinderController) snapshotGVKsSnapshot() []schema.GroupVersionKind {
	r.watchMu.RLock()
	defer r.watchMu.RUnlock()
	out := make([]schema.GroupVersionKind, len(r.SnapshotGVKs))
	copy(out, r.SnapshotGVKs)
	return out
}

// genericBinderControllerOptions parallelizes reconciles across DISTINCT snapshots so a capture wave
// (e.g. a VM snapshot + its child disk + a standalone disk) is not serialized through a single worker.
// controller-runtime still serializes reconciles of the SAME object key, so distinct snapshots are the
// only thing that runs in parallel here; the binder's cross-object writes are conflict-safe:
//   - SnapshotContent / Snapshot status writes use optimistic-lock merge patches (MergeFromWithOptimisticLock)
//     and RetryOnConflict, and children-ref publication preserves the other writer's edges (status_publish.go).
//   - Create paths are idempotent (Get-then-Create tolerating AlreadyExists).
//
// Start conservative at 4; raising to 8 (matching Snapshot/SnapshotContent) should follow only after the
// L0a trace confirms no conflict-retry pressure. The rate limiter mirrors the other ssc controllers
// (200ms floor -> 10s ceiling) so a wedged item re-plans tightly without hot-looping.
func genericBinderControllerOptions() controller.Options {
	return controller.Options{
		MaxConcurrentReconciles: 4,
		RateLimiter:             workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](200*time.Millisecond, 10*time.Second),
	}
}

// registerSnapshotWatch calls builder.Complete. When the manager is already running, this relies on
// controller-runtime allowing new runnables via Add — behavior is runtime-sensitive; upgrade c-r with care.
func (r *GenericSnapshotBinderController) registerSnapshotWatch(mgr ctrl.Manager, gvk, contentGVK schema.GroupVersionKind) error {
	// Guard: contentGVK must be a real GVK. An empty Kind would register the reverse content wake-up
	// (.Watches) on a bogus unstructured GVK and silently never fire. All callers (SetupWithManager via
	// getSnapshotContentGVK, AddWatchForPair via the resolved/paired content GVK) are expected to pass a
	// non-empty content GVK; fail fast here so a regression in any callsite cannot create a dead watch.
	if contentGVK.Kind == "" {
		return fmt.Errorf("empty SnapshotContent GVK for Snapshot %s: refusing to register content wake-up watch", gvk.String())
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	contentObj := &unstructured.Unstructured{}
	contentObj.SetGroupVersionKind(contentGVK)
	builder := ctrl.NewControllerManagedBy(mgr).
		For(obj).
		// Reverse wake-up so Snapshot.Ready mirrors the bound SnapshotContent.Ready in both directions
		// (including Ready=True -> False after the binder stopped polling). Enqueue-only; truth stays on
		// SnapshotContent. See mapBoundContentToSnapshots.
		Watches(contentObj, handler.EnqueueRequestsFromMapFunc(r.mapBoundContentToSnapshots(gvk))).
		// Event-driven parent->child unblock: when a PARENT SnapshotContent appears/changes, wake the child
		// snapshots of this GVK that are waiting to resolve their parent's content ownerRef. Replaces the
		// per-poll re-check that previously gated children on the Reconcile RequeueAfter fallback. See
		// mapParentContentToChildSnapshots.
		Watches(contentObj, handler.EnqueueRequestsFromMapFunc(r.mapParentContentToChildSnapshots(gvk))).
		// Event-driven capture handoff: when an MCR publishes its checkpoint, wake the owning snapshot so
		// the binder publishes SnapshotContent.status.manifestCheckpointName immediately instead of on the
		// next poll. See mapMCRToOwningSnapshots.
		Watches(&ssv1alpha1.ManifestCaptureRequest{}, handler.EnqueueRequestsFromMapFunc(r.mapMCRToOwningSnapshots(gvk))).
		WithOptions(genericBinderControllerOptions()).
		Named(fmt.Sprintf("snapshot-%s-%s", gvk.Group, gvk.Kind))
	return builder.Complete(r)
}

// AddWatchForPair registers Snapshot + SnapshotContent watches for one pair at runtime (after manager.New).
// Idempotent per snapshot GVK. Uses explicit content GVK (CSD mapping may differ from Kind+"Content").
// If watch setup fails after a new slice entry was appended, that entry is removed and registry entries
// matching this exact pair are reverted (see GVKRegistry.RevertSnapshotRegistrationIfExact). If the
// snapshot GVK was already in the slice (bootstrap), registry is not reverted on failure.
func (r *GenericSnapshotBinderController) AddWatchForPair(mgr ctrl.Manager, snapshotGVK, contentGVK schema.GroupVersionKind) error {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	if r.activeSnapshotWatchSet == nil {
		r.activeSnapshotWatchSet = make(map[string]struct{})
	}
	key := snapshotGVK.String()
	if _, ok := r.activeSnapshotWatchSet[key]; ok {
		return nil
	}
	if err := r.GVKRegistry.RegisterSnapshotContentMapping(
		snapshotGVK.Kind, snapshotGVK.GroupVersion().String(),
		contentGVK.Kind, contentGVK.GroupVersion().String(),
	); err != nil {
		return fmt.Errorf("register snapshot/content mapping: %w", err)
	}
	needAppend := true
	for _, g := range r.SnapshotGVKs {
		if g == snapshotGVK {
			needAppend = false
			break
		}
	}
	if needAppend {
		r.SnapshotGVKs = append(r.SnapshotGVKs, snapshotGVK)
	}
	if err := r.registerSnapshotWatch(mgr, snapshotGVK, contentGVK); err != nil {
		if needAppend {
			r.SnapshotGVKs = r.SnapshotGVKs[:len(r.SnapshotGVKs)-1]
			r.GVKRegistry.RevertSnapshotRegistrationIfExact(snapshotGVK.Kind, snapshotGVK, contentGVK)
		}
		return fmt.Errorf("setup snapshot watch for %s: %w", snapshotGVK.String(), err)
	}
	r.activeSnapshotWatchSet[key] = struct{}{}
	return nil
}

// SetupWithManager sets up the controller with the Manager
// Registers watches for all registered Snapshot GVKs and their corresponding SnapshotContent GVKs
// Each GVK gets its own controller instance to ensure correct GVK context
func (r *GenericSnapshotBinderController) SetupWithManager(mgr ctrl.Manager) error {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	if r.activeSnapshotWatchSet == nil {
		r.activeSnapshotWatchSet = make(map[string]struct{})
	}
	for _, gvk := range r.SnapshotGVKs {
		key := gvk.String()
		if _, ok := r.activeSnapshotWatchSet[key]; ok {
			continue
		}
		contentGVK, err := r.getSnapshotContentGVK(gvk)
		if err != nil {
			return fmt.Errorf("failed to resolve SnapshotContent GVK for %s: %w", gvk.String(), err)
		}
		if err := r.registerSnapshotWatch(mgr, gvk, contentGVK); err != nil {
			return fmt.Errorf("failed to setup watch for Snapshot GVK %s: %w", gvk.String(), err)
		}
		r.activeSnapshotWatchSet[key] = struct{}{}
	}
	return nil
}
