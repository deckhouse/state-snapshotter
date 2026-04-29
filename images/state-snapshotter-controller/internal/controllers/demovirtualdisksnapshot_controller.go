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
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// DemoVirtualDiskSnapshotReconciler owns demo disk content, manifest materialization, and Ready.
// Parent graph edges are owned by the parent snapshot controller, not by this child reconciler.
type DemoVirtualDiskSnapshotReconciler struct {
	Client client.Client
}

// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshots,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshotcontents,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshotcontents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=state-snapshotter.deckhouse.io,resources=manifestcapturerequests,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=state-snapshotter.deckhouse.io,resources=manifestcapturerequests/status,verbs=get
// +kubebuilder:rbac:groups=state-snapshotter.deckhouse.io,resources=manifestcheckpoints,verbs=get;list;watch

func AddDemoVirtualDiskSnapshotControllerToManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&demov1alpha1.DemoVirtualDiskSnapshot{}).
		Complete(&DemoVirtualDiskSnapshotReconciler{Client: mgr.GetClient()})
}

func demoVirtualDiskSnapshotContentName(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	return "demodiskc-" + hex.EncodeToString(sum[:10])
}

func validateDiskParentRef(s *demov1alpha1.DemoVirtualDiskSnapshot) error {
	ref := s.Spec.ParentSnapshotRef
	if ref.APIVersion == "" {
		return fmt.Errorf("spec.parentSnapshotRef.apiVersion is required")
	}
	if ref.Kind == "" {
		return fmt.Errorf("spec.parentSnapshotRef.kind is required")
	}
	if ref.Name == "" {
		return fmt.Errorf("spec.parentSnapshotRef.name is required")
	}
	switch ref.Kind {
	case "NamespaceSnapshot":
		if ref.APIVersion != storagev1alpha1.SchemeGroupVersion.String() {
			return fmt.Errorf("spec.parentSnapshotRef.apiVersion %q is not supported for NamespaceSnapshot parent", ref.APIVersion)
		}
	case "DemoVirtualMachineSnapshot":
		if ref.APIVersion != demov1alpha1.SchemeGroupVersion.String() {
			return fmt.Errorf("spec.parentSnapshotRef.apiVersion %q is not supported for DemoVirtualMachineSnapshot parent", ref.APIVersion)
		}
	default:
		return fmt.Errorf("spec.parentSnapshotRef.kind %q is not supported", ref.Kind)
	}
	return nil
}

func validateDiskSourceRef(s *demov1alpha1.DemoVirtualDiskSnapshot) (string, error) {
	ref := s.Spec.SourceRef
	if ref.APIVersion != demov1alpha1.SchemeGroupVersion.String() {
		return "", fmt.Errorf("spec.sourceRef.apiVersion must be %q", demov1alpha1.SchemeGroupVersion.String())
	}
	if ref.Kind != "DemoVirtualDisk" {
		return "", fmt.Errorf("spec.sourceRef.kind must be %q", "DemoVirtualDisk")
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
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if s.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	if err := validateDiskParentRef(s); err != nil {
		return ctrl.Result{}, err
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
		if apierrors.IsNotFound(err) {
			if err := patchDemoVirtualDiskSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, "SourceNotFound", fmt.Sprintf("DemoVirtualDisk %q not found", sourceName)); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	contentName := demoVirtualDiskSnapshotContentName(s.Namespace, s.Name)
	if err := r.ensureSnapshotContent(ctx, s, contentName); err != nil {
		return ctrl.Result{}, err
	}
	if err := patchDemoVirtualDiskSnapshotBound(ctx, r.Client, req.NamespacedName, contentName); err != nil {
		return ctrl.Result{}, err
	}

	mcr, err := ensureDemoSnapshotManifestCaptureRequest(
		ctx,
		r.Client,
		s.Namespace,
		s.Name,
		"DemoVirtualDiskSnapshot",
		demov1alpha1.SchemeGroupVersion.String(),
		"DemoVirtualDisk",
		source.Name,
		demoSnapshotOwnerReference(demov1alpha1.SchemeGroupVersion.String(), "DemoVirtualDiskSnapshot", s.Name, s.UID),
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	mcpName, ready, failed, msg, err := demoManifestCheckpointReady(ctx, r.Client, mcr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if failed {
		if err := patchDemoVirtualDiskSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, "ManifestCheckpointFailed", msg); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if !ready {
		if err := patchDemoVirtualDiskSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, snapshot.ReasonSubtreeManifestCapturePending, msg); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
	}
	if err := patchDemoVirtualDiskSnapshotContentManifestCheckpoint(ctx, r.Client, contentName, mcpName); err != nil {
		return ctrl.Result{}, err
	}
	if err := patchDemoVirtualDiskSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionTrue, snapshot.ReasonCompleted, fmt.Sprintf("demo disk snapshot materialized (ManifestCheckpoint %s)", mcpName)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *DemoVirtualDiskSnapshotReconciler) ensureSnapshotContent(ctx context.Context, snap *demov1alpha1.DemoVirtualDiskSnapshot, contentName string) error {
	existing := &demov1alpha1.DemoVirtualDiskSnapshotContent{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	content := &demov1alpha1.DemoVirtualDiskSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: contentName},
		Spec: demov1alpha1.DemoVirtualDiskSnapshotContentSpec{
			SnapshotRef: storagev1alpha1.SnapshotSubjectRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       "DemoVirtualDiskSnapshot",
				Name:       snap.Name,
				Namespace:  snap.Namespace,
				UID:        snap.UID,
			},
		},
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

func patchDemoVirtualDiskSnapshotContentManifestCheckpoint(
	ctx context.Context,
	c client.Client,
	contentName string,
	mcpName string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		content := &demov1alpha1.DemoVirtualDiskSnapshotContent{}
		if err := c.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
			return err
		}
		if content.Status.ManifestCheckpointName == mcpName {
			return nil
		}
		base := content.DeepCopy()
		content.Status.ManifestCheckpointName = mcpName
		return c.Status().Patch(ctx, content, client.MergeFrom(base))
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
