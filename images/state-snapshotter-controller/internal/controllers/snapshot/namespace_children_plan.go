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
	stderrors "errors"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snaphelpers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/csdregistry"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

// namespaceChildrenOutcome is the planning verdict planNamespaceChildren returns for the reconciler to map
// onto the SDK recipe (wave5 design §4.2): AllPlanned → MarkPlanned/Finished gate; Pending/Forbidden →
// requeue (non-terminal); Terminal → sdk.Fail. It replaces the bespoke reconcileParentOwnedChildGraph's
// (changed, ready, err) tri-return once the root is content-free.
type namespaceChildrenOutcome int

const (
	// namespaceChildrenAllPlanned: every weight layer reached domainSpecificController.phase>=Planned; the
	// full child graph is enumerated and planned.
	namespaceChildrenAllPlanned namespaceChildrenOutcome = iota
	// namespaceChildrenPending: a weight layer has not yet reached phase>=Planned (unbounded by design —
	// a child may stay pending for hours). Non-terminal; the reconciler requeues.
	namespaceChildrenPending
	// namespaceChildrenForbidden: listing a mapped source kind was rejected with Forbidden (RBAC granted
	// externally via CSD AccessGranted). Non-terminal; the reconciler degrades Ready and requeues.
	namespaceChildrenForbidden
	// namespaceChildrenTerminal: a child surfaced a terminal capture failure (phase=Failed or a terminal
	// Ready reason). The reconciler fails the root capture.
	namespaceChildrenTerminal
)

// namespaceChildrenPlan is the result of planning the root's domain child graph: the desired child
// snapshots as SDK ChildSpecs (fed to sdk.EnsureChildren, which creates/adopts + publishes refs), the
// root's own top-level exclude-veto drops (published as domainSpecificController.excludedRefs), and the
// planning outcome. It never creates objects or writes status — that is the reconciler's job via the SDK.
type namespaceChildrenPlan struct {
	desired  []snapshotsdk.ChildSpec
	excluded []storagev1alpha1.ExcludedObjectRef
	outcome  namespaceChildrenOutcome
	reason   string
	message  string
}

