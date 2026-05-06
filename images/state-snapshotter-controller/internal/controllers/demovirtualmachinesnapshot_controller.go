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

package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// DemoVirtualMachineSnapshotReconciler owns demo VM sourceRef validation,
// domain MCR creation, parent-owned disk child graph, snapshot-level Ready,
// and binding to common SnapshotContent. Content status/result aggregation
// stays in SnapshotContentController.
type DemoVirtualMachineSnapshotReconciler struct {
	Client    client.Client
	APIReader client.Reader
	Config    *config.Options
}

func AddDemoVirtualMachineSnapshotControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
	// RBAC is not generated from kubebuilder markers in this module.
	// Static controller RBAC is defined in templates/controller/rbac-for-us.yaml.
	// Domain/custom RBAC is granted externally by Deckhouse RBAC controller/hook
	// before RBACReady=True is set on DSC.
	return ctrl.NewControllerManagedBy(mgr).
		For(&demov1alpha1.DemoVirtualMachineSnapshot{}).
		Watches(&demov1alpha1.DemoVirtualDiskSnapshot{}, handler.EnqueueRequestsFromMapFunc(mapDemoDiskSnapshotToParentVM)).
		Complete(&DemoVirtualMachineSnapshotReconciler{
			Client:    mgr.GetClient(),
			APIReader: mgr.GetAPIReader(),
			Config:    cfg,
		})
}

func demoVirtualMachineSnapshotContentName(namespace, name string) string {
	sum := sha256.Sum256([]byte("vm:" + namespace + "/" + name))
	return "demovmc-" + hex.EncodeToString(sum[:10])
}

func demoVirtualMachineDiskSnapshotName(namespace, vmSnapshotName, sourceDiskName string) string {
	sum := sha256.Sum256([]byte("vm-disk:" + namespace + "/" + vmSnapshotName + "/" + sourceDiskName))
	return "demovmdisk-" + hex.EncodeToString(sum[:8])
}

func mapDemoDiskSnapshotToParentVM(_ context.Context, o client.Object) []reconcile.Request {
	for _, ref := range o.GetOwnerReferences() {
		if ref.APIVersion == demov1alpha1.SchemeGroupVersion.String() && ref.Kind == KindDemoVirtualMachineSnapshot && ref.Name != "" {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: ref.Name}}}
		}
	}
	return nil
}

func validateVMSourceRef(s *demov1alpha1.DemoVirtualMachineSnapshot) (string, error) {
	ref := s.Spec.SourceRef
	if ref.APIVersion != demov1alpha1.SchemeGroupVersion.String() {
		return "", fmt.Errorf("spec.sourceRef.apiVersion must be %q", demov1alpha1.SchemeGroupVersion.String())
	}
	if ref.Kind != KindDemoVirtualMachine {
		return "", fmt.Errorf("spec.sourceRef.kind must be %q", KindDemoVirtualMachine)
	}
	if ref.Name == "" {
		return "", fmt.Errorf("spec.sourceRef.name is required")
	}
	return ref.Name, nil
}

