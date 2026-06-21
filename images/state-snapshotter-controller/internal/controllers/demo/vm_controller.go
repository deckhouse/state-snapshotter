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
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
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
	// before RBACReady=True is set on CSD.
	if err := registerDemoVMBoundContentFieldIndex(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&demov1alpha1.DemoVirtualMachineSnapshot{}).
		Watches(&demov1alpha1.DemoVirtualDiskSnapshot{}, handler.EnqueueRequestsFromMapFunc(mapDemoDiskSnapshotToParentVM)).
		// SnapshotContent -> bound demo Snapshot wake-up so a content Ready change re-mirrors onto the
		// demo Snapshot (INV-MIRROR); enqueue-only.
		Watches(&storagev1alpha1.SnapshotContent{}, handler.EnqueueRequestsFromMapFunc(mapContentToBoundDemoVMSnapshots(mgr.GetClient()))).
		Complete(&DemoVirtualMachineSnapshotReconciler{
			Client:    mgr.GetClient(),
			APIReader: mgr.GetAPIReader(),
			Config:    cfg,
		})
}

func demoVirtualMachineDiskSnapshotName(namespace, vmSnapshotName, sourceDiskName string) string {
	sum := sha256.Sum256([]byte("vm-disk:" + namespace + "/" + vmSnapshotName + "/" + sourceDiskName))
	return "demovmdisk-" + hex.EncodeToString(sum[:8])
}

func mapDemoDiskSnapshotToParentVM(_ context.Context, o client.Object) []reconcile.Request {
	for _, ref := range o.GetOwnerReferences() {
		if ref.APIVersion == demov1alpha1.SchemeGroupVersion.String() && ref.Kind == controllercommon.KindDemoVirtualMachineSnapshot && ref.Name != "" {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: ref.Name}}}
		}
	}
	return nil
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

	resolution := resolveDemoSnapshotSource(controllercommon.KindDemoVirtualMachine, s.Spec.SourceRef)
	if resolution.Reason != "" {
		if patchErr := patchDemoVirtualMachineSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, resolution.Reason, resolution.Message); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, nil
	}
	sourceName := resolution.Name
	source := &demov1alpha1.DemoVirtualMachine{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: sourceName}, source); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err := patchDemoVirtualMachineSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, demoReasonSourceNotFound, fmt.Sprintf("%s %q not found", controllercommon.KindDemoVirtualMachine, sourceName)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Stale-cache guard (TOCTOU): status.manifestCaptured is set by the common controller via a live write
	// immediately BEFORE it deletes the MCR. Refresh it from a live read before planning so a stale
	// informer cache cannot re-create an MCR the common controller already cleaned up.
	if !s.Status.ManifestCaptured {
		live := &demov1alpha1.DemoVirtualMachineSnapshot{}
		if err := r.APIReader.Get(ctx, req.NamespacedName, live); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
		s.Status.ManifestCaptured = live.Status.ManifestCaptured
	}

	// Children planning: ensure child DemoVirtualDiskSnapshot objects (owned by this VM snapshot) and
	// publish status.childrenSnapshotRefs. The common controller projects these into
	// SnapshotContent.status.childrenSnapshotContentRefs once the children are bound.
	childRefs, err := r.ensureDemoVirtualMachineChildren(ctx, s, source)
	if err != nil {
		if patchErr := patchDemoVirtualMachineSnapshotChildrenSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, snapshot.ReasonCreateChildFailed, err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}
	if err := patchDemoVirtualMachineSnapshotChildrenRefs(ctx, r.Client, req.NamespacedName, childRefs); err != nil {
		if patchErr := patchDemoVirtualMachineSnapshotChildrenSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, snapshot.ReasonGraphPlanningFailed, err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}

	// Manifest leg: ensure the per-snapshot MCR (owned by this VM snapshot; VM is manifest-only, no data
	// leg) and publish its name. Suppressed once status.manifestCaptured is set by the common controller.
	if !s.Status.ManifestCaptured {
		mcr, err := ensureDemoSnapshotManifestCaptureRequest(
			ctx,
			r.Client,
			s.Namespace,
			s.Name,
			controllercommon.KindDemoVirtualMachineSnapshot,
			demov1alpha1.SchemeGroupVersion.String(),
			controllercommon.KindDemoVirtualMachine,
			source.Name,
			demoSnapshotOwnerReference(demov1alpha1.SchemeGroupVersion.String(), controllercommon.KindDemoVirtualMachineSnapshot, s.Name, s.UID),
			"",
		)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := patchDemoVirtualMachineSnapshotManifestCaptureRequestName(ctx, r.Client, req.NamespacedName, mcr.Name); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Planning barrier: children planned/published and MCR created. The common controller waits on this
	// before taking over SnapshotContent (creation, children/MCP projection, Ready mirror).
	if err := patchDemoVirtualMachineSnapshotChildrenSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionTrue, snapshot.ReasonCompleted, "child planning complete"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
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
			Kind:       controllercommon.KindDemoVirtualDiskSnapshot,
			Name:       childName,
		})
	}
	return refs, nil
}