// planNamespaceChildren reproduces the bespoke reconcileParentOwnedChildGraph enumeration as a pure
// planner (wave5 design §6.2): CSD-eligible resource mappings, ascending weight layers, resourceSelector
// narrowing, exclude-label veto, and cross-layer coverage dedup — but it BUILDS []ChildSpec instead of
// creating child snapshots, and returns the planning outcome instead of patching status. The reconciler
// then calls sdk.EnsureChildren(desired, excluded) to create/adopt + publish, and maps the outcome onto
// the barrier (MarkPlanned / Fail / requeue).
//
// Weight-layer gating is preserved exactly: a higher layer's coverage depends on lower layers having
// reached capture barrier 1, so the loop stops at the first layer not yet phase>=Planned and returns
// Pending with the layers planned so far. Because the just-built children do not exist until the caller's
// EnsureChildren creates them, the per-reconcile requeue drives the layer-by-layer progression — the same
// convergence the bespoke path had (which also requeued after each freshly-created layer).
//
// mappings are the CSD-eligible resource→snapshot mappings the reconciler resolves from the live registry
// (csdregistry.EligibleResourceSnapshotMappings, ascending by weight). They are passed in rather than
// fetched here so the planner needs no manager/RESTMapper and stays unit-testable with a fake mapping list.
func (r *SnapshotReconciler) planNamespaceChildren(ctx context.Context, nsSnap *storagev1alpha1.Snapshot, mappings []csdregistry.EligibleResourceSnapshotMapping) (namespaceChildrenPlan, error) {
	if len(mappings) == 0 {
		return namespaceChildrenPlan{outcome: namespaceChildrenAllPlanned}, nil
	}

	// resourceSelector narrows which top-level domain source objects the root expands into child snapshots
	// (nil = expand all). Resolved once and threaded into every layer, keeping the manifest leg consistent.
	selector, err := nsSnap.ResolveResourceSelector()
	if err != nil {
		return namespaceChildrenPlan{}, fmt.Errorf("resolve spec.resourceSelector: %w", err)
	}

	var desired []snapshotsdk.ChildSpec
	var desiredRefs []storagev1alpha1.SnapshotChildRef
	var topLevelDrops []storagev1alpha1.ExcludedObjectRef
	coverage := newSnapshotCoverageChecker(r.Client, nsSnap.Namespace, nil)
	for layerStart := 0; layerStart < len(mappings); {
		// Mappings are ordered ascending by CSD spec.weight; a weight layer is the run of equal-weight
		// mappings processed together before the next (higher) layer.
		weight := mappings[layerStart].Weight
		layerEnd := layerStart + 1
		for layerEnd < len(mappings) && mappings[layerEnd].Weight == weight {
			layerEnd++
		}
		var layerSpecs []snapshotsdk.ChildSpec
		var layerRefs []storagev1alpha1.SnapshotChildRef
		for _, mapping := range mappings[layerStart:layerEnd] {
			specs, refs, excluded, err := r.planParentOwnedChildGraphLayer(ctx, nsSnap, mapping, coverage, selector)
			if err != nil {
				var forbidden *sourceListForbiddenError
				if stderrors.As(err, &forbidden) {
					return namespaceChildrenPlan{
						desired:  desired,
						excluded: topLevelDrops,
						outcome:  namespaceChildrenForbidden,
						reason:   snapshotpkg.ReasonSourceListForbidden,
						message:  forbidden.Error(),
					}, nil
				}
				return namespaceChildrenPlan{}, err
			}
			layerSpecs = append(layerSpecs, specs...)
			layerRefs = append(layerRefs, refs...)
			topLevelDrops = append(topLevelDrops, excluded...)
		}
		desired = append(desired, layerSpecs...)
		desiredRefs = append(desiredRefs, layerRefs...)
		ready, terminalMessage, pending, err := r.weightLayerCaptureReady(ctx, nsSnap.Namespace, layerRefs)
		if err != nil {
			return namespaceChildrenPlan{}, err
		}
		if terminalMessage != "" {
			return namespaceChildrenPlan{
				desired:  desired,
				excluded: topLevelDrops,
				outcome:  namespaceChildrenTerminal,
				reason:   snapshotpkg.ReasonGraphPlanningFailed,
				message:  terminalMessage,
			}, nil
		}
		if !ready {
			// Unbounded by design: a child snapshot (e.g. large-storage capture) may stay pending for
			// hours. Non-terminal — the reconciler surfaces ChildrenPending and requeues; never a deadline.
			message := fmt.Sprintf("waiting for weight %d child snapshots to reach capture phase Planned; %s", weight, summarizePendingChildren(pending))
			return namespaceChildrenPlan{
				desired:  desired,
				excluded: topLevelDrops,
				outcome:  namespaceChildrenPending,
				reason:   snapshotpkg.ReasonChildrenPending,
				message:  message,
			}, nil
		}
		coverage = newSnapshotCoverageChecker(r.Client, nsSnap.Namespace, coverageRootsForNextWave(desiredRefs))
		layerStart = layerEnd
	}
	// Pre-Planned lost-child gate (Block E, case 4): the whole domain graph is now planned, so desiredRefs
	// is the COMPLETE set of children whose sources are live. A previously-published domain child that has
	// left that set (source gone) AND whose CR was also deleted is unrecoverable — surface it as a terminal
	// ChildSnapshotLost instead of wedging the root forever on a dangling ref.
	if reason, message, lostErr := r.detectLostDomainChildrenPrePlanned(ctx, nsSnap, desiredRefs); lostErr != nil {
		return namespaceChildrenPlan{}, lostErr
	} else if reason != "" {
		return namespaceChildrenPlan{
			desired:  desired,
			excluded: topLevelDrops,
			outcome:  namespaceChildrenTerminal,
			reason:   reason,
			message:  message,
		}, nil
	}
	return namespaceChildrenPlan{desired: desired, excluded: topLevelDrops, outcome: namespaceChildrenAllPlanned}, nil
}

// namespaceDomainPrePlanned reports whether the root's domain half is still before capture barrier 1
// (phase absent, or Planning). Past Planned/Finished/Failed the binder/main owns Snapshot.Ready, so the
// pre-Planned reconciler MUST NOT write Ready (no dual-writer) — the pre-Planned lost-child gate is a
// no-op there, and a lost declared child is instead surfaced by the main-side owner-mirror folds.
func namespaceDomainPrePlanned(nsSnap *storagev1alpha1.Snapshot) bool {
	cs := nsSnap.Status.CaptureState
	if cs == nil || cs.DomainSpecificController == nil {
		return true
	}
	switch cs.DomainSpecificController.Phase {
	case storagev1alpha1.SnapshotCapturePhasePlanned,
		storagev1alpha1.SnapshotCapturePhaseFinished,
		storagev1alpha1.SnapshotCapturePhaseFailed:
		return false
	default:
		return true
	}
}

