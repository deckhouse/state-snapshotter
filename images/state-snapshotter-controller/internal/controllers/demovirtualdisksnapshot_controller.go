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
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// DemoVirtualDiskSnapshotReconciler owns demo disk sourceRef validation, domain
// MCR creation, snapshot-level Ready, and binding to common SnapshotContent.
// Content status/result aggregation stays in SnapshotContentController.
type DemoVirtualDiskSnapshotReconciler struct {
	Client    client.Client
	APIReader client.Reader
	Config    *config.Options
}

func AddDemoVirtualDiskSnapshotControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
	// RBAC is not generated from kubebuilder markers in this module.
	// Static controller RBAC is defined in templates/controller/rbac-for-us.yaml.
	// Domain/custom RBAC is granted externally by Deckhouse RBAC controller/hook
	// before RBACReady=True is set on DSC.
	return ctrl.NewControllerManagedBy(mgr).
		For(&demov1alpha1.DemoVirtualDiskSnapshot{}).
		Complete(&DemoVirtualDiskSnapshotReconciler{
			Client:    mgr.GetClient(),
			APIReader: mgr.GetAPIReader(),
			Config:    cfg,
		})
}

func demoVirtualDiskSnapshotContentName(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	return "demodiskc-" + hex.EncodeToString(sum[:10])
}

func validateDiskSourceRef(s *demov1alpha1.DemoVirtualDiskSnapshot) (string, error) {
	ref := s.Spec.SourceRef
	if ref.APIVersion != demov1alpha1.SchemeGroupVersion.String() {
		return "", fmt.Errorf("spec.sourceRef.apiVersion must be %q", demov1alpha1.SchemeGroupVersion.String())
	}
	if ref.Kind != KindDemoVirtualDisk {
		return "", fmt.Errorf("spec.sourceRef.kind must be %q", KindDemoVirtualDisk)
	}
	if ref.Name == "" {
		return "", fmt.Errorf("spec.sourceRef.name is required")
	}
	return ref.Name, nil
}

func (r *DemoVirtualDiskSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("demoVirtualDiskSnapshot", req.NamespacedName)
	ctx = log.IntoContext(ctx, logger)

	s := &demov1alpha1.DemoVirtualDiskSnapshot{}
	if err := r.Client.Get(ctx, req.NamespacedName, s); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Deletion is handled by higher-level lifecycle (no finalizers here).
	// This controller is materialization-only.
	if s.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	sourceName, err := validateDiskSourceRef(s)
	if err != nil {
		if patchErr := patchDemoVirtualDiskSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, "InvalidSourceRef", err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, nil
	}
	source := &demov1alpha1.DemoVirtualDisk{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: sourceName}, source); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err := patchDemoVirtualDiskSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, "SourceNotFound", fmt.Sprintf("%s %q not found", KindDemoVirtualDisk, sourceName)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if err := patchDemoVirtualDiskSnapshotGraphReady(ctx, r.Client, req.NamespacedName, metav1.ConditionTrue, snapshot.ReasonCompleted, "leaf snapshot has no child graph"); err != nil {
		return ctrl.Result{}, err
	}

	contentName := demoVirtualDiskSnapshotContentName(s.Namespace, s.Name)
	contentOwnerRef, res, err := r.ensureDemoDiskSnapshotLifecycle(ctx, s)
	if err != nil {
		return ctrl.Result{}, err
	}
	if res.Requeue || res.RequeueAfter > 0 {
		return res, nil
	}
	if err := r.ensureContent(ctx, s, contentName, *contentOwnerRef); err != nil {
		return ctrl.Result{}, err
	}
	if err := patchDemoVirtualDiskSnapshotBound(ctx, r.Client, req.NamespacedName, contentName); err != nil {
		return ctrl.Result{}, err
	}
	if err := publishSnapshotContentLeafChildrenRefs(ctx, r.Client, contentName); err != nil {
		return ctrl.Result{}, err
	}

	mcr, err := ensureDemoSnapshotManifestCaptureRequest(
		ctx,
		r.Client,
		s.Namespace,
		s.Name,
		KindDemoVirtualDiskSnapshot,
		demov1alpha1.SchemeGroupVersion.String(),
		KindDemoVirtualDisk,
		source.Name,
		demoSnapshotOwnerReference(demov1alpha1.SchemeGroupVersion.String(), KindDemoVirtualDiskSnapshot, s.Name, s.UID),
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := patchDemoVirtualDiskSnapshotManifestCaptureRequestName(ctx, r.Client, req.NamespacedName, mcr.Name); err != nil {
		return ctrl.Result{}, err
	}
	if err := publishSnapshotContentManifestCheckpointName(ctx, r.Client, contentName, manifestCheckpointNameFromRequest(mcr)); err != nil {
		return ctrl.Result{}, err
	}
	contentReady, contentReason, contentMessage, err := commonSnapshotContentReadyForSnapshot(ctx, r.Client, contentName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !contentReady {
		if err := patchDemoVirtualDiskSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, contentReason, contentMessage); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
	}
	if err := patchDemoVirtualDiskSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionTrue, snapshot.ReasonCompleted, contentMessage); err != nil {
		return ctrl.Result{}, err
	}
	mcrReady, err := demoSnapshotManifestCaptureRequestReadyForCleanup(ctx, r.Client, client.ObjectKeyFromObject(mcr), contentName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !mcrReady {
		return ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
	}
	if err := cleanupDemoSnapshotManifestCaptureRequest(ctx, r.Client, mcr); err != nil {
		return ctrl.Result{}, err
	}
	if err := patchDemoVirtualDiskSnapshotManifestCaptureRequestName(ctx, r.Client, req.NamespacedName, ""); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func patchDemoVirtualDiskSnapshotGraphReady(
	ctx context.Context,
	c client.Client,
	diskKey types.NamespacedName,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualDiskSnapshot{}
		if err := c.Get(ctx, diskKey, o); err != nil {
			return err
		}
		if rc := meta.FindStatusCondition(o.Status.Conditions, snapshot.ConditionGraphReady); rc != nil &&
			rc.Status == status && rc.Reason == reason && rc.Message == message && rc.ObservedGeneration == o.Generation {
			return nil
		}
		base := o.DeepCopy()
		meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionGraphReady,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: o.Generation,
		})
		return c.Status().Patch(ctx, o, client.MergeFrom(base))
	})
}

