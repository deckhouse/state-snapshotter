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
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// NamespaceSnapshotContentReconciler implements unified-snapshot-deletion-algorithm: orphan finalizer,
// optional scheduled delete of retained content, and child finalizer strip on delete (no Delete(children)).
type NamespaceSnapshotContentReconciler struct {
	Client client.Client
}

// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshotcontents,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshots,verbs=get;list;watch

// AddNamespaceSnapshotContentControllerToManager registers the reconciler.
func AddNamespaceSnapshotContentControllerToManager(mgr ctrl.Manager, _ *config.Options) error {
	r := &NamespaceSnapshotContentReconciler{
		Client: mgr.GetClient(),
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.NamespaceSnapshotContent{}).
		Watches(
			&storagev1alpha1.NamespaceSnapshot{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
				return mapDeletingNamespaceSnapshotToContents(ctx, mgr.GetClient(), o)
			}),
		).
		Complete(r)
}

func mapDeletingNamespaceSnapshotToContents(ctx context.Context, c client.Client, o client.Object) []reconcile.Request {
	snap, ok := o.(*storagev1alpha1.NamespaceSnapshot)
	if !ok || snap.DeletionTimestamp == nil {
		return nil
	}
	var list storagev1alpha1.NamespaceSnapshotContentList
	if err := c.List(ctx, &list); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		ref := list.Items[i].Spec.NamespaceSnapshotRef
		if ref.Namespace == snap.Namespace && ref.Name == snap.Name && ref.UID == snap.UID {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: list.Items[i].Name}})
		}
	}
	return out
}

func (r *NamespaceSnapshotContentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("namespaceSnapshotContent", req.Name)
	ctx = log.IntoContext(ctx, logger)

	nsc := &storagev1alpha1.NamespaceSnapshotContent{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: req.Name}, nsc); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !nsc.DeletionTimestamp.IsZero() {
		return r.reconcileDeleting(ctx, nsc)
	}
	return r.reconcileLiving(ctx, nsc)
}

func (r *NamespaceSnapshotContentReconciler) reconcileLiving(ctx context.Context, nsc *storagev1alpha1.NamespaceSnapshotContent) (ctrl.Result, error) {
	ref := nsc.Spec.NamespaceSnapshotRef
	snap := &storagev1alpha1.NamespaceSnapshot{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, snap)
	orphan := false
	if errors.IsNotFound(err) {
		orphan = true
	} else if err != nil {
		return ctrl.Result{}, err
	} else if string(snap.UID) != string(ref.UID) {
		orphan = true
	}
	if orphan && snapshot.RemoveFinalizer(nsc, snapshot.FinalizerParentProtect) {
		if err := r.Client.Update(ctx, nsc); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	raw := ""
	if nsc.Annotations != nil {
		raw = nsc.Annotations[namespacemanifest.AnnotationOrphanPurgeAt]
	}
	if raw == "" {
		return ctrl.Result{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return ctrl.Result{}, nil
	}
	now := time.Now().UTC()
	if now.Before(t) {
		d := time.Until(t)
		if d < time.Second {
			d = time.Second
		}
		if d > 2*time.Minute {
			d = 2 * time.Minute
		}
		return ctrl.Result{RequeueAfter: d}, nil
	}
	if err := r.Client.Delete(ctx, nsc); err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *NamespaceSnapshotContentReconciler) reconcileDeleting(ctx context.Context, nsc *storagev1alpha1.NamespaceSnapshotContent) (ctrl.Result, error) {
	for _, ch := range nsc.Status.ChildrenSnapshotContentRefs {
		if ch.Name == "" {
			continue
		}
		child := &storagev1alpha1.NamespaceSnapshotContent{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: ch.Name}, child); err != nil {
			continue
		}
		if snapshot.RemoveFinalizer(child, snapshot.FinalizerParentProtect) {
			if err := r.Client.Update(ctx, child); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	return ctrl.Result{}, nil
}