func (r *DemoVirtualMachineSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("demoVirtualMachineSnapshot", req.NamespacedName)
	ctx = log.IntoContext(ctx, logger)

	s := &demov1alpha1.DemoVirtualMachineSnapshot{}
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

	sourceName, err := validateVMSourceRef(s)
	if err != nil {
		if patchErr := patchDemoVirtualMachineSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, "InvalidSourceRef", err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, nil
	}
	source := &demov1alpha1.DemoVirtualMachine{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: sourceName}, source); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err := patchDemoVirtualMachineSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, "SourceNotFound", fmt.Sprintf("%s %q not found", KindDemoVirtualMachine, sourceName)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	contentName := demoVirtualMachineSnapshotContentName(s.Namespace, s.Name)
	contentOwnerRef, res, err := r.ensureDemoVMSnapshotLifecycle(ctx, s)
	if err != nil {
		return ctrl.Result{}, err
	}
	if res.Requeue || res.RequeueAfter > 0 {
		return res, nil
	}
	if err := ensureDemoSnapshotContent(ctx, r.Client, contentName, *contentOwnerRef); err != nil {
		return ctrl.Result{}, err
	}

	if err := patchDemoVirtualMachineSnapshotBound(ctx, r.Client, req.NamespacedName, contentName); err != nil {
		return ctrl.Result{}, err
	}

	mcr, err := ensureDemoSnapshotManifestCaptureRequest(
		ctx,
		r.Client,
		s.Namespace,
		s.Name,
		KindDemoVirtualMachineSnapshot,
		demov1alpha1.SchemeGroupVersion.String(),
		KindDemoVirtualMachine,
		source.Name,
		demoSnapshotOwnerReference(demov1alpha1.SchemeGroupVersion.String(), KindDemoVirtualMachineSnapshot, s.Name, s.UID),
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := patchDemoVirtualMachineSnapshotManifestCaptureRequestName(ctx, r.Client, req.NamespacedName, mcr.Name); err != nil {
		return ctrl.Result{}, err
	}
	childRefs, err := r.ensureDemoVirtualMachineChildren(ctx, s, source)
	if err != nil {
		if patchErr := patchDemoVirtualMachineSnapshotGraphReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, snapshot.ReasonCreateChildFailed, err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}
	if err := patchDemoVirtualMachineSnapshotChildrenRefs(ctx, r.Client, req.NamespacedName, childRefs); err != nil {
		if patchErr := patchDemoVirtualMachineSnapshotGraphReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, snapshot.ReasonGraphPlanningFailed, err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}
	if err := patchDemoVirtualMachineSnapshotGraphReady(ctx, r.Client, req.NamespacedName, metav1.ConditionTrue, snapshot.ReasonCompleted, "child graph planned"); err != nil {
		return ctrl.Result{}, err
	}
	graphPublished, err := publishSnapshotContentChildrenFromSnapshotRefs(ctx, r.Client, s.Namespace, contentName, childRefs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !graphPublished {
		if err := patchDemoVirtualMachineSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, snapshot.ReasonChildSnapshotPending, "waiting for child content objects to bind"); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
	}
	if err := publishSnapshotContentManifestCheckpointName(ctx, r.Client, contentName, manifestCheckpointNameFromRequest(mcr)); err != nil {
		return ctrl.Result{}, err
	}

	contentReady, contentReason, contentMessage, err := commonSnapshotContentReadyForSnapshot(ctx, r.Client, contentName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !contentReady {
		if err := patchDemoVirtualMachineSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, contentReason, contentMessage); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
	}
	if err := patchDemoVirtualMachineSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionTrue, snapshot.ReasonCompleted, contentMessage); err != nil {
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
	if err := patchDemoVirtualMachineSnapshotManifestCaptureRequestName(ctx, r.Client, req.NamespacedName, ""); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *DemoVirtualMachineSnapshotReconciler) ensureDemoVMSnapshotLifecycle(ctx context.Context, s *demov1alpha1.DemoVirtualMachineSnapshot) (*metav1.OwnerReference, ctrl.Result, error) {
	if parentRef := snapshotParentOwnerRef(s); parentRef != nil {
		contentOwnerRef, pending, err := resolveParentSnapshotContentOwnerRef(ctx, r.Client, s)
		if err != nil {
			return nil, ctrl.Result{}, err
		}
		if pending {
			if err := patchDemoVirtualMachineSnapshotReady(ctx, r.Client, client.ObjectKeyFromObject(s), metav1.ConditionFalse, snapshot.ReasonChildSnapshotPending, fmt.Sprintf("waiting for parent %s/%s bound SnapshotContent", parentRef.Kind, parentRef.Name)); err != nil {
				return nil, ctrl.Result{}, err
			}
			return nil, ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
		}
		return contentOwnerRef, ctrl.Result{}, nil
	}

	ok, res, err := ensureRootObjectKeeperWithTTL(ctx, r.Client, r.APIReader, r.Config, s, demov1alpha1.SchemeGroupVersion.WithKind(KindDemoVirtualMachineSnapshot))
	if err != nil {
		return nil, ctrl.Result{}, err
	}
	if res.Requeue || res.RequeueAfter > 0 {
		return nil, res, nil
	}
	ref := rootObjectKeeperOwnerReference(ok)
	return &ref, ctrl.Result{}, nil
}

func (r *DemoVirtualMachineSnapshotReconciler) ensureDemoVirtualMachineChildren(ctx context.Context, vm *demov1alpha1.DemoVirtualMachineSnapshot, source *demov1alpha1.DemoVirtualMachine) ([]storagev1alpha1.SnapshotChildRef, error) {
	disks := &demov1alpha1.DemoVirtualDiskList{}
	if err := r.Client.List(ctx, disks, client.InNamespace(vm.Namespace)); err != nil {
		return nil, err
	}
	sort.Slice(disks.Items, func(i, j int) bool {
		return disks.Items[i].Name < disks.Items[j].Name
	})

	var refs []storagev1alpha1.SnapshotChildRef
	for i := range disks.Items {
		disk := &disks.Items[i]
		if !demoDiskOwnedByVM(disk, source) {
			continue
		}
		childName := demoVirtualMachineDiskSnapshotName(vm.Namespace, vm.Name, disk.Name)
		if err := r.ensureDemoVirtualMachineDiskChild(ctx, vm, disk, childName); err != nil {
			return nil, err
		}
		refs = append(refs, storagev1alpha1.SnapshotChildRef{
			APIVersion: demov1alpha1.SchemeGroupVersion.String(),
			Kind:       KindDemoVirtualDiskSnapshot,
			Name:       childName,
		})
	}
	return refs, nil
}

