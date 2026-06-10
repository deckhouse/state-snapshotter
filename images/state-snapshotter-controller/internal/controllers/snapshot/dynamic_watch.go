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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
)

// snapshotDynamicWatchManager registers one controller-runtime controller per child snapshot GVK
// so parent Snapshot reconciles when a referenced child snapshot changes (additive after startup).
type snapshotDynamicWatchManager struct {
	mu      sync.Mutex
	mgr     ctrl.Manager
	main    *SnapshotReconciler
	watched map[string]struct{}
}

func newSnapshotDynamicWatchManager(mgr ctrl.Manager, main *SnapshotReconciler) *snapshotDynamicWatchManager {
	return &snapshotDynamicWatchManager{
		mgr:     mgr,
		main:    main,
		watched: make(map[string]struct{}),
	}
}

// EnsureWatches registers missing watches for every snapshot kind in the live graph registry.
func (m *snapshotDynamicWatchManager) EnsureWatches(ctx context.Context, live snapshotgraphregistry.LiveReader) error {
	if m == nil || m.mgr == nil || live == nil {
		return nil
	}
	reg, err := usecase.EnsureGVKRegistryFromLive(ctx, live)
	if err != nil || reg == nil {
		return nil
	}
	for _, sk := range reg.RegisteredSnapshotKinds() {
		gvk, err := reg.ResolveSnapshotGVK(sk)
		if err != nil {
			continue
		}
		if err := m.ensureWatchLocked(ctx, gvk); err != nil {
			return err
		}
	}
	return nil
}

func (m *snapshotDynamicWatchManager) ensureWatchLocked(ctx context.Context, gvk schema.GroupVersionKind) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := gvk.String()
	if _, ok := m.watched[key]; ok {
		return nil
	}
	relay := &nssChildSnapshotWatchRelay{gvk: gvk, client: m.main.Client, main: m.main}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	name := controllerRuntimeNameForChildWatch(gvk)
	// Explicit allow-all predicate: status-only updates on child snapshots must enqueue the relay
	// (controller-runtime does not add GenerationChangedPredicate by default; this documents intent).
	passAll := predicate.NewPredicateFuncs(func(client.Object) bool { return true })
	if err := ctrl.NewControllerManagedBy(m.mgr).
		For(obj, builder.WithPredicates(passAll)).
		Named(name).
		Complete(relay); err != nil {
		return fmt.Errorf("register child snapshot watch for %s: %w", gvk.String(), err)
	}
	m.watched[key] = struct{}{}
	_ = ctx
	return nil
}

func controllerRuntimeNameForChildWatch(gvk schema.GroupVersionKind) string {
	// controller-runtime "Named" must be a valid DNS-1123 subdomain (<= 63 chars).
	// Hash the full GVK string so distinct kinds (e.g. two demo snapshots in the same API group)
	// never collide after truncation.
	sum := sha256.Sum256([]byte(gvk.String()))
	return "nss-chw-" + hex.EncodeToString(sum[:6])
}

// nssChildSnapshotWatchRelay forwards child snapshot events to parent Snapshot reconciles.
type nssChildSnapshotWatchRelay struct {
	gvk    schema.GroupVersionKind
	client client.Client
	main   *SnapshotReconciler
}