func (r *DemoVirtualMachineSnapshotReconciler) ensureDemoVirtualMachineDiskChild(ctx context.Context, vm *demov1alpha1.DemoVirtualMachineSnapshot, disk *demov1alpha1.DemoVirtualDisk, childName string) error {
	key := types.NamespacedName{Namespace: vm.Namespace, Name: childName}
	child := &demov1alpha1.DemoVirtualDiskSnapshot{}
	// spec.sourceRef is the single source-of-truth for what the child disk snapshot captures; the CRD
	// enforces its immutability, so it is set once at creation and never rewritten.
	desiredSourceRef := demov1alpha1.SnapshotSourceRef{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDisk,
		Name:       disk.Name,
	}
	err := r.Client.Get(ctx, key, child)
	if apierrors.IsNotFound(err) {
		child = &demov1alpha1.DemoVirtualDiskSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      childName,
				Namespace: vm.Namespace,
				OwnerReferences: []metav1.OwnerReference{demoSnapshotOwnerReference(
					demov1alpha1.SchemeGroupVersion.String(),
					controllercommon.KindDemoVirtualMachineSnapshot,
					vm.Name,
					vm.UID,
				)},
			},
			Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
				SourceRef: desiredSourceRef,
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
	desiredOwnerRefs := []metav1.OwnerReference{demoSnapshotOwnerReference(
		demov1alpha1.SchemeGroupVersion.String(),
		controllercommon.KindDemoVirtualMachineSnapshot,
		vm.Name,
		vm.UID,
	)}
	base := child.DeepCopy()
	if err := ensureDemoSnapshotOwnerRef(child, desiredOwnerRefs[0]); err != nil {
		return err
	}
	if controllercommon.OwnerReferencesEqual(child.GetOwnerReferences(), desiredOwnerRefs) {
		return nil
	}
	return r.Client.Patch(ctx, child, client.MergeFrom(base))
}

// demoDiskOwnedByVM resolves the snapshot-tree parent->child link from the VM side:
// DemoVirtualMachine.spec.virtualDiskName names the owned disk (VM -> Disk -> PVC). The disk no longer
// carries a back-reference to the VM, so topology flows strictly downward.
func demoDiskOwnedByVM(disk *demov1alpha1.DemoVirtualDisk, vm *demov1alpha1.DemoVirtualMachine) bool {
	return vm.Spec.VirtualDiskName != "" && vm.Spec.VirtualDiskName == disk.Name
}

func patchDemoVirtualMachineSnapshotChildrenSnapshotReady(
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
		if rc := meta.FindStatusCondition(o.Status.Conditions, snapshot.ConditionChildrenSnapshotReady); rc != nil &&
			rc.Status == status && rc.Reason == reason && rc.Message == message && rc.ObservedGeneration == o.Generation {
			return nil
		}
		base := o.DeepCopy()
		meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionChildrenSnapshotReady,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: o.Generation,
		})
		// D4a: optimistic-lock merge patch so co-writing conditions (core writes Ready) never
		// silently clobbers this owner's entry — a concurrent write yields 409 → RetryOnConflict re-reads.
		return c.Status().Patch(ctx, o, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
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
		if controllercommon.SnapshotChildRefsEqualIgnoreOrder(desired, o.Status.ChildrenSnapshotRefs) {
			return nil
		}
		base := o.DeepCopy()
		o.Status.ChildrenSnapshotRefs = append([]storagev1alpha1.SnapshotChildRef(nil), desired...)
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
		// D4a: optimistic-lock merge patch so this Ready write and the domain ChildrenSnapshotReady
		// writer co-own the same conditions array safely (409 → RetryOnConflict re-reads).
		return c.Status().Patch(ctx, o, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}