func (r *DemoVirtualMachineSnapshotReconciler) ensureDemoVirtualMachineDiskChild(ctx context.Context, vm *demov1alpha1.DemoVirtualMachineSnapshot, disk *demov1alpha1.DemoVirtualDisk, childName string) error {
	key := types.NamespacedName{Namespace: vm.Namespace, Name: childName}
	child := &demov1alpha1.DemoVirtualDiskSnapshot{}
	err := r.Client.Get(ctx, key, child)
	if apierrors.IsNotFound(err) {
		child = &demov1alpha1.DemoVirtualDiskSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      childName,
				Namespace: vm.Namespace,
				OwnerReferences: []metav1.OwnerReference{demoSnapshotOwnerReference(
					demov1alpha1.SchemeGroupVersion.String(),
					KindDemoVirtualMachineSnapshot,
					vm.Name,
					vm.UID,
				)},
			},
			Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
				SourceRef: demov1alpha1.SnapshotSourceRef{
					APIVersion: demov1alpha1.SchemeGroupVersion.String(),
					Kind:       KindDemoVirtualDisk,
					Name:       disk.Name,
				},
			},
		}
		if err := r.Client.Create(ctx, child); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	desiredSourceRef := demov1alpha1.SnapshotSourceRef{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       KindDemoVirtualDisk,
		Name:       disk.Name,
	}
	desiredOwnerRefs := []metav1.OwnerReference{demoSnapshotOwnerReference(
		demov1alpha1.SchemeGroupVersion.String(),
		KindDemoVirtualMachineSnapshot,
		vm.Name,
		vm.UID,
	)}
	base := child.DeepCopy()
	if err := ensureDemoSnapshotOwnerRef(child, desiredOwnerRefs[0]); err != nil {
		return err
	}
	if child.Spec.SourceRef == desiredSourceRef && ownerReferencesEqual(child.GetOwnerReferences(), desiredOwnerRefs) {
		return nil
	}
	child.Spec.SourceRef = desiredSourceRef
	return r.Client.Patch(ctx, child, client.MergeFrom(base))
}

func demoDiskOwnedByVM(disk *demov1alpha1.DemoVirtualDisk, vm *demov1alpha1.DemoVirtualMachine) bool {
	for _, ref := range disk.OwnerReferences {
		if ref.APIVersion != demov1alpha1.SchemeGroupVersion.String() || ref.Kind != KindDemoVirtualMachine || ref.Name != vm.Name {
			continue
		}
		return ref.UID == "" || ref.UID == vm.UID
	}
	return false
}

func patchDemoVirtualMachineSnapshotGraphReady(
	ctx context.Context,
	c client.Client,
	vmKey types.NamespacedName,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualMachineSnapshot{}
		if err := c.Get(ctx, vmKey, o); err != nil {
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

func patchDemoVirtualMachineSnapshotChildrenRefs(
	ctx context.Context,
	c client.Client,
	parent types.NamespacedName,
	desired []storagev1alpha1.SnapshotChildRef,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualMachineSnapshot{}
		if err := c.Get(ctx, parent, o); err != nil {
			return err
		}
		if snapshotChildRefsEqualIgnoreOrder(desired, o.Status.ChildrenSnapshotRefs) {
			return nil
		}
		base := o.DeepCopy()
		o.Status.ChildrenSnapshotRefs = append([]storagev1alpha1.SnapshotChildRef(nil), desired...)
		return c.Status().Patch(ctx, o, client.MergeFrom(base))
	})
}

func patchDemoVirtualMachineSnapshotBound(
	ctx context.Context,
	c client.Client,
	vmKey types.NamespacedName,
	contentName string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualMachineSnapshot{}
		if err := c.Get(ctx, vmKey, o); err != nil {
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

func patchDemoVirtualMachineSnapshotManifestCaptureRequestName(
	ctx context.Context,
	c client.Client,
	vmKey types.NamespacedName,
	mcrName string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualMachineSnapshot{}
		if err := c.Get(ctx, vmKey, o); err != nil {
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

func patchDemoVirtualMachineSnapshotReady(
	ctx context.Context,
	c client.Client,
	vmKey types.NamespacedName,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualMachineSnapshot{}
		if err := c.Get(ctx, vmKey, o); err != nil {
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
