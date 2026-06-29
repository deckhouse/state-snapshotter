// Package unifiedruntime applies R2 phase 2b / R3: after CSD status reconciliation, merge bootstrap
// with eligible CSD mappings and add missing unified Snapshot / SnapshotContent watches without pod restart.
//
// Runtime note: each successful add calls controller-runtime builder.Complete while the manager may
// already be running. That path is supported by controller-runtime (runnables added after Start) but
// is sensitive to library upgrades — treat as a contract to re-verify on bumps.
package unifiedruntime

import (
	"context"
	"sort"
	"sync"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// DedicatedControllerActivator registers a dedicated snapshot controller (one that reconciles a
// specific snapshot kind outside GenericSnapshotBinderController, e.g. the demo domain controllers)
// on an already-running manager. It is invoked at most once per kind, only after the kind's CSD is
// watch-eligible (Accepted=True && AccessGranted=True), so the controller's informers start with the
// domain RBAC already granted by the Deckhouse hook — never at boot, which would deadlock cache sync.
type DedicatedControllerActivator func(ctrl.Manager) error

// Syncer recomputes layered GVK state (see LayeredGVKState) and registers additive watches on the manager.
type Syncer struct {
	mu        sync.Mutex
	mgr       ctrl.Manager
	log       logr.Logger
	bootstrap []unifiedbootstrap.UnifiedGVKPair
	reader    client.Reader
	snap      *controllers.GenericSnapshotBinderController
	content   *controllers.SnapshotContentController

	// dedicatedActivators is keyed by snapshot Kind. When a dedicated kind enters the eligible
	// resolved set, the matching activator is called once to register its controller at runtime.
	// A nil/absent entry preserves the legacy behavior of marking the kind active without registering
	// anything here (used by focused tests that wire the controllers themselves).
	dedicatedActivators map[string]DedicatedControllerActivator

	lastState LayeredGVKState
	// activeSnapshotGVKKeys: snapshot GVK String() for which the required runtime watches
	// were successfully registered at least once in this process (monotonic; not cleared when resolved drops).
	activeSnapshotGVKKeys map[string]struct{}
}

// NewSyncer builds a syncer. bootstrap must be the static unified-runtime list
// (same as DefaultUnifiedRuntimeBootstrapPairs / legacy DefaultDesiredUnifiedSnapshotPairs);
// eligible CSD pairs are merged on each Sync.
//
// dedicatedActivators maps a snapshot Kind (e.g. "DemoVirtualDiskSnapshot") to a function that
// registers its dedicated controller at runtime. It may be nil/empty: then dedicated kinds are only
// marked active (legacy behavior) and the caller is responsible for registering their controllers.
func NewSyncer(
	mgr ctrl.Manager,
	log logr.Logger,
	bootstrap []unifiedbootstrap.UnifiedGVKPair,
	reader client.Reader,
	snap *controllers.GenericSnapshotBinderController,
	content *controllers.SnapshotContentController,
	dedicatedActivators map[string]DedicatedControllerActivator,
) *Syncer {
	registerUnifiedRuntimeMetrics()
	return &Syncer{
		mgr:                   mgr,
		log:                   log.WithName("unified-runtime"),
		bootstrap:             bootstrap,
		reader:                reader,
		snap:                  snap,
		content:               content,
		dedicatedActivators:   dedicatedActivators,
		activeSnapshotGVKKeys: make(map[string]struct{}),
	}
}

func copyLayeredState(src LayeredGVKState) LayeredGVKState {
	out := LayeredGVKState{CSDEligibleError: src.CSDEligibleError}
	out.BootstrapDesired = append([]unifiedbootstrap.UnifiedGVKPair(nil), src.BootstrapDesired...)
	out.EligibleFromCSD = append([]unifiedbootstrap.UnifiedGVKPair(nil), src.EligibleFromCSD...)
	out.DesiredMerged = append([]unifiedbootstrap.UnifiedGVKPair(nil), src.DesiredMerged...)
	out.ResolvedSnapshotGVKs = append([]schema.GroupVersionKind(nil), src.ResolvedSnapshotGVKs...)
	out.ResolvedContentGVKs = append([]schema.GroupVersionKind(nil), src.ResolvedContentGVKs...)
	return out
}

// LastLayeredState returns a deep copy of the state from the last successful Sync (under lock).
func (s *Syncer) LastLayeredState() LayeredGVKState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyLayeredState(s.lastState)
}

