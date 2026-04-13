// Package unifiedruntime applies R2 phase 2b / R3: after DSC status reconciliation, merge bootstrap
// with eligible DSC mappings and add missing unified Snapshot / SnapshotContent watches without pod restart.
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

// Syncer recomputes layered GVK state (see LayeredGVKState) and registers additive watches on the manager.
type Syncer struct {
	mu        sync.Mutex
	mgr       ctrl.Manager
	log       logr.Logger
	bootstrap []unifiedbootstrap.UnifiedGVKPair
	reader    client.Reader
	snap      *controllers.SnapshotController
	content   *controllers.SnapshotContentController

	lastState LayeredGVKState
	// activeSnapshotGVKKeys: snapshot GVK String() for which both Snapshot and SnapshotContent watches
	// were successfully registered at least once in this process (monotonic; not cleared when resolved drops).
	activeSnapshotGVKKeys map[string]struct{}
}

// NewSyncer builds a syncer. bootstrap must be the static default list (same as DefaultDesiredUnifiedSnapshotPairs);
// eligible DSC pairs are merged on each Sync.
func NewSyncer(
	mgr ctrl.Manager,
	log logr.Logger,
	bootstrap []unifiedbootstrap.UnifiedGVKPair,
	reader client.Reader,
	snap *controllers.SnapshotController,
	content *controllers.SnapshotContentController,
) *Syncer {
	registerUnifiedRuntimeMetrics()
	return &Syncer{
		mgr:                   mgr,
		log:                   log.WithName("unified-runtime"),
		bootstrap:             bootstrap,
		reader:                reader,
		snap:                  snap,
		content:               content,
		activeSnapshotGVKKeys: make(map[string]struct{}),
	}
}

func copyLayeredState(src LayeredGVKState) LayeredGVKState {
	out := LayeredGVKState{DSCEligibleError: src.DSCEligibleError}
	out.BootstrapDesired = append([]unifiedbootstrap.UnifiedGVKPair(nil), src.BootstrapDesired...)
	out.EligibleFromDSC = append([]unifiedbootstrap.UnifiedGVKPair(nil), src.EligibleFromDSC...)
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

// Sync is safe to call from the DSC reconciler after successful status updates. Errors from individual
// watch registration are logged; the function returns nil unless a programming invariant breaks.
func (s *Syncer) Sync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state := BuildLayeredGVKState(ctx, s.reader, s.mgr.GetRESTMapper(), s.bootstrap, s.log)
	if state.DSCEligibleError != nil {
		s.log.Info("DSC list/parse for unified runtime sync failed; using bootstrap-only merge", "error", state.DSCEligibleError)
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

	for i := range state.ResolvedSnapshotGVKs {
		snapGVK, contentGVK := state.ResolvedSnapshotGVKs[i], state.ResolvedContentGVKs[i]
		if unifiedbootstrap.IsDedicatedSnapshotControllerKind(snapGVK.Kind) {
			continue
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
