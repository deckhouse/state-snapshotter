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

package demo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/manifestcapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
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
	// before RBACReady=True is set on CSD.
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

	resolution := resolveDemoSnapshotSource(s.GetAnnotations(), s.Namespace, controllercommon.KindDemoVirtualDisk, s.Spec.SourceRef)
	if resolution.Reason != "" {
		if patchErr := patchDemoVirtualDiskSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, resolution.Reason, resolution.Message); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, nil
	}
	if resolution.DeriveRef != nil {
		if err := patchDemoVirtualDiskSnapshotSourceRef(ctx, r.Client, req.NamespacedName, *resolution.DeriveRef); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	sourceName, sourceUID := resolution.Name, resolution.UID
	source := &demov1alpha1.DemoVirtualDisk{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: sourceName}, source); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err := patchDemoVirtualDiskSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, demoReasonSourceNotFound, fmt.Sprintf("%s %q not found", controllercommon.KindDemoVirtualDisk, sourceName)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if sourceUID != "" && string(source.UID) != sourceUID {
		if err := patchDemoVirtualDiskSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, demoReasonSourceUIDMismatch, fmt.Sprintf("%s %q UID mismatch", controllercommon.KindDemoVirtualDisk, sourceName)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if err := patchDemoVirtualDiskSnapshotDomainReady(ctx, r.Client, req.NamespacedName, metav1.ConditionTrue, snapshot.ReasonCompleted, "leaf snapshot has no children"); err != nil {
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
	if err := ensureDemoSnapshotContent(ctx, r.Client, contentName, *contentOwnerRef); err != nil {
		return ctrl.Result{}, err
	}
	if err := patchDemoVirtualDiskSnapshotBound(ctx, r.Client, req.NamespacedName, contentName); err != nil {
		return ctrl.Result{}, err
	}
	if err := snapshotcontent.PublishSnapshotContentLeafChildrenRefs(ctx, r.Client, contentName); err != nil {
		return ctrl.Result{}, err
	}

	reader := demoReconcilerReader(r.APIReader, r.Client)
	if steady, err := demoReturnIfManifestCaptureSteadyState(
		ctx,
		r.Client,
		reader,
		s.Namespace,
		controllercommon.KindDemoVirtualDiskSnapshot,
		s.Name,
		s.Status.Conditions,
		contentName,
	); err != nil {
		return ctrl.Result{}, err
	} else if steady {
		if s.Status.ManifestCaptureRequestName != "" {
			if err := patchDemoVirtualDiskSnapshotManifestCaptureRequestName(ctx, r.Client, req.NamespacedName, ""); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	mcr, err := ensureDemoSnapshotManifestCaptureRequest(
		ctx,
		r.Client,
		s.Namespace,
		s.Name,
		controllercommon.KindDemoVirtualDiskSnapshot,
		demov1alpha1.SchemeGroupVersion.String(),
		controllercommon.KindDemoVirtualDisk,
		source.Name,
		demoSnapshotOwnerReference(demov1alpha1.SchemeGroupVersion.String(), controllercommon.KindDemoVirtualDiskSnapshot, s.Name, s.UID),
		contentName,
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	if mcr == nil {
		return ctrl.Result{}, nil
	}
	if err := patchDemoVirtualDiskSnapshotManifestCaptureRequestName(ctx, r.Client, req.NamespacedName, mcr.Name); err != nil {
		return ctrl.Result{}, err
	}
	if err := snapshotcontent.PublishSnapshotContentManifestCheckpointName(ctx, r.Client, contentName, manifestcapture.ManifestCheckpointNameFromRequest(mcr)); err != nil {
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

func patchDemoVirtualDiskSnapshotDomainReady(
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
		if rc := meta.FindStatusCondition(o.Status.Conditions, snapshot.ConditionDomainReady); rc != nil &&
			rc.Status == status && rc.Reason == reason && rc.Message == message && rc.ObservedGeneration == o.Generation {
			return nil
		}
		base := o.DeepCopy()
		meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionDomainReady,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: o.Generation,
		})
		return c.Status().Patch(ctx, o, client.MergeFrom(base))
	})
}

func (r *DemoVirtualDiskSnapshotReconciler) ensureDemoDiskSnapshotLifecycle(ctx context.Context, s *demov1alpha1.DemoVirtualDiskSnapshot) (*metav1.OwnerReference, ctrl.Result, error) {
	if parentRef := controllercommon.SnapshotParentOwnerRef(s); parentRef != nil {
		contentOwnerRef, pending, err := controllercommon.ResolveParentSnapshotContentOwnerRef(ctx, r.Client, s)
		if err != nil {
			return nil, ctrl.Result{}, err
		}
		if pending {
			if err := patchDemoVirtualDiskSnapshotReady(ctx, r.Client, client.ObjectKeyFromObject(s), metav1.ConditionFalse, snapshot.ReasonChildrenPending, fmt.Sprintf("waiting for parent %s/%s bound SnapshotContent", parentRef.Kind, parentRef.Name)); err != nil {
				return nil, ctrl.Result{}, err
			}
			return nil, ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
		}
		return contentOwnerRef, ctrl.Result{}, nil
	}

	ok, res, err := controllercommon.EnsureRootObjectKeeperWithTTL(ctx, r.Client, r.APIReader, r.Config, s, demov1alpha1.SchemeGroupVersion.WithKind(controllercommon.KindDemoVirtualDiskSnapshot))
	if err != nil {
		return nil, ctrl.Result{}, err
	}
	if res.Requeue || res.RequeueAfter > 0 {
		return nil, res, nil
	}
	ref := controllercommon.RootObjectKeeperOwnerReference(ok)
	return &ref, ctrl.Result{}, nil
}

// patchDemoVirtualDiskSnapshotSourceRef one-shot fills spec.sourceRef derived from the
// generic source identity annotation. spec.sourceRef is demo/manual API-compat only;
// generic tree coverage uses AnnotationKeySourceRef, never this field.
func patchDemoVirtualDiskSnapshotSourceRef(
	ctx context.Context,
	c client.Client,
	diskKey types.NamespacedName,
	ref demov1alpha1.SnapshotSourceRef,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualDiskSnapshot{}
		if err := c.Get(ctx, diskKey, o); err != nil {
			return err
		}
		if o.Spec.SourceRef == ref {
			return nil
		}
		if !demoSourceRefEmpty(o.Spec.SourceRef) {
			return nil
		}
		base := o.DeepCopy()
		o.Spec.SourceRef = ref
		return c.Patch(ctx, o, client.MergeFrom(base))
	})
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
