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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// NamespaceSnapshotReconciler implements N1 bind + N2a manifest capture (MCR→MCP); status via conditions only (no status.phase).
// Binding uses status + spec.namespaceSnapshotRef only (no ownerReference on cluster NamespaceSnapshotContent).
type NamespaceSnapshotReconciler struct {
	Client    client.Client
	APIReader client.Reader
	Dynamic   dynamic.Interface
	Scheme    *runtime.Scheme
	Config    *config.Options
}

// AddNamespaceSnapshotControllerToManager registers the NamespaceSnapshot reconciler.
func AddNamespaceSnapshotControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
	if cfg == nil {
		return fmt.Errorf("config must not be nil")
	}
	dyn, err := dynamic.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("namespace snapshot controller: dynamic client: %w", err)
	}
	r := &NamespaceSnapshotReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Dynamic:   dyn,
		Scheme:    mgr.GetScheme(),
		Config:    cfg,
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.NamespaceSnapshot{}).
		Watches(
			&storagev1alpha1.NamespaceSnapshotContent{},
			handler.EnqueueRequestsFromMapFunc(mapNamespaceSnapshotContentToNamespaceSnapshot),
		).
		Watches(
			&storagev1alpha1.NamespaceSnapshot{},
			handler.EnqueueRequestsFromMapFunc(mapSyntheticChildNamespaceSnapshotToParent),
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

		newContent := &storagev1alpha1.NamespaceSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: expectedName},
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
			Message:            fmt.Sprintf("existing NamespaceSnapshotContent %q is not owned by this NamespaceSnapshot", expectedName),
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

// reconcileDelete implements N2a cancel/cleanup policy when NamespaceSnapshot is deleted (design §5.2):
// 1) Delete ManifestCaptureRequest (§4.7).
// 2) Requeue until MCR is gone.
// 3) Best-effort Delete(ManifestCheckpoint); chunks GC via ownerRef on MCP.
// 4) NamespaceSnapshotContent + root finalizer per deletion policy.
func (r *NamespaceSnapshotReconciler) reconcileDelete(ctx context.Context, nsSnap *storagev1alpha1.NamespaceSnapshot) (ctrl.Result, error) {
	mcrKey := client.ObjectKey{Namespace: nsSnap.Namespace, Name: namespacemanifest.NamespaceSnapshotMCRName(nsSnap.UID)}
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	var mcpName string

	if err := r.Client.Get(ctx, mcrKey, mcr); err == nil {
		mcpName = namespacemanifest.GenerateManifestCheckpointNameFromUID(mcr.UID)
		if err := r.Client.Delete(ctx, mcr); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	} else if !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	if mcpName == "" && nsSnap.Status.BoundSnapshotContentName != "" {
		contentForMCP := &storagev1alpha1.NamespaceSnapshotContent{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: nsSnap.Status.BoundSnapshotContentName}, contentForMCP); err == nil {
			mcpName = contentForMCP.Status.ManifestCheckpointName
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	if err := r.Client.Get(ctx, mcrKey, mcr); err == nil {
		return ctrl.Result{RequeueAfter: 300 * time.Millisecond}, nil
	} else if !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	if mcpName != "" {
		mcp := &ssv1alpha1.ManifestCheckpoint{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err == nil {
			// Step 3 of §5.2 / reconcileDelete policy: explicit MCP delete after MCR is gone (see function doc).
			if err := r.Client.Delete(ctx, mcp); err != nil && !errors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err == nil {
			return ctrl.Result{RequeueAfter: 300 * time.Millisecond}, nil
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	if nsSnap.Status.BoundSnapshotContentName == "" {
		if snapshot.RemoveFinalizer(nsSnap, snapshot.FinalizerNamespaceSnapshot) {
			if err := r.Client.Update(ctx, nsSnap); err != nil {
				return ctrl.Result{}, err
			}
		}
		log.FromContext(ctx).V(1).Info("namespace snapshot delete reconcile done (no bound content)")
		return ctrl.Result{}, nil
	}

	contentKey := client.ObjectKey{Name: nsSnap.Status.BoundSnapshotContentName}
	content := &storagev1alpha1.NamespaceSnapshotContent{}
	err := r.Client.Get(ctx, contentKey, content)
	if errors.IsNotFound(err) {
		if snapshot.RemoveFinalizer(nsSnap, snapshot.FinalizerNamespaceSnapshot) {
			if err := r.Client.Update(ctx, nsSnap); err != nil {
				return ctrl.Result{}, err
			}
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

	if snapshot.RemoveFinalizer(nsSnap, snapshot.FinalizerNamespaceSnapshot) {
		if err := r.Client.Update(ctx, nsSnap); err != nil {
			return ctrl.Result{}, err
		}
	}
	log.FromContext(ctx).V(1).Info("namespace snapshot delete reconcile done")
	return ctrl.Result{}, nil
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