func (r *nssChildSnapshotWatchRelay) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).V(1).WithValues(
		"relay", "nss-child-snapshot",
		"childGVK", r.gvk.String(),
		"childNamespace", req.Namespace,
		"childName", req.Name,
	)
	logger.Info("nss child relay: reconcile triggered")

	var childReader client.Reader = r.client
	if r.main != nil && r.main.APIReader != nil {
		// Prefer reads from apiserver so this reconcile sees the triggering write
		// (cache can lag right after child status updates).
		childReader = r.main.APIReader
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(r.gvk)
	if err := childReader.Get(ctx, req.NamespacedName, u); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// Child snapshot deletion (or a delete that already raced past the cache): the object is
			// gone, so we cannot read its identity, but the relay already knows the child's GVK
			// (r.gvk) and its key (req). Synthesize a minimal child object and STILL wake every parent
			// Snapshot whose status.childrenSnapshotRefs reference it. Without this, a deleted child
			// Snapshot would not be propagated event-driven to the parent — invalidation/recovery would
			// rely only on the content-side ownerRef chain and the 30s polling fallback
			// (INV-FAIL-PROP: a missing required descendant must wake the parent so Ready is recomputed).
			logger.Info("nss child relay: child not found; waking referencing parents for deletion-driven recompute")
			return r.enqueueParentsForDeletedChild(ctx, childReader, req)
		}
		logger.Info("nss child relay: get child failed", "error", err.Error())
		return ctrl.Result{}, err
	}
	logger.Info("nss child relay: child fetched from reader",
		"apiVersion", u.GetAPIVersion(), "kind", u.GetKind(), "rv", u.GetResourceVersion())

	reqs := findParentsReferencingChildSnapshot(ctx, childReader, u)
	if len(reqs) == 0 {
		bound, hasBound, _ := unstructured.NestedString(u.Object, "status", "boundSnapshotContentName")
		if hasBound && bound != "" {
			// Domain controllers may patch child status (bound) before the parent Snapshot lists the
			// child in status.childrenSnapshotRefs; retry shortly instead of dropping the event.
			logger.Info("nss child relay: bound child but no parent Snapshot matched yet; requeue")
			return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
		}
		logger.Info("nss child relay: no Snapshot parents reference this child (strict ref mismatch or empty graph)")
		return ctrl.Result{}, nil
	}
	return r.reconcileParents(ctx, reqs)
}

// enqueueParentsForDeletedChild handles the delete event for a child snapshot whose object is already
// gone from the API. It reconstructs a minimal unstructured child from the relay's known GVK and the
// request key (namespace/name) — exactly the fields findParentsReferencingChildSnapshot matches on
// (apiVersion, kind, name, namespace) — and reconciles every parent Snapshot that still references it.
// This makes child-Snapshot deletion an event-driven parent wake-up instead of relying solely on the
// content-side ownerRef chain and the polling fallback.
func (r *nssChildSnapshotWatchRelay) enqueueParentsForDeletedChild(ctx context.Context, childReader client.Reader, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).V(1)
	if req.Namespace == "" {
		// Run tree is namespace-local; a cluster-scoped child key cannot match any namespaced parent.
		return ctrl.Result{}, nil
	}
	synthetic := buildSyntheticDeletedChild(r.gvk, req.Namespace, req.Name)
	reqs := findParentsReferencingChildSnapshot(ctx, childReader, synthetic)
	if len(reqs) == 0 {
		logger.Info("nss child relay: deleted child has no referencing parents (already pruned)")
		return ctrl.Result{}, nil
	}
	return r.reconcileParents(ctx, reqs)
}

// buildSyntheticDeletedChild reconstructs the minimal child snapshot identity (apiVersion, kind,
// namespace, name) needed to match parents' status.childrenSnapshotRefs after the real object has been
// deleted from the API. Only the fields findParentsReferencingChildSnapshot reads are populated.
func buildSyntheticDeletedChild(gvk schema.GroupVersionKind, namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

// reconcileParents drives parent Snapshot reconciles for the given requests and folds their results
// into a single best result (requeue wins, longest RequeueAfter wins) plus the first error. Returning
// the error preserves the controller-runtime backoff requeue of the child event so a transient parent
// failure is retried rather than dropped.
func (r *nssChildSnapshotWatchRelay) reconcileParents(ctx context.Context, reqs []reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).V(1)
	parentNames := make([]string, 0, len(reqs))
	for _, pr := range reqs {
		parentNames = append(parentNames, pr.Namespace+"/"+pr.Name)
	}
	logger.Info("nss child relay: enqueuing parent Snapshot reconciles", "parentCount", len(reqs), "parents", parentNames)

	var best ctrl.Result
	var firstErr error
	for _, pr := range reqs {
		res, err := r.main.Reconcile(ctx, pr)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if res.Requeue || res.RequeueAfter > best.RequeueAfter {
			best = res
		}
	}
	return best, firstErr
}
