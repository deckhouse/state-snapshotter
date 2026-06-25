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
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
	liblogger "github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// SnapshotReconciler owns namespace root discovery, top-level child
// snapshot refs, MCR creation for the namespace own manifest scope, and binding
// the root to common SnapshotContent. SnapshotContent status/result aggregation
// stays in SnapshotContentController.
// Root SnapshotContent is not owned by Snapshot; binding lives in Snapshot status.
type SnapshotReconciler struct {
	Client                client.Client
	APIReader             client.Reader
	Dynamic               dynamic.Interface
	Discovery             discovery.DiscoveryInterface
	Scheme                *runtime.Scheme
	Config                *config.Options
	Archive               *usecase.ArchiveService
	SnapshotGraphRegistry snapshotgraphregistry.LiveReader
	Mgr                   ctrl.Manager
	childWatchMgr         *snapshotDynamicWatchManager
}

// childSnapshotStatusReader returns the client used for child snapshot status reads.
// The split-client cache is invalidated by watch-driven updates in the same manager process; using the
// API reader here can return unstructured objects whose status shape is harder to parse consistently
// for demo CRDs in envtest.
func (r *SnapshotReconciler) childSnapshotStatusReader() client.Reader {
	return r.Client
}

// snapshotReader returns a reader that prefers the API reader for Snapshot reads.
// The Snapshot controller owns status.childrenSnapshotRefs; the split client cache can lag
// behind status updates and a subsequent Status().Update would otherwise wipe fresh graph refs.
func (r *SnapshotReconciler) snapshotReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// AddSnapshotControllerToManager registers the Snapshot reconciler.
// snapshotGraphRegistry provides CSD/bootstrap snapshot↔content pairs for generic subtree graph and E5 child resolution (no domain imports in usecase).
// Child snapshot watches are registered dynamically from the live registry (see snapshotDynamicWatchManager).
func AddSnapshotControllerToManager(mgr ctrl.Manager, cfg *config.Options, snapshotGraphRegistry snapshotgraphregistry.LiveReader) error {
	if cfg == nil {
		return fmt.Errorf("config must not be nil")
	}
	dyn, err := dynamic.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("snapshot controller: dynamic client: %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("snapshot controller: discovery client: %w", err)
	}
	logImpl, _ := liblogger.NewLogger("error")
	r := &SnapshotReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Dynamic:   dyn,
		Discovery: disco,
		Scheme:    mgr.GetScheme(),
		Config:    cfg,
		// Chunks are internal-only (no list/watch informer); use APIReader like the /manifests API server.
		Archive:               usecase.NewArchiveService(mgr.GetAPIReader(), mgr.GetAPIReader(), logImpl),
		SnapshotGraphRegistry: snapshotGraphRegistry,
		Mgr:                   mgr,
	}
	r.childWatchMgr = newSnapshotDynamicWatchManager(mgr, r)
	if err := registerSnapshotBoundContentFieldIndex(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}
	// Status-only SnapshotContent updates must enqueue the bound Snapshot (Ready propagation).
	passAll := predicate.NewPredicateFuncs(func(client.Object) bool { return true })
	b := ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.Snapshot{}).
		WithOptions(controller.Options{
			// Bound the per-item retry backoff for the Snapshot controller only (domain controllers keep the
			// controller-runtime default, where a not-found MCR target is critical). Namespace manifest capture
			// races against ephemeral-target churn: an MCR admission rejection ("target not found in namespace")
			// or a similar transient surfaces as a reconcile error, and the default rate limiter backs off up to
			// ~16min, so a wedged capture re-plans far too slowly. Capping the exponential backoff at 10s keeps
			// the re-plan/retry loop tight (200ms floor -> 10s ceiling) without hot-looping a wedged item.
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](200*time.Millisecond, 10*time.Second),
		}).
		Watches(
			&storagev1alpha1.SnapshotContent{},
			snapshotContentToSnapshotEnqueueHandler(mgr.GetClient()),
			builder.WithPredicates(passAll),
		)
	b = r.addOrphanVolumeSnapshotWatch(b, mgr)
	return b.Complete(r)
}