// detectLostDomainChildrenPrePlanned scans the root's PUBLISHED domain child refs (delete-free/union, so
// a stale ref lingers after its source vanishes) for one whose source object is gone — the ref is no
// longer in the freshly-computed desired set — AND whose child CR was also deleted (uncached NotFound).
// Pre-Planned that is unrecoverable: the re-plan cannot recreate the CR (no source to enumerate) and its
// UID-derived content name could not relink even if recreated, so the node would otherwise wait forever
// on a child that never comes back. It returns a terminal ChildSnapshotLost fold for planNamespaceChildren.
//
// A ref still in desired (source alive) is skipped — EnsureChildren re-creates a missing CR by its
// deterministic name (self-heal), so a live source is never a failure. A ref whose CR is still present is
// skipped — losing its source does not un-capture an already-created child. Orphan CSI VolumeSnapshot
// children are skipped: they are re-derived from live PVCs by the orphan wave (not planNamespaceChildren),
// so they are never in desiredRefs and must not be judged against it. Runs only pre-Planned (the caller
// reaches it only at AllPlanned, and the phase gate keeps it off the binder's Ready post-Planned).
func (r *SnapshotReconciler) detectLostDomainChildrenPrePlanned(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	desiredRefs []storagev1alpha1.SnapshotChildRef,
) (reason, message string, err error) {
	if !namespaceDomainPrePlanned(nsSnap) {
		return "", "", nil
	}
	desiredNames := make(map[string]struct{}, len(desiredRefs))
	for _, ref := range desiredRefs {
		desiredNames[ref.Name] = struct{}{}
	}
	for _, ref := range nonOrphanCSIVolumeSnapshotChildRefs(nsSnap.Status.ChildrenSnapshotRefs) {
		if _, wanted := desiredNames[ref.Name]; wanted {
			continue
		}
		gv, gvErr := schema.ParseGroupVersion(ref.APIVersion)
		if gvErr != nil {
			return "", "", gvErr
		}
		child := &unstructured.Unstructured{}
		child.SetGroupVersionKind(gv.WithKind(ref.Kind))
		getErr := r.APIReader.Get(ctx, client.ObjectKey{Namespace: nsSnap.Namespace, Name: ref.Name}, child)
		if getErr == nil {
			continue
		}
		if !errors.IsNotFound(getErr) {
			return "", "", getErr
		}
		return snapshotpkg.ReasonChildSnapshotLost,
			fmt.Sprintf("declared child snapshot %s %q is gone and its source object no longer exists; it cannot be re-planned and is unrecoverably lost — a new snapshot is required", ref.Kind, ref.Name), nil
	}
	return "", "", nil
}

