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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snaphelpers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotbinding"
	deckhousev1alpha1 "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/deckhouseio/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// GenericSnapshotBinderController reconciles registered generic XxxxSnapshot resources.
//
// It owns snapshot -> common SnapshotContent binding and writes status.boundSnapshotContentName.
// SnapshotContentController (the aggregator) is the single writer of SnapshotContent.status result refs
// (childrenSnapshotContentRefs, manifestCheckpointName, dataRefs), validates them, and owns the Ready
// condition; the binder no longer projects those refs.
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
	// (creates MCR/VCR/children, publishes captureState.domainSpecificController incl. phase). The binder
	// uses the set only to gate eager content-shell creation on the domain claim (domainHasClaimed) — the
	// capture-leg latches, the request reap, and all status projection are main-owned
	// (SnapshotContentController, decision #10). Guarded by domainCaptureMu.
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
// planning (MCR/VCR/children + captureState.domainSpecificController.phase), while this binder owns all
// SnapshotContent work for it (children/dataRefs projection, VSC handoff, MCR/VCR cleanup, core-owned
// capture-leg latches). Idempotent.
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

// MarkRequiresDataArtifact records whether a snapshot Kind carries a volume data leg (CSD
// spec.requiresDataArtifact) on the GVK registry, so the import path knows whether to wait for a
// DataImport/data artifact. Idempotent; guarded by watchMu (the same lock that serializes registry
// mutations in AddWatchForPair).
func (r *GenericSnapshotBinderController) MarkRequiresDataArtifact(snapshotKind string, requiresDataArtifact bool) {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	r.GVKRegistry.MarkRequiresDataArtifact(snapshotKind, requiresDataArtifact)
}