func (r *SnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log.FromContext(ctx).V(1).Info("reconcile Snapshot", "snapshot", req.NamespacedName)
	nsSnap := &storagev1alpha1.Snapshot{}
	if err := r.snapshotReader().Get(ctx, req.NamespacedName, nsSnap); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if r.childWatchMgr != nil && r.SnapshotGraphRegistry != nil {
		if err := r.childWatchMgr.EnsureWatches(ctx, r.SnapshotGraphRegistry); err != nil {
			log.FromContext(ctx).Error(err, "ensure dynamic child snapshot watches")
		}
	}

	if nsSnap.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, nsSnap)
	}

	if snapshotpkg.AddFinalizer(nsSnap, snapshotpkg.FinalizerSnapshot) {
		if err := r.Client.Update(ctx, nsSnap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	rootOK, res, err := controllercommon.EnsureRootObjectKeeperWithTTL(
		ctx,
		r.Client,
		r.APIReader,
		r.Config,
		nsSnap,
		storagev1alpha1.SchemeGroupVersion.WithKind(controllercommon.KindSnapshot),
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	if res.Requeue || res.RequeueAfter > 0 {
		return res, nil
	}

	var ns corev1.Namespace
	if err := r.Client.Get(ctx, client.ObjectKey{Name: nsSnap.Namespace}, &ns); err != nil {
		if errors.IsNotFound(err) {
			nsSnap.Status.ObservedGeneration = nsSnap.Generation
			meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
				Type:               snapshotpkg.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             "NamespaceNotFound",
				Message:            fmt.Sprintf("namespace %q does not exist", nsSnap.Namespace),
				ObservedGeneration: nsSnap.Generation,
			})
			if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	_ = ns

	// CSI-like static (pre-provisioning) bind: when spec.source.snapshotContentName is set the
	// Snapshot binds to existing pre-provisioned content (created by the import path) instead of
	// running dynamic capture. This MUST be handled before the deterministic expectedName logic below,
	// which would otherwise reset the bind (the static bind points at the import-chosen content name).
	// The root ObjectKeeper ensured above is intentionally kept for static-bind Snapshots too: it
	// TTL-cleans the Snapshot record itself (its cascade to retained content is simply a no-op here,
	// since the pre-provisioned content is owned via the import path, not re-owned on this path).
	if nsSnap.IsStaticBind() {
		return r.reconcileStaticBind(ctx, nsSnap)
	}

	// Import-mode Snapshots (spec.source.import) are materialized from an uploaded payload
	// (manifests-and-children-refs-upload) — the controller MUST NOT capture the live namespace. The
	// import orchestrator reconstructs the SnapshotContent from the uploaded ManifestCheckpoint + child
	// refs and binds it (the root structural node carries no data leg); it falls back to a non-terminal
	// pending hold until the upload arrives.
	if nsSnap.IsImportMode() {
		return r.reconcileImport(ctx, nsSnap, rootOK)
	}

	expectedName := snapshotContentName(nsSnap)

	if nsSnap.Status.BoundSnapshotContentName != "" && nsSnap.Status.BoundSnapshotContentName != expectedName {
		nsSnap.Status.BoundSnapshotContentName = ""
		if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	content := &storagev1alpha1.SnapshotContent{}
	err = r.Client.Get(ctx, client.ObjectKey{Name: expectedName}, content)
	if errors.IsNotFound(err) {
		if nsSnap.Status.BoundSnapshotContentName != "" {
			nsSnap.Status.BoundSnapshotContentName = ""
			meta.RemoveStatusCondition(&nsSnap.Status.Conditions, snapshotpkg.ConditionReady)
			nsSnap.Status.ObservedGeneration = nsSnap.Generation
			if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}

		om := snapshotContentObjectMeta(nsSnap)
		om.OwnerReferences = []metav1.OwnerReference{controllercommon.RootObjectKeeperOwnerReference(rootOK)}
		newContent := &storagev1alpha1.SnapshotContent{
			ObjectMeta: om,
			Spec:       desiredSnapshotContentSpec(nsSnap),
		}
		if err := r.Client.Create(ctx, newContent); err != nil {
			if errors.IsAlreadyExists(err) {
				return r.finishReconcileWithExistingContent(ctx, nsSnap, expectedName)
			}
			return ctrl.Result{}, err
		}
		nsSnap.Status.BoundSnapshotContentName = expectedName
		nsSnap.Status.ObservedGeneration = nsSnap.Generation
		if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	if nsSnap.Status.BoundSnapshotContentName == "" {
		nsSnap.Status.BoundSnapshotContentName = expectedName
		nsSnap.Status.ObservedGeneration = nsSnap.Generation
		if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if err := r.Client.Get(ctx, client.ObjectKey{Name: expectedName}, content); err != nil {
		return ctrl.Result{}, err
	}
	graphChanged, graphReady, err := r.reconcileParentOwnedChildGraph(ctx, nsSnap, content)
	if err != nil {
		if patchErr := r.patchSnapshotChildrenSnapshotReady(ctx, types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, metav1.ConditionFalse, snapshotpkg.ReasonGraphPlanningFailed, err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}
	if res, block := childGraphCaptureGate(graphChanged, graphReady); block {
		return res, nil
	}
	graphPublished, err := snapshotcontent.PublishSnapshotContentChildrenFromSnapshotRefs(ctx, r.Client, r.snapshotReader(), nsSnap.Namespace, content.Name, nsSnap.Status.ChildrenSnapshotRefs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !graphPublished {
		return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
	}
	return r.reconcileCaptureN2a(ctx, nsSnap, content)
}

// snapshotChildGraphPollInterval is the polling fallback cadence used while a priority layer is
// pending ChildrenSnapshotReady. It is NOT a deadline: child snapshots may stay pending for hours. Child watches
// are the primary wake-up; this RequeueAfter only covers a missed watch event so the parent does not
// stall if a child-kind notification is dropped.
const snapshotChildGraphPollInterval = 30 * time.Second

// childGraphCaptureGate decides how reconcile proceeds after child-graph planning and reports whether
// capture must be blocked (block=true means return the result, do not capture):
//   - graphChanged: planner just wrote status; requeue immediately so the fresh status is re-read.
//     This is cheap (also woken by the self-watch) and avoids a 30s delay on an ordinary status update.
//   - !graphReady: a priority layer is still pending; requeue via RequeueAfter polling fallback. This
//     is intentionally unbounded — a child snapshot may stay pending for hours — and never a deadline.
//   - otherwise: do not block; proceed to capture.
func childGraphCaptureGate(graphChanged, graphReady bool) (ctrl.Result, bool) {
	if graphChanged {
		return ctrl.Result{Requeue: true}, true
	}
	if !graphReady {
		return ctrl.Result{RequeueAfter: snapshotChildGraphPollInterval}, true
	}
	return ctrl.Result{}, false
}

func (r *SnapshotReconciler) finishReconcileWithExistingContent(ctx context.Context, nsSnap *storagev1alpha1.Snapshot, expectedName string) (ctrl.Result, error) {
	content := &storagev1alpha1.SnapshotContent{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: expectedName}, content); err != nil {
		return ctrl.Result{}, err
	}
	nsSnap.Status.BoundSnapshotContentName = expectedName
	nsSnap.Status.ObservedGeneration = nsSnap.Generation
	if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// reconcileDelete removes the Snapshot finalizer. It does not delete ManifestCheckpoint, chunks, or MCR;
// retained manifest artifacts follow SnapshotContent lifecycle (separate from snapshot object deletion).
func (r *SnapshotReconciler) reconcileDelete(ctx context.Context, nsSnap *storagev1alpha1.Snapshot) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(nsSnap)
	fresh := &storagev1alpha1.Snapshot{}
	if err := r.Client.Get(ctx, key, fresh); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if fresh.DeletionTimestamp == nil {
		return ctrl.Result{}, nil
	}

	if fresh.Status.BoundSnapshotContentName == "" {
		// An import Snapshot deleted before the import orchestrator (C5) materializes/binds its
		// SnapshotContent may still own a reconstructed (ownerless, cluster-scoped) ManifestCheckpoint
		// from manifests-and-children-refs-upload. Nothing else GCs it in this window, so clean it up
		// here before dropping the finalizer. Non import-mode snapshots have no such artifact.
		if fresh.IsImportMode() {
			if err := usecase.DeleteReconstructedManifestCheckpoint(ctx, r.Client, fresh.UID); err != nil {
				return ctrl.Result{}, err
			}
		}
		if err := r.updateSnapshotRemoveFinalizer(ctx, key); err != nil {
			return ctrl.Result{}, err
		}
		log.FromContext(ctx).V(1).Info("snapshot delete reconcile done (no bound content)")
		return ctrl.Result{}, nil
	}

	contentKey := client.ObjectKey{Name: fresh.Status.BoundSnapshotContentName}
	content := &storagev1alpha1.SnapshotContent{}
	err := r.Client.Get(ctx, contentKey, content)
	if errors.IsNotFound(err) {
		if err := r.updateSnapshotRemoveFinalizer(ctx, key); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	policy := content.Spec.DeletionPolicy
	if policy == storagev1alpha1.SnapshotContentDeletionPolicyDelete {
		if err := r.Client.Delete(ctx, content); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		// Do not remove the root finalizer until SnapshotContent is fully gone from the API.
		if err := r.Client.Get(ctx, contentKey, content); err == nil {
			return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	if err := r.updateSnapshotRemoveFinalizer(ctx, key); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).V(1).Info("snapshot delete reconcile done")
	return ctrl.Result{}, nil
}

func (r *SnapshotReconciler) updateSnapshotRemoveFinalizer(ctx context.Context, key client.ObjectKey) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, key, cur); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if cur.DeletionTimestamp == nil {
			return nil
		}
		if !snapshotpkg.RemoveFinalizer(cur, snapshotpkg.FinalizerSnapshot) {
			return nil
		}
		return r.Client.Update(ctx, cur)
	})
}

func desiredSnapshotContentSpec(nsSnap *storagev1alpha1.Snapshot) storagev1alpha1.SnapshotContentSpec {
	return controllercommon.NewSnapshotContentSpec(
		storagev1alpha1.SnapshotContentDeletionPolicyRetain,
		controllercommon.SnapshotSubjectRefFromSnapshot(nsSnap),
	)
}

func snapshotContentName(ns *storagev1alpha1.Snapshot) string {
	uid := strings.ReplaceAll(string(ns.UID), "-", "")
	return fmt.Sprintf("ns-%s", uid)
}

// snapshotContentObjectMeta builds metadata for a new SnapshotContent.
func snapshotContentObjectMeta(nsSnap *storagev1alpha1.Snapshot) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:       snapshotContentName(nsSnap),
		Finalizers: []string{snapshotpkg.FinalizerParentProtect},
	}
}