// ActiveSnapshotGVKKeys returns a copy of snapshot GVK keys that have both watches registered successfully.
func (s *Syncer) ActiveSnapshotGVKKeys() map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.activeSnapshotGVKKeys) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(s.activeSnapshotGVKKeys))
	for k := range s.activeSnapshotGVKKeys {
		out[k] = struct{}{}
	}
	return out
}

// Sync is safe to call from the CSD reconciler after successful status updates. Errors from individual
// watch registration are logged; the function returns nil unless a programming invariant breaks.
func (s *Syncer) Sync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state := BuildLayeredGVKState(ctx, s.reader, s.mgr.GetRESTMapper(), s.bootstrap, s.log)
	if state.CSDEligibleError != nil {
		s.log.Info("CSD list/parse for unified runtime sync failed; using bootstrap-only merge", "error", state.CSDEligibleError)
	}

	prevResolved := s.lastState.ResolvedSnapshotKeySet()
	curResolved := state.ResolvedSnapshotKeySet()
	for k := range prevResolved {
		if _, ok := curResolved[k]; !ok {
			s.log.V(1).Info("snapshot GVK left resolved set; in-process watches are not removed",
				"snapshot", k)
		}
	}
	s.lastState = state

	// Register dedicated controllers for newly-eligible kinds first, in a deterministic order, so a
	// child kind's typed informer + field index exists before a parent controller's Watches starts it
	// (see activateDedicatedControllersLocked). Generic watches are wired in the loop below.
	s.activateDedicatedControllersLocked(state.ResolvedSnapshotGVKs)

	// CSD spec.dataBacked is carried on the merged pairs (lost by the parallel resolved slices); index
	// it by snapshot Kind so the binder's registry learns which kinds expect a DataImport/data artifact.
	dataBackedByKind := make(map[string]bool, len(state.DesiredMerged))
	for _, p := range state.DesiredMerged {
		dataBackedByKind[p.Snapshot.Kind] = p.DataBacked
	}

	for i := range state.ResolvedSnapshotGVKs {
		snapGVK, contentGVK := state.ResolvedSnapshotGVKs[i], state.ResolvedContentGVKs[i]
		s.snap.MarkDataBacked(snapGVK.Kind, dataBackedByKind[snapGVK.Kind])
		if err := s.content.AddSnapshotStatusWatch(s.mgr, snapGVK); err != nil {
			s.log.Error(err, "add SnapshotContent snapshot status watch failed", "snapshot", snapGVK.String())
			continue
		}
		if unifiedbootstrap.IsDedicatedSnapshotControllerKind(snapGVK.Kind) {
			if !unifiedbootstrap.IsDomainCaptureSnapshotKind(snapGVK.Kind) {
				// Fully-dedicated kind (e.g. the namespace-root "Snapshot"): it owns its own
				// SnapshotContent, so the generic binder never watches it. Activation + marking happen
				// in activateDedicatedControllersLocked. When no activator is wired for this kind,
				// preserve the legacy behavior of marking it active here.
				if _, hasActivator := s.dedicatedActivators[snapGVK.Kind]; !hasActivator {
					s.activeSnapshotGVKKeys[snapGVK.String()] = struct{}{}
				}
				continue
			}
			// Domain-capture kind (demo): the dedicated planning controller owns MCR/VCR/children +
			// PlanningReady, while the generic binder owns its SnapshotContent. The binder uses
			// its own unstructured informer and registers no field index, so it can be wired
			// independently of the planning controller — EXCEPT in a single manager that runs both: there
			// the planning controller's typed informer + field index must be registered first to avoid an
			// indexer conflict on the shared informer. So the gate only applies when this manager owns the
			// planning controller (an activator is wired for the kind).
			//
			// Two-pod split (core): the planning controller runs in the domain-controller pod, so no
			// activator is wired here. Core then owns the SnapshotContent directly with no ordering
			// hazard. This keeps the cutover invariant (exactly one SnapshotContent owner — the generic
			// binder) while the demo CR is reconciled solely by the out-of-process domain controller.
			if _, hasActivator := s.dedicatedActivators[snapGVK.Kind]; hasActivator {
				if _, planningActive := s.activeSnapshotGVKKeys[snapGVK.String()]; !planningActive {
					continue
				}
			}
			s.snap.MarkDomainCaptureKind(snapGVK)
		}
		if err := s.snap.AddWatchForPair(s.mgr, snapGVK, contentGVK); err != nil {
			s.log.Error(err, "add Snapshot watch failed", "snapshot", snapGVK.String())
			continue
		}
		if err := s.content.AddWatchForContent(s.mgr, snapGVK, contentGVK); err != nil {
			s.log.Error(err, "add SnapshotContent watch failed", "snapshotContent", contentGVK.String())
			continue
		}
		s.activeSnapshotGVKKeys[snapGVK.String()] = struct{}{}
	}

	var staleKeys []string
	for k := range s.activeSnapshotGVKKeys {
		if _, ok := curResolved[k]; !ok {
			staleKeys = append(staleKeys, k)
		}
	}
	sort.Strings(staleKeys)

	resolvedN := len(curResolved)
	activeN := len(s.activeSnapshotGVKKeys)
	staleN := len(staleKeys)
	resolvedSnapshotGVKGauge.Set(float64(resolvedN))
	activeMonotonicSnapshotGVKGauge.Set(float64(activeN))
	staleActiveSnapshotGVKGauge.Set(float64(staleN))

	s.log.V(2).Info("unified runtime sync summary",
		"resolvedSnapshotCount", resolvedN,
		"activeMonotonicCount", activeN,
		"staleActiveCount", staleN)
	if staleN > 0 {
		s.log.Info("unified runtime: snapshot GVK keys remain active in-process but are no longer in the resolved set (additive watches are not removed); consider restarting the pod for a clean informer set",
			"staleSnapshotGVKKeys", staleKeys,
			"staleActiveCount", staleN,
			"resolvedSnapshotCount", resolvedN,
			"activeMonotonicCount", activeN)
	}
	return nil
}