// isDomainPlanningComplete reports whether the domain controller reached capture barrier 1
// (status.captureState.domainSpecificController.phase >= Planned), i.e. all objects created and refs
// published. It replaces the former PlanningReady=True gate. Spec is immutable, so no observedGeneration
// gate is needed.
func isDomainPlanningComplete(obj *unstructured.Unstructured) bool {
	return domainCaptureAtLeastPlanned(obj)
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
		// Mark the bound SnapshotContent boundSnapshotDeleted on parent Snapshot deletion (watch-driven, no
		// reverse content ref). We deliberately do NOT remove the content's parent-protect finalizer here: in
		// the durable-content tree the content's Kubernetes GC owner is its root ObjectKeeper (root content)
		// or its parent SnapshotContent (child content), NOT this namespaced bound Snapshot. spec.snapshotRef
		// is a logical bound-snapshot back-reference, not the GC parent, so the content must keep parent-protect
		// until its OWN teardown handler runs; boundSnapshotDeleted only records that the logical bound Snapshot
		// is gone (and gates future restore/re-point).
		if err := r.markBoundSnapshotDeletedOnContent(ctx, snapshotLike, obj); err != nil {
			logger.Error(err, "Failed to mark bound SnapshotContent boundSnapshotDeleted on snapshot deletion")
			// Non-fatal: continue with deletion
		}
		// Snapshot is being deleted - no need to continue create-path
		return ctrl.Result{}, nil
	}

	// Import branch (C5): an import-mode leaf (spec.mode: Import) has no live capture and no domain
	// planning, so it bypasses the Step-1 barrier below. The same common controller / SnapshotContent
	// materializes its content (manifest leg from the reconstructed ManifestCheckpoint; for
	// data-artifact kinds, the data leg from the reverse-looked-up DataImport's produced artifact) —
	// there is no second content creator.
	if snapshotIsImportMode(obj) {
		return r.reconcileGenericImport(ctx, obj, snapshotLike)
	}

	// Domain-claim gate (content-single-writer design §11.3/§11.6): for a domain-capture kind, DO NOT
	// materialize any state (ObjectKeeper, eager SnapshotContent shell) until the domain controller has
	// CLAIMED the object by writing status.captureState.domainSpecificController. This is what lets a domain
	// expose only a SUBSET of a registered kind as domain objects: instances the domain skips — a
	// VolumeSnapshot that is legacy/unlabeled, vetoed, import-mode, or pre-provisioned (§11.3) — are never
	// claimed, so the binder leaves them entirely untouched (a plain CSI snapshot with no content and no
	// ObjectKeeper). For domains where every instance is domain-driven (the namespace Snapshot, demo kinds)
	// the claim is present on the domain's first reconcile, so this only defers the shell to that first
	// write and never blocks: the claim is independent of the content existing (proven for the root: the
	// step-3 EnsureChildren claim precedes the step-4 orphan-wave Ready gate), so the eager-shell creation
	// cycle (§9) stays broken. The binder wakes on the snapshot's own watch when the claim is written.
	if r.isDomainCaptureKind(obj.GetObjectKind().GroupVersionKind()) && !domainHasClaimed(obj) {
		logger.V(1).Info("domain-capture snapshot not yet claimed by its domain controller; deferring content shell until captureState.domainSpecificController is written")
		return ctrl.Result{}, nil
	}

	// Eager content shell (creator/main, content-single-writer design §9): the SnapshotContent object is
	// created and BOUND as soon as the snapshot exists, decoupled from the domain phase>=Planned barrier.
	// This is the deadlock fix. A child's ResolveParentSnapshotContentOwnerRef needs the parent content
	// BOUND (not just created), and the namespace root's pre-Planned orphan wave needs its children Ready
	// (which needs their content bound) — both would otherwise wait on a content that was only created at
	// Planned, forming a cycle (root content <- root Planned <- children Ready <- child content bound <-
	// root content). Creating + binding eagerly breaks that edge. Only the STATUS projection legs (capture
	// legs, links, Ready mirror) stay gated on phase>=Planned below; creation + bind do not.
	//
	// Idempotency is structural, not condition-based: the steps below (ensure ObjectKeeper/ownerRef,
	// create SnapshotContent only when status.boundSnapshotContentName is empty, publish result links)
	// are all idempotent and converge to the consistency/mirror step. No progress/handled marker
	// condition is needed; status.boundSnapshotContentName is the durable "content created" signal.
	contentName := snapshotLike.GetStatusContentName()

	// Step 1: Create ObjectKeeper for root snapshots first (needed for SnapshotContent ownerRef)
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

	// Step 2: Create SnapshotContent if it doesn't exist
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
			if !errors.IsAlreadyExists(err) {
				logger.Error(err, "Failed to create SnapshotContent", "name", contentName)
				return ctrl.Result{}, err
			}
			// The deterministic-named content already exists. contentName is UID-derived
			// (names.ContentName), so a same-named object is this Snapshot's own content that was created
			// by a previous reconcile which crashed before patching status.boundSnapshotContentName, or a
			// pre-provisioned root content (e.g. a Delete-policy content). Adopt it instead of wedging on
			// the non-idempotent Create: verify its snapshotRef points back at THIS Snapshot (anti-spoof;
			// a foreign match would only be a stale/rogue object under a UID collision), then bind by name.
			// Spec is immutable, so an existing Delete deletionPolicy is preserved on adoption.
			existing := &unstructured.Unstructured{}
			existing.SetGroupVersionKind(contentGVK)
			if gerr := r.Get(ctx, client.ObjectKey{Name: contentName}, existing); gerr != nil {
				return ctrl.Result{}, gerr
			}
			if !contentSnapshotRefMatchesSnapshot(existing, snapshotGVK, obj) {
				return ctrl.Result{}, fmt.Errorf(
					"SnapshotContent %s already exists but its spec.snapshotRef does not point back at Snapshot %s/%s (uid %s); refusing to adopt",
					contentName, obj.GetNamespace(), obj.GetName(), obj.GetUID())
			}
			logger.Info("Adopted pre-existing SnapshotContent", "name", contentName)
		} else {
			logger.Info("Created SnapshotContent", "name", contentName, "owner", contentOwnerRef.Kind)
		}

		if err := snapshotbinding.PatchUnstructuredBoundContentName(ctx, r.Client, req.NamespacedName, snapshotGVK, contentName); err != nil {
			logger.Error(err, "Failed to update Snapshot status.boundSnapshotContentName")
			return ctrl.Result{}, err
		}
		// Log both field names for backward compatibility with log parsers
		logger.Info("Updated Snapshot status.boundSnapshotContentName", "boundSnapshotContentName", contentName, "contentName", contentName)
		return ctrl.Result{Requeue: true}, nil
	}

	// Step 3 (projection barrier): the content shell exists + is bound (eager, above). Publish the status
	// legs / init capture legs / mirror Ready ONLY after the domain reached capture barrier 1
	// (phase>=Planned: child refs published, MCR/VCR created). Before Planned there is nothing to project;
	// the eager shell is what breaks the create cycle.
	if !isDomainPlanningComplete(obj) {
		logger.V(1).Info("Content shell created+bound; waiting for domain controller to reach capture phase Planned before projecting status legs")
		return ctrl.Result{}, nil
	}
	// Main-owned commonController (decision #10): the capture-leg eager-init, the
	// manifestCaptured/dataCaptured latches, and the MCR/VCR reap moved to the SnapshotContentController
	// (capture_legs.go) — the binder is a pure creator and writes no captureState. A data-leg terminal
	// (failed VCR) is surfaced by main's owner Ready mirror, not co-written here.

	// Step 4: mirror the self-contained captured-volume descriptor (source/artifact + volume metadata)
	// from the bound content's status.data onto the data leaf snapshot's top-level status.data, so d8
	// reads it namespaced on export. data-artifact leaves only — manifest-only kinds have no data binding.
	// No-op until the content publishes status.data.
	if contentName != "" {
		if r.GVKRegistry.RequiresDataArtifact(obj.GetObjectKind().GroupVersionKind().Kind) {
			if err := r.mirrorLeafDataFromContent(ctx, obj, contentName, ""); err != nil {
				// A NotFound is the bound content being deleted out from under the leaf (E3
				// degradation): do NOT error-requeue here, or the leaf wedges in an infinite
				// error loop with a stale Ready=True. Fall through to Step 5, where
				// checkConsistencyAndSetReady co-writes Ready=False/ContentMissing. A real Get/Patch
				// or schema failure on an EXISTING content still requeues so wire-shape drift fails loud.
				if !errors.IsNotFound(err) {
					logger.Error(err, "Failed to mirror captured volume data to leaf status")
					return ctrl.Result{}, err
				}
				logger.V(1).Info("Bound SnapshotContent not found while mirroring data; deferring to Ready degradation", "content", contentName)
			}
		}
	}

	// Step 5: Check consistency and set Ready condition
	// Only check if SnapshotContent already exists (has been processed by SnapshotContentController)
	// This avoids checking consistency in a "half-assembled" state where SnapshotContent
	// might not have finalizer or Ready condition yet. SnapshotContent has no reverse
	// reference to wake this Snapshot, so pending content is mirrored through polling.
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