func (r *DemoVirtualDiskSnapshotReconciler) ensureDemoDiskSnapshotLifecycle(ctx context.Context, s *demov1alpha1.DemoVirtualDiskSnapshot) (*metav1.OwnerReference, ctrl.Result, error) {
	if parentRef := snapshotParentOwnerRef(s); parentRef != nil {
		contentOwnerRef, pending, err := resolveParentSnapshotContentOwnerRef(ctx, r.Client, s)
		if err != nil {
			return nil, ctrl.Result{}, err
		}
		if pending {
			if err := patchDemoVirtualDiskSnapshotReady(ctx, r.Client, client.ObjectKeyFromObject(s), metav1.ConditionFalse, snapshot.ReasonChildSnapshotPending, fmt.Sprintf("waiting for parent %s/%s bound SnapshotContent", parentRef.Kind, parentRef.Name)); err != nil {
				return nil, ctrl.Result{}, err
			}
			return nil, ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
		}
		return contentOwnerRef, ctrl.Result{}, nil
	}

	ok, res, err := ensureRootObjectKeeperWithTTL(ctx, r.Client, r.APIReader, r.Config, s, demov1alpha1.SchemeGroupVersion.WithKind(KindDemoVirtualDiskSnapshot))
	if err != nil {
		return nil, ctrl.Result{}, err
	}
	if res.Requeue || res.RequeueAfter > 0 {
		return nil, res, nil
	}
	if _, err := ensureLifecycleOwnerRef(ctx, r.Client, s, rootObjectKeeperOwnerReference(ok)); err != nil {
		return nil, ctrl.Result{}, err
	}
	ref := rootObjectKeeperOwnerReference(ok)
	return &ref, ctrl.Result{}, nil
}

func (r *DemoVirtualDiskSnapshotReconciler) ensureContent(ctx context.Context, _ *demov1alpha1.DemoVirtualDiskSnapshot, contentName string, ownerRef metav1.OwnerReference) error {
	existing := &storagev1alpha1.SnapshotContent{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, existing)
	if err == nil {
		_, err := ensureLifecycleOwnerRef(ctx, r.Client, existing, ownerRef)
		return err
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	// Content is cluster-scoped and intentionally retained/managed separately.
	// This controller publishes result refs; SnapshotContentController validates them.
	// We intentionally do not use controllerutil.CreateOrUpdate here.
	// This controller owns only a subset of fields and must avoid
	// accidental overwrites of fields owned by other controllers.
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:            contentName,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: storagev1alpha1.SnapshotContentSpec{},
	}
	return r.Client.Create(ctx, content)
}

func patchDemoVirtualDiskSnapshotBound(
	ctx context.Context,
	c client.Client,
	diskKey types.NamespacedName,
	contentName string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualDiskSnapshot{}
		if err := c.Get(ctx, diskKey, o); err != nil {
			return err
		}
		if o.Status.BoundSnapshotContentName == contentName {
			return nil
		}
		base := o.DeepCopy()
		o.Status.BoundSnapshotContentName = contentName
		return c.Status().Patch(ctx, o, client.MergeFrom(base))
	})
}

func patchDemoVirtualDiskSnapshotManifestCaptureRequestName(
	ctx context.Context,
	c client.Client,
	diskKey types.NamespacedName,
	mcrName string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualDiskSnapshot{}
		if err := c.Get(ctx, diskKey, o); err != nil {
			return err
		}
		if o.Status.ManifestCaptureRequestName == mcrName {
			return nil
		}
		base := o.DeepCopy()
		o.Status.ManifestCaptureRequestName = mcrName
		return c.Status().Patch(ctx, o, client.MergeFrom(base))
	})
}

func patchDemoVirtualDiskSnapshotReady(
	ctx context.Context,
	c client.Client,
	diskKey types.NamespacedName,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualDiskSnapshot{}
		if err := c.Get(ctx, diskKey, o); err != nil {
			return err
		}
		if rc := meta.FindStatusCondition(o.Status.Conditions, snapshot.ConditionReady); rc != nil &&
			rc.Status == status && rc.Reason == reason && rc.Message == message && rc.ObservedGeneration == o.Generation {
			return nil
		}
		base := o.DeepCopy()
		meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionReady,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: o.Generation,
		})
		return c.Status().Patch(ctx, o, client.MergeFrom(base))
	})
}