// planParentOwnedChildGraphLayer is the build-spec (create-free) counterpart of
// ensureParentOwnedChildGraphLayer: it lists a mapping's source objects, applies the exclude-label veto
// and resourceSelector, dedups against coverage, and BUILDS one SDK ChildSpec per kept object (via
// buildNamespaceChildSpec) instead of creating the child snapshot. It returns the specs, their refs (for
// the weight-layer readiness gate + coverage seeding), and the top-level drops. Owner-reference stamping
// and create/adopt are the SDK's job (sdk.EnsureChildren), so the spec carries no owner ref.
//
// NOTE (wave5 PR-B, transitional): this duplicates the enumeration loop of ensureParentOwnedChildGraphLayer
// deliberately, so the bespoke create path keeps working until the atomic content-free flip wires the SDK
// recipe. The bespoke ensureParentOwnedChildGraphLayer/reconcileParentOwnedChildGraph are removed in the
// same flip, retiring this duplication.
func (r *SnapshotReconciler) planParentOwnedChildGraphLayer(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	mapping csdregistry.EligibleResourceSnapshotMapping,
	coverage snapshotCoverageChecker,
	selector labels.Selector,
) ([]snapshotsdk.ChildSpec, []storagev1alpha1.SnapshotChildRef, []storagev1alpha1.ExcludedObjectRef, error) {
	var specs []snapshotsdk.ChildSpec
	var refs []storagev1alpha1.SnapshotChildRef
	var excluded []storagev1alpha1.ExcludedObjectRef
	// A CSD that maps a source (a PVC) directly onto the NATIVE CSI VolumeSnapshot kind is NOT expanded into
	// a domain child here. PVC volume capture is owned end-to-end by the root's residual/orphan wave
	// (ensureOrphanVolumeSnapshotsPrePlanned -> ensureOrphanPVCVolumeSnapshots), which builds the correct
	// spec.source.persistentVolumeClaimName, resolves the VolumeSnapshotClass, and honors resourceSelector.
	// buildNamespaceChildSpec only emits the unified spec.sourceRef shape, so expanding a native VolumeSnapshot
	// mapping would POST an invalid VolumeSnapshot ("spec.source: Required value") and wedge the root capture
	// (the child never gets conditions). We still enumerate the source objects below so a veto-labeled PVC is
	// recorded in excludedRefs (same treatment as a vetoed domain source) — only the child-spec build is
	// skipped; coverage is left untouched so the residual/orphan wave picks the PVCs up.
	expandsToOrphanWave := isNativeCSIVolumeSnapshotMapping(mapping)
	resources, err := r.Dynamic.Resource(mapping.SourceGVR).Namespace(nsSnap.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		// NotFound: the mapped source kind is not (yet) served by the API; legitimately empty for now.
		if errors.IsNotFound(err) {
			return nil, nil, nil, nil
		}
		// Forbidden is RBAC-driven (granted externally via CSD AccessGranted). Treating it as "no objects"
		// would silently drop coverage, so signal a degrade instead of returning empty (fail-closed).
		if errors.IsForbidden(err) {
			return nil, nil, nil, &sourceListForbiddenError{msg: fmt.Sprintf("list source %s: %v", mapping.SourceGVK.String(), err)}
		}
		return nil, nil, nil, err
	}
	items := resources.Items
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.GetNamespace() != b.GetNamespace() {
			return a.GetNamespace() < b.GetNamespace()
		}
		if a.GetName() != b.GetName() {
			return a.GetName() < b.GetName()
		}
		return string(a.GetUID()) < string(b.GetUID())
	})
	for i := range items {
		resource := &items[i]
		// Absolute exclude veto (wave4A): a top-level source object carrying the exclude label is dropped
		// from every leg (it also fails selector.Matches below, since ResolveResourceSelector folds the veto
		// in) and recorded as an explicit top-level drop — the root node's OWN direct exclusion.
		if _, vetoed := resource.GetLabels()[storagev1alpha1.ExcludeLabelKey]; vetoed {
			excluded = append(excluded, storagev1alpha1.ExcludedObjectRef{
				APIVersion: mapping.SourceGVK.GroupVersion().String(),
				Kind:       mapping.SourceGVK.Kind,
				Name:       resource.GetName(),
			})
		}
		// Native CSI VolumeSnapshot mapping: the veto above is still recorded, but the source object is NOT
		// expanded into a child (nor marked covered) — the residual/orphan wave owns its VolumeSnapshot.
		if expandsToOrphanWave {
			continue
		}
		// User-provided resourceSelector narrows expansion: a source object whose labels do not match is not
		// expanded into a child snapshot (nil selector = expand all).
		if selector != nil && !selector.Matches(labels.Set(resource.GetLabels())) {
			continue
		}
		covered, err := coverage.IsCovered(ctx, resource)
		if err != nil {
			return nil, nil, nil, err
		}
		if covered {
			continue
		}
		sourceIdentity, err := controllercommon.SnapshotSourceIdentityFromObject(resource)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("source identity for %s/%s: %w", resource.GroupVersionKind().String(), resource.GetName(), err)
		}
		childName := snapshotChildSnapshotName(nsSnap.UID, resource.GetUID())
		specs = append(specs, buildNamespaceChildSpec(nsSnap.Namespace, childName, mapping.SnapshotGVK, sourceIdentity))
		ref := storagev1alpha1.SnapshotChildRef{
			APIVersion: mapping.SnapshotGVK.GroupVersion().String(),
			Kind:       mapping.SnapshotGVK.Kind,
			Name:       childName,
		}
		refs = append(refs, ref)
		if err := coverage.ObservePlannedSnapshot(ctx, resource, ref, nil); err != nil {
			return nil, nil, nil, err
		}
	}
	sortSnapshotChildRefs(refs)
	return specs, refs, excluded, nil
}

// isNativeCSIVolumeSnapshotMapping reports whether a CSD maps its source onto the native CSI VolumeSnapshot
// kind (snapshot.storage.k8s.io/VolumeSnapshot). Such mappings must not be expanded into domain children by
// the namespace planner: the native VolumeSnapshot has no spec.sourceRef (it requires
// spec.source.persistentVolumeClaimName), and PVC volume capture is owned by the root's residual/orphan wave.
// Group+Kind (not the pinned version) is matched so a future served version still routes to the orphan wave.
//
// This guard STAYS load-bearing even after VolumeSnapshot became a built-in pair (BuiltInVolumeSnapshotPair)
// and its dedicated CustomSnapshotDefinition was removed from storage-foundation — do NOT delete it as dead
// code:
//   - Rollout window: core and storage-foundation deploy independently. While an older storage-foundation
//     still ships the storage-foundation-volumesnapshot CSD, EligibleResourceSnapshotMappings still yields a
//     PVC->VolumeSnapshot mapping into this planner; only this guard keeps it out of the child graph.
//   - Permanent defense: CustomSnapshotDefinition admission does not forbid mapping onto a native CSI kind
//     (no CEL rule / webhook on spec.apiVersion+kind), so any third-party CSD could re-introduce the invalid
//     native-VolumeSnapshot expansion. The guard fails that closed regardless of who authored the CSD.
func isNativeCSIVolumeSnapshotMapping(mapping csdregistry.EligibleResourceSnapshotMapping) bool {
	return mapping.SnapshotGVK.Group == snapshotpkg.CSISnapshotGroup &&
		mapping.SnapshotGVK.Kind == snapshotpkg.KindVolumeSnapshot
}