// contentSnapshotRefMatchesSnapshot reports whether an existing SnapshotContent's spec.snapshotRef points
// back at the given Snapshot. It is the anti-spoof guard for adopting a pre-existing deterministic-named
// content on the create path (AlreadyExists): a ref UID, when present, must equal the Snapshot UID, and
// apiVersion/kind/namespace/name must match. An absent/empty ref is treated as non-matching — we never
// adopt a content that does not claim this Snapshot.
func contentSnapshotRefMatchesSnapshot(content *unstructured.Unstructured, snapshotGVK schema.GroupVersionKind, obj *unstructured.Unstructured) bool {
	ref, found, err := unstructured.NestedMap(content.Object, "spec", "snapshotRef")
	if err != nil || !found || ref == nil {
		return false
	}
	getStr := func(k string) string {
		v, _ := ref[k].(string)
		return v
	}
	if uid := getStr("uid"); uid != "" && uid != string(obj.GetUID()) {
		return false
	}
	return getStr("apiVersion") == snapshotGVK.GroupVersion().String() &&
		getStr("kind") == snapshotGVK.Kind &&
		getStr("name") == obj.GetName() &&
		getStr("namespace") == obj.GetNamespace()
}

// markBoundSnapshotDeletedOnContent latches status.boundSnapshotDeleted=true on the bound SnapshotContent
// when its logical bound Snapshot is deleted. It does NOT remove the content's parent-protect finalizer: the
// content is a durable tree node whose Kubernetes GC owner is its root ObjectKeeper (root content) or parent
// SnapshotContent (child content), never this namespaced bound Snapshot. parent-protect must stay until the
// content's OWN deletion handler completes its reclaim; boundSnapshotDeleted only records that the logical
// bound Snapshot is gone — it stops the live SnapshotContent reconcile from aggregating status against a gone
// owner and gates future restore/re-point. Idempotent (false->true).
func (r *GenericSnapshotBinderController) markBoundSnapshotDeletedOnContent(
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

	// Latch status.boundSnapshotDeleted=true (status subresource) so the live SnapshotContent controller
	// stops aggregating status against a gone owner (it still keeps/ensures parent-protect). Idempotent
	// (false->true). The parent-protect finalizer is intentionally NOT removed here — see the doc comment.
	if latched, _, _ := unstructured.NestedBool(contentObj.Object, "status", "boundSnapshotDeleted"); !latched {
		if err := unstructured.SetNestedField(contentObj.Object, true, "status", "boundSnapshotDeleted"); err != nil {
			return err
		}
		if err := r.Status().Update(ctx, contentObj); err != nil {
			return err
		}
		log.FromContext(ctx).Info("Marked SnapshotContent boundSnapshotDeleted after Snapshot deletion", "content", contentName)
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
	retainerName := snapshot.GenerateObjectKeeperName(obj.GetUID())

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
	ttl := config.DefaultSnapshotTTLAfterDelete
	if r.Config != nil && r.Config.SnapshotTTLAfterDelete > 0 {
		ttl = r.Config.SnapshotTTLAfterDelete
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

// checkConsistencyAndSetReady mirrors the bound SnapshotContent's durable side-channel fields onto the
// domain CR and derives the user-facing Ready ONLY for the degradation cases the content controller cannot
// express (bound content missing or being deleted). GenericSnapshotBinderController does not aggregate
// children; SnapshotContent is the single source of truth for readiness.
//
// wave7 final-wave-1: the STEADY-STATE Ready mirror is owned by the SnapshotContentController
// (mirrorReadyToOwnerSnapshot) — the single post-bind writer that mirrors content.Ready, bubbles a domain
// phase=Failed, and applies the barrier-2 (phase=Finished) finalization gate in the SAME pass that computes
// content.Ready (no cross-controller staleness, INV-FAIL-PROP). The binder no longer re-derives content.Ready
// here. It still owns the "bound content gone/deleting" degradation, because a deleted content produces no
// content reconcile to mirror from; the bound-content watch (mapBoundContentToSnapshots) wakes the binder
// for that. The excludedRefs mirror stays here too (it is not Ready and is triggered by the same watch); the
// childSubtreesManifestsPersisted latch is main-owned (snapshotcontent/capture_legs.go). This function is
// only reached after the Step-1 barrier
// (isDomainPlanningComplete) confirmed phase>=Planned; the domain phase itself is never written here (owned
// by the domain SDK).
func (r *GenericSnapshotBinderController) checkConsistencyAndSetReady(
	ctx context.Context,
	snapshotLike snapshot.SnapshotLike,
	obj *unstructured.Unstructured,
) error {
	logger := log.FromContext(ctx)
	contentName := snapshotLike.GetStatusContentName()
	if contentName == "" {
		return r.patchSnapshotNotReadyFromContent(ctx, obj, snapshotLike, snapshot.ReasonContentMissing, "SnapshotContent is not bound")
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
			// E3 degradation: the bound content was deleted out from under a Ready snapshot. The content
			// controller cannot express this (no reconcile for a gone object), so the binder co-writes the
			// terminal-shaped ContentMissing directly, woken by mapBoundContentToSnapshots on the delete.
			return r.patchSnapshotNotReadyFromContent(ctx, obj, snapshotLike, snapshot.ReasonContentMissing, fmt.Sprintf("SnapshotContent %s not found", contentName))
		}
		return fmt.Errorf("failed to get SnapshotContent: %w", err)
	}

	if !contentObj.GetDeletionTimestamp().IsZero() {
		return r.patchSnapshotNotReadyFromContent(ctx, obj, snapshotLike, snapshot.ReasonDeleting, fmt.Sprintf("SnapshotContent %s is being deleted", contentName))
	}

	// Mirror the bound content's durable excludedRefs aggregate onto this domain CR's top-level
	// status.excludedRefs (user-facing audit). Best-effort: a mirror miss is retried on the next reconcile.
	if err := r.mirrorExcludedRefsFromContent(ctx, obj, contentObj); err != nil {
		logger.V(1).Info("Failed to mirror SnapshotContent excludedRefs; will retry", "error", err.Error())
	}

	// The childSubtreesManifestsPersisted latch onto commonController is written by main
	// (snapshotcontent/capture_legs.go — main-owned commonController, decision #10).

	// Steady-state Ready (content.Ready mirror + phase=Failed bubble + barrier-2 gate) is owned by the
	// SnapshotContentController's single post-bind writer; the binder does not re-derive it here.
	return nil
}

// patchSnapshotNotReadyFromContent mirrors a Ready=False condition from the bound SnapshotContent onto the
// Snapshot. The binder only ever writes the failure state (content missing/misbound/deleting or an import
// terminal); the Ready=True path is owned by the SnapshotContentController's post-bind mirror, so the status
// is always ConditionFalse here.
func (r *GenericSnapshotBinderController) patchSnapshotNotReadyFromContent(
	ctx context.Context,
	obj *unstructured.Unstructured,
	snapshotLike snapshot.SnapshotLike,
	reason string,
	message string,
) error {
	return r.patchSnapshotConditionFromContent(ctx, obj, snapshotLike, snapshot.ConditionReady, metav1.ConditionFalse, reason, message)
}

// patchSnapshotConditionFromContent mirrors the Ready condition from the bound SnapshotContent onto the
// Snapshot, gen-stamped under an optimistic-lock merge patch.
//
// D4a: read-modify-write only the target condition. The domain controller co-writes
// captureState.domainSpecificController into the same status; a bare Status().Update / MergeFrom would
// replace the whole status and could silently drop the other writer's entry. MergeFromWithOptimisticLock
// turns a concurrent write into a 409 so RetryOnConflict re-reads the fresh object (already carrying the
// other writer's state) and re-applies only this condition, stamping observedGeneration for gen-gated
// readers (INV-DOMAIN-GEN).
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
		// No MCR watch: the binder no longer latches/reaps the capture legs (main-owned commonController,
		// decision #10) — the aggregator carries its own MCR watch (mapMCRToBoundContent) for the
		// projection + latch + reap lifecycle.
		Named(fmt.Sprintf("snapshot-%s-%s", gvk.Group, gvk.Kind)).
		// Independent XxxSnapshots (one per set + per child) fan out under a multi-tree burst; a single
		// worker serializes their binding behind the queue. The binder writes only its own snapshot's
		// status.boundSnapshotContentName and keeps no shared mutable state, so parallel workers are safe.
		WithOptions(controller.Options{MaxConcurrentReconciles: 4})
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
