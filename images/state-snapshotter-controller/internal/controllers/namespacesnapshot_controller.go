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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
	liblogger "github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// NamespaceSnapshotReconciler binds root NamespaceSnapshot to NamespaceSnapshotContent and drives manifest capture (MCR→MCP); status via conditions only (no status.phase).
// Root NamespaceSnapshotContent is not owned by NamespaceSnapshot (spec.namespaceSnapshotRef + status only).
// Child NamespaceSnapshotContent in the synthetic tree uses ownerReferences -> parent NamespaceSnapshotContent so GC can cascade the content tree.
type NamespaceSnapshotReconciler struct {
	Client                client.Client
	APIReader             client.Reader
	Dynamic               dynamic.Interface
	Scheme                *runtime.Scheme
	Config                *config.Options
	Archive               *usecase.ArchiveService
	SnapshotGraphRegistry snapshotgraphregistry.LiveReader
}

// AddNamespaceSnapshotControllerToManager registers the NamespaceSnapshot reconciler.
// snapshotGraphRegistry provides DSC/bootstrap snapshot↔content pairs for generic subtree graph and E5 child resolution (no domain imports in usecase).
func AddNamespaceSnapshotControllerToManager(mgr ctrl.Manager, cfg *config.Options, snapshotGraphRegistry snapshotgraphregistry.LiveReader) error {
	if cfg == nil {
		return fmt.Errorf("config must not be nil")
	}
	dyn, err := dynamic.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("namespace snapshot controller: dynamic client: %w", err)
	}
	logImpl, _ := liblogger.NewLogger("error")
	r := &NamespaceSnapshotReconciler{
		Client:                mgr.GetClient(),
		APIReader:             mgr.GetAPIReader(),
		Dynamic:               dyn,
		Scheme:                mgr.GetScheme(),
		Config:                cfg,
		Archive:               usecase.NewArchiveService(mgr.GetClient(), mgr.GetClient(), logImpl),
		SnapshotGraphRegistry: snapshotGraphRegistry,
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.NamespaceSnapshot{}).
		Watches(
			&storagev1alpha1.NamespaceSnapshotContent{},
			handler.EnqueueRequestsFromMapFunc(mapNamespaceSnapshotContentToNamespaceSnapshot),
		).
		// Secondary source: child NamespaceSnapshot status updates → parent reconcile (map only enqueues
		// for labelled synthetic children; not redundant with For()).
		Watches(
			&storagev1alpha1.NamespaceSnapshot{},
			handler.EnqueueRequestsFromMapFunc(mapSyntheticChildSnapshotToParent),
		).
		Complete(r)
}

// mapNamespaceSnapshotContentToNamespaceSnapshot requeues the NamespaceSnapshot named in
// spec.namespaceSnapshotRef when cluster-scoped content changes (spec repair, drift injection in tests, etc.).
func mapNamespaceSnapshotContentToNamespaceSnapshot(_ context.Context, o client.Object) []reconcile.Request {
	content, ok := o.(*storagev1alpha1.NamespaceSnapshotContent)
	if !ok {
		return nil
	}
	ref := content.Spec.NamespaceSnapshotRef
	if ref.Namespace == "" || ref.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}}}
}

// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshots,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshotcontents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshotcontents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=state-snapshotter.deckhouse.io,resources=manifestcapturerequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=state-snapshotter.deckhouse.io,resources=manifestcapturerequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=state-snapshotter.deckhouse.io,resources=manifestcheckpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=state-snapshotter.deckhouse.io,resources=manifestcheckpoints/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=deckhouse.io,resources=objectkeepers,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *NamespaceSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log.FromContext(ctx).V(1).Info("reconcile NamespaceSnapshot", "namespaceSnapshot", req.NamespacedName)
	nsSnap := &storagev1alpha1.NamespaceSnapshot{}
	if err := r.Client.Get(ctx, req.NamespacedName, nsSnap); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if nsSnap.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, nsSnap)
	}

	if snapshot.AddFinalizer(nsSnap, snapshot.FinalizerNamespaceSnapshot) {
		if err := r.Client.Update(ctx, nsSnap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
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

	if !namespacemanifest.IsSyntheticChildNamespaceSnapshot(nsSnap.GetLabels()) {
		pr, err := r.pruneSyntheticOwnedGraphRefsIfTreeDisabled(ctx, nsSnap)
		if err != nil {
			return ctrl.Result{}, err
		}
		if pr.Requeue || pr.RequeueAfter > 0 {
			return pr, nil
		}
	}

	expectedName := namespaceSnapshotContentName(nsSnap)

	if nsSnap.Status.BoundSnapshotContentName != "" && nsSnap.Status.BoundSnapshotContentName != expectedName {
		nsSnap.Status.BoundSnapshotContentName = ""
		meta.RemoveStatusCondition(&nsSnap.Status.Conditions, snapshot.ConditionBound)
		if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	content := &storagev1alpha1.NamespaceSnapshotContent{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: expectedName}, content)
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

		om, err := r.namespaceSnapshotContentObjectMeta(ctx, nsSnap)
		if err != nil {
			return ctrl.Result{}, err
		}
		newContent := &storagev1alpha1.NamespaceSnapshotContent{
			ObjectMeta: om,
			Spec:       desiredNamespaceSnapshotContentSpec(nsSnap),
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
			Message:            "NamespaceSnapshotContent exists",
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

	if !snapshotSubjectRefMatches(content.Spec.NamespaceSnapshotRef, nsSnap) {
		nsSnap.Status.ObservedGeneration = nsSnap.Generation
		meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "ContentRefMismatch",
			Message:            fmt.Sprintf("NamespaceSnapshotContent %q does not reference this NamespaceSnapshot", expectedName),
			ObservedGeneration: nsSnap.Generation,
		})
		meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionBound,
			Status:             metav1.ConditionFalse,
			Reason:             "ContentRefMismatch",
			Message:            "NamespaceSnapshotContent namespaceSnapshotRef does not match this object",
			ObservedGeneration: nsSnap.Generation,
		})
		if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if nsSnap.Status.BoundSnapshotContentName == "" {
		nsSnap.Status.BoundSnapshotContentName = expectedName
		nsSnap.Status.ObservedGeneration = nsSnap.Generation
		meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionBound,
			Status:             metav1.ConditionTrue,
			Reason:             "ContentBound",
			Message:            "NamespaceSnapshotContent exists and references this NamespaceSnapshot",
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
	if err := r.ensureSyntheticChildContentOwnerRef(ctx, nsSnap, content); err != nil {
		return ctrl.Result{}, err
	}
	return r.reconcileCaptureN2a(ctx, nsSnap, content)
}

func (r *NamespaceSnapshotReconciler) finishReconcileWithExistingContent(ctx context.Context, nsSnap *storagev1alpha1.NamespaceSnapshot, expectedName string) (ctrl.Result, error) {
	content := &storagev1alpha1.NamespaceSnapshotContent{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: expectedName}, content); err != nil {
		return ctrl.Result{}, err
	}
	if !snapshotSubjectRefMatches(content.Spec.NamespaceSnapshotRef, nsSnap) {
		nsSnap.Status.ObservedGeneration = nsSnap.Generation
		meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "ContentRefMismatch",
			Message:            fmt.Sprintf("existing NamespaceSnapshotContent %q does not reference this NamespaceSnapshot", expectedName),
			ObservedGeneration: nsSnap.Generation,
		})
		if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	nsSnap.Status.BoundSnapshotContentName = expectedName
	nsSnap.Status.ObservedGeneration = nsSnap.Generation
	meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
		Type:               snapshot.ConditionBound,
		Status:             metav1.ConditionTrue,
		Reason:             "ContentExists",
		Message:            "NamespaceSnapshotContent already existed with matching namespaceSnapshotRef",
		ObservedGeneration: nsSnap.Generation,
	})
	if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// reconcileDelete removes the NamespaceSnapshot finalizer. It does not delete ManifestCheckpoint, chunks, or MCR;
// retained manifest artifacts follow NamespaceSnapshotContent lifecycle (separate from snapshot object deletion).
func (r *NamespaceSnapshotReconciler) reconcileDelete(ctx context.Context, nsSnap *storagev1alpha1.NamespaceSnapshot) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(nsSnap)
	fresh := &storagev1alpha1.NamespaceSnapshot{}
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
		if err := r.updateNamespaceSnapshotRemoveFinalizer(ctx, key); err != nil {
			return ctrl.Result{}, err
		}
		log.FromContext(ctx).V(1).Info("namespace snapshot delete reconcile done (no bound content)")
		return ctrl.Result{}, nil
	}

	contentKey := client.ObjectKey{Name: fresh.Status.BoundSnapshotContentName}
	content := &storagev1alpha1.NamespaceSnapshotContent{}
	err := r.Client.Get(ctx, contentKey, content)
	if errors.IsNotFound(err) {
		if err := r.updateNamespaceSnapshotRemoveFinalizer(ctx, key); err != nil {
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
		// Do not remove the root finalizer until NamespaceSnapshotContent is fully gone from the API.
		if err := r.Client.Get(ctx, contentKey, content); err == nil {
			return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	if err := r.updateNamespaceSnapshotRemoveFinalizer(ctx, key); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).V(1).Info("namespace snapshot delete reconcile done")
	return ctrl.Result{}, nil
}

func (r *NamespaceSnapshotReconciler) updateNamespaceSnapshotRemoveFinalizer(ctx context.Context, key client.ObjectKey) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.NamespaceSnapshot{}
		if err := r.Client.Get(ctx, key, cur); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if cur.DeletionTimestamp == nil {
			return nil
		}
		if !snapshot.RemoveFinalizer(cur, snapshot.FinalizerNamespaceSnapshot) {
			return nil
		}
		return r.Client.Update(ctx, cur)
	})
}

