/*
Copyright 2025 Flant JSC

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

package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
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
	Scheme                *runtime.Scheme
	Config                *config.Options
	Archive               *usecase.ArchiveService
	SnapshotGraphRegistry snapshotgraphregistry.LiveReader
	Mgr                   ctrl.Manager
	childWatchMgr         *snapshotDynamicWatchManager
}

// e6ChildStatusReader returns the client used for E6 child snapshot reads.
// The split-client cache is invalidated by watch-driven updates in the same manager process; using the
// API reader here can return unstructured objects whose status shape is harder to parse consistently
// for demo CRDs in envtest.
func (r *SnapshotReconciler) e6ChildStatusReader() client.Reader {
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
// snapshotGraphRegistry provides DSC/bootstrap snapshot↔content pairs for generic subtree graph and E5 child resolution (no domain imports in usecase).
// Child snapshot watches are registered dynamically from the live registry (see snapshotDynamicWatchManager).
func AddSnapshotControllerToManager(mgr ctrl.Manager, cfg *config.Options, snapshotGraphRegistry snapshotgraphregistry.LiveReader) error {
	if cfg == nil {
		return fmt.Errorf("config must not be nil")
	}
	dyn, err := dynamic.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("snapshot controller: dynamic client: %w", err)
	}
	logImpl, _ := liblogger.NewLogger("error")
	r := &SnapshotReconciler{
		Client:                mgr.GetClient(),
		APIReader:             mgr.GetAPIReader(),
		Dynamic:               dyn,
		Scheme:                mgr.GetScheme(),
		Config:                cfg,
		Archive:               usecase.NewArchiveService(mgr.GetClient(), mgr.GetClient(), logImpl),
		SnapshotGraphRegistry: snapshotGraphRegistry,
		Mgr:                   mgr,
	}
	r.childWatchMgr = newSnapshotDynamicWatchManager(mgr, r)
	b := ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.Snapshot{})
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

	if snapshot.AddFinalizer(nsSnap, snapshot.FinalizerSnapshot) {
		if err := r.Client.Update(ctx, nsSnap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	rootOK, res, err := ensureRootObjectKeeperWithTTL(
		ctx,
		r.Client,
		r.APIReader,
		r.Config,
		nsSnap,
		storagev1alpha1.SchemeGroupVersion.WithKind(KindSnapshot),
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	if res.Requeue || res.RequeueAfter > 0 {
		return res, nil
	}
	if _, err := ensureLifecycleOwnerRef(ctx, r.Client, nsSnap, rootObjectKeeperOwnerReference(rootOK)); err != nil {
		return ctrl.Result{}, err
	}

	var ns corev1.Namespace
	if err := r.Client.Get(ctx, client.ObjectKey{Name: nsSnap.Namespace}, &ns); err != nil {
		if errors.IsNotFound(err) {
			nsSnap.Status.ObservedGeneration = nsSnap.Generation
			meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
				Type:               snapshot.ConditionReady,
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

	expectedName := snapshotContentName(nsSnap)

	if nsSnap.Status.BoundSnapshotContentName != "" && nsSnap.Status.BoundSnapshotContentName != expectedName {
		nsSnap.Status.BoundSnapshotContentName = ""
		meta.RemoveStatusCondition(&nsSnap.Status.Conditions, snapshot.ConditionBound)
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
			meta.RemoveStatusCondition(&nsSnap.Status.Conditions, snapshot.ConditionBound)
			meta.RemoveStatusCondition(&nsSnap.Status.Conditions, snapshot.ConditionReady)
			nsSnap.Status.ObservedGeneration = nsSnap.Generation
			if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}

		om := snapshotContentObjectMeta(nsSnap)
		om.OwnerReferences = []metav1.OwnerReference{rootObjectKeeperOwnerReference(rootOK)}
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
		meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionBound,
			Status:             metav1.ConditionTrue,
			Reason:             "ContentCreated",
			Message:            "SnapshotContent exists",
			ObservedGeneration: nsSnap.Generation,
		})
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
		meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionBound,
			Status:             metav1.ConditionTrue,
			Reason:             "ContentBound",
			Message:            "SnapshotContent exists and references this Snapshot",
			ObservedGeneration: nsSnap.Generation,
		})
		if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if err := r.Client.Get(ctx, client.ObjectKey{Name: expectedName}, content); err != nil {
		return ctrl.Result{}, err
	}
	graphChanged, err := r.reconcileParentOwnedChildGraph(ctx, nsSnap, content)
	if err != nil {
		if patchErr := r.patchSnapshotGraphReady(ctx, types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, metav1.ConditionFalse, snapshot.ReasonGraphPlanningFailed, err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}
	if graphChanged {
		return ctrl.Result{Requeue: true}, nil
	}
	graphPublished, err := publishSnapshotContentChildrenFromSnapshotRefs(ctx, r.Client, nsSnap.Namespace, content.Name, nsSnap.Status.ChildrenSnapshotRefs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !graphPublished {
		return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
	}
	return r.reconcileCaptureN2a(ctx, nsSnap, content)
}

func (r *SnapshotReconciler) finishReconcileWithExistingContent(ctx context.Context, nsSnap *storagev1alpha1.Snapshot, expectedName string) (ctrl.Result, error) {
	content := &storagev1alpha1.SnapshotContent{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: expectedName}, content); err != nil {
		return ctrl.Result{}, err
	}
	nsSnap.Status.BoundSnapshotContentName = expectedName
	nsSnap.Status.ObservedGeneration = nsSnap.Generation
	meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
		Type:               snapshot.ConditionBound,
		Status:             metav1.ConditionTrue,
		Reason:             "ContentExists",
		Message:            "SnapshotContent already exists",
		ObservedGeneration: nsSnap.Generation,
	})
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
		if !snapshot.RemoveFinalizer(cur, snapshot.FinalizerSnapshot) {
			return nil
		}
		return r.Client.Update(ctx, cur)
	})
}

func desiredSnapshotContentSpec(_ *storagev1alpha1.Snapshot) storagev1alpha1.SnapshotContentSpec {
	return storagev1alpha1.SnapshotContentSpec{
		DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
	}
}

func snapshotContentName(ns *storagev1alpha1.Snapshot) string {
	uid := strings.ReplaceAll(string(ns.UID), "-", "")
	return fmt.Sprintf("ns-%s", uid)
}

// snapshotContentObjectMeta builds metadata for a new SnapshotContent.
func snapshotContentObjectMeta(nsSnap *storagev1alpha1.Snapshot) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:       snapshotContentName(nsSnap),
		Finalizers: []string{snapshot.FinalizerParentProtect},
	}
}