// activateDedicatedControllersLocked registers dedicated snapshot controllers for the kinds that are
// present in the eligible resolved set and have a wired activator, iterating in
// unifiedbootstrap.DedicatedSnapshotControllerKinds order.
//
// That order is dependency order (children before parents): a parent controller (e.g. the demo VM
// snapshot controller) Watches a child kind (DemoVirtualDiskSnapshot), and starting that Watch starts
// the child's typed informer. The child's controller registers a typed field index, which
// controller-runtime rejects once the informer has already started ("indexer conflict"). Activating
// the child first guarantees its field index is registered before any parent Watch can start the
// child informer.
//
// Each kind is activated at most once (guarded by activeSnapshotGVKKeys). On activation failure the key
// is left unset so the next Sync retries. The caller must hold s.mu.
func (s *Syncer) activateDedicatedControllersLocked(resolved []schema.GroupVersionKind) {
	if len(s.dedicatedActivators) == 0 {
		return
	}
	resolvedByKind := make(map[string]schema.GroupVersionKind, len(resolved))
	for _, gvk := range resolved {
		if _, exists := resolvedByKind[gvk.Kind]; !exists {
			resolvedByKind[gvk.Kind] = gvk
		}
	}
	for _, kind := range unifiedbootstrap.DedicatedSnapshotControllerKinds {
		activator := s.dedicatedActivators[kind]
		if activator == nil {
			continue
		}
		gvk, inResolved := resolvedByKind[kind]
		if !inResolved {
			continue
		}
		if _, already := s.activeSnapshotGVKKeys[gvk.String()]; already {
			continue
		}
		if err := activator(s.mgr); err != nil {
			s.log.Error(err, "activate dedicated snapshot controller failed; will retry on next sync",
				"snapshot", gvk.String())
			continue
		}
		s.log.Info("activated dedicated snapshot controller", "snapshot", gvk.String())
		s.activeSnapshotGVKKeys[gvk.String()] = struct{}{}
	}
}