func desiredNamespaceSnapshotContentSpec(nsSnap *storagev1alpha1.NamespaceSnapshot) storagev1alpha1.NamespaceSnapshotContentSpec {
	return storagev1alpha1.NamespaceSnapshotContentSpec{
		NamespaceSnapshotRef: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
			Kind:       "NamespaceSnapshot",
			Name:       nsSnap.Name,
			Namespace:  nsSnap.Namespace,
			UID:        nsSnap.UID,
		},
		DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
	}
}

func snapshotSubjectRefMatches(ref storagev1alpha1.SnapshotSubjectRef, ns *storagev1alpha1.NamespaceSnapshot) bool {
	return ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() &&
		ref.Kind == "NamespaceSnapshot" &&
		ref.Name == ns.Name &&
		ref.Namespace == ns.Namespace &&
		ref.UID == ns.UID
}

func namespaceSnapshotContentName(ns *storagev1alpha1.NamespaceSnapshot) string {
	uid := strings.ReplaceAll(string(ns.UID), "-", "")
	return fmt.Sprintf("ns-%s", uid)
}

// namespaceSnapshotContentObjectMeta builds metadata for a new NamespaceSnapshotContent.
// Synthetic child snapshots add ownerReferences -> parent NamespaceSnapshotContent (scaffold until domain wiring).
func (r *NamespaceSnapshotReconciler) namespaceSnapshotContentObjectMeta(ctx context.Context, nsSnap *storagev1alpha1.NamespaceSnapshot) (metav1.ObjectMeta, error) {
	om := metav1.ObjectMeta{
		Name:       namespaceSnapshotContentName(nsSnap),
		Finalizers: []string{snapshot.FinalizerParentProtect},
	}
	if !namespacemanifest.IsSyntheticChildNamespaceSnapshot(nsSnap.GetLabels()) {
		return om, nil
	}
	parentName := nsSnap.GetLabels()[namespacemanifest.LabelSyntheticParentName]
	if parentName == "" {
		return om, nil
	}
	parent := &storagev1alpha1.NamespaceSnapshot{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: nsSnap.Namespace, Name: parentName}, parent); err != nil {
		if errors.IsNotFound(err) {
			return om, fmt.Errorf("synthetic child: parent NamespaceSnapshot %s/%s not found yet", nsSnap.Namespace, parentName)
		}
		return om, err
	}
	pc := &storagev1alpha1.NamespaceSnapshotContent{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: namespaceSnapshotContentName(parent)}, pc); err != nil {
		return om, err
	}
	om.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "NamespaceSnapshotContent",
		Name:       pc.Name,
		UID:        pc.UID,
	}}
	return om, nil
}

// ensureSyntheticChildContentOwnerRef patches an existing child NamespaceSnapshotContent if it lacks ownerRef to the parent content.
func (r *NamespaceSnapshotReconciler) ensureSyntheticChildContentOwnerRef(ctx context.Context, nsSnap *storagev1alpha1.NamespaceSnapshot, content *storagev1alpha1.NamespaceSnapshotContent) error {
	if !namespacemanifest.IsSyntheticChildNamespaceSnapshot(nsSnap.GetLabels()) {
		return nil
	}
	parentName := nsSnap.GetLabels()[namespacemanifest.LabelSyntheticParentName]
	if parentName == "" {
		return nil
	}
	parent := &storagev1alpha1.NamespaceSnapshot{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: nsSnap.Namespace, Name: parentName}, parent); err != nil {
		return client.IgnoreNotFound(err)
	}
	parentNSCName := namespaceSnapshotContentName(parent)
	pc := &storagev1alpha1.NamespaceSnapshotContent{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: parentNSCName}, pc); err != nil {
		return err
	}
	want := metav1.OwnerReference{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "NamespaceSnapshotContent",
		Name:       pc.Name,
		UID:        pc.UID,
	}
	for _, ref := range content.OwnerReferences {
		if ref.Kind == want.Kind && ref.APIVersion == want.APIVersion && ref.UID == want.UID {
			return nil
		}
	}
	base := content.DeepCopy()
	content.OwnerReferences = append(content.OwnerReferences, want)
	return r.Client.Patch(ctx, content, client.MergeFrom(base))
}
