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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// DemoVirtualMachineSnapshotReconciler owns demo VM materialization and its parent-owned disk child graph.
type DemoVirtualMachineSnapshotReconciler struct {
	Client client.Client
}

// +kubebuilder:rbac:groups=demo.state-snapshotter.deckhouse.io,resources=demovirtualmachinesnapshots,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=demo.state-snapshotter.deckhouse.io,resources=demovirtualmachinesnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=demo.state-snapshotter.deckhouse.io,resources=demovirtualmachinesnapshotcontents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=demo.state-snapshotter.deckhouse.io,resources=demovirtualmachinesnapshotcontents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshots,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshotcontents,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshotcontents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=demo.state-snapshotter.deckhouse.io,resources=demovirtualdisksnapshots,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=demo.state-snapshotter.deckhouse.io,resources=demovirtualdisksnapshots/status,verbs=get
// +kubebuilder:rbac:groups=state-snapshotter.deckhouse.io,resources=manifestcapturerequests,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=state-snapshotter.deckhouse.io,resources=manifestcapturerequests/status,verbs=get
// +kubebuilder:rbac:groups=state-snapshotter.deckhouse.io,resources=manifestcheckpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch

func AddDemoVirtualMachineSnapshotControllerToManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&demov1alpha1.DemoVirtualMachineSnapshot{}).
		Watches(&demov1alpha1.DemoVirtualDiskSnapshot{}, handler.EnqueueRequestsFromMapFunc(mapDemoDiskSnapshotToParentVM)).
		Complete(&DemoVirtualMachineSnapshotReconciler{Client: mgr.GetClient()})
}

func demoVirtualMachineSnapshotContentName(namespace, name string) string {
	sum := sha256.Sum256([]byte("vm:" + namespace + "/" + name))
	return "demovmc-" + hex.EncodeToString(sum[:10])
}

func demoVirtualMachineDiskSnapshotName(namespace, name string) string {
	sum := sha256.Sum256([]byte("vm-disk:" + namespace + "/" + name))
	return "demovmdisk-" + hex.EncodeToString(sum[:8])
}

func mapDemoDiskSnapshotToParentVM(_ context.Context, o client.Object) []reconcile.Request {
	disk, ok := o.(*demov1alpha1.DemoVirtualDiskSnapshot)
	if !ok {
		return nil
	}
	ref := disk.Spec.ParentSnapshotRef
	if ref.APIVersion != demov1alpha1.SchemeGroupVersion.String() || ref.Kind != "DemoVirtualMachineSnapshot" || ref.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: disk.Namespace, Name: ref.Name}}}
}

func (r *DemoVirtualMachineSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("demoVirtualMachineSnapshot", req.NamespacedName)
	ctx = log.IntoContext(ctx, logger)

	s := &demov1alpha1.DemoVirtualMachineSnapshot{}
	if err := r.Client.Get(ctx, req.NamespacedName, s); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if s.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	parentRef := s.Spec.ParentSnapshotRef
	if parentRef.APIVersion == "" {
		return ctrl.Result{}, fmt.Errorf("spec.parentSnapshotRef.apiVersion is required")
	}
	if parentRef.Kind == "" {
		return ctrl.Result{}, fmt.Errorf("spec.parentSnapshotRef.kind is required")
	}
	if parentRef.Name == "" {
		return ctrl.Result{}, fmt.Errorf("spec.parentSnapshotRef.name is required")
	}
	if parentRef.Kind != "NamespaceSnapshot" {
		return ctrl.Result{}, fmt.Errorf("spec.parentSnapshotRef.kind %q is not supported (only NamespaceSnapshot)", parentRef.Kind)
	}
	if parentRef.APIVersion != storagev1alpha1.SchemeGroupVersion.String() {
		return ctrl.Result{}, fmt.Errorf("spec.parentSnapshotRef.apiVersion %q is not supported for NamespaceSnapshot parent", parentRef.APIVersion)
	}

	contentName := demoVirtualMachineSnapshotContentName(s.Namespace, s.Name)
	if err := r.ensureSnapshotContent(ctx, s, contentName); err != nil {
		return ctrl.Result{}, err
	}

	if s.Status.BoundSnapshotContentName != contentName {
		base := s.DeepCopy()
		s.Status.BoundSnapshotContentName = contentName
		if err := r.Client.Status().Patch(ctx, s, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
	}

	mcr, err := ensureDemoSnapshotManifestCaptureRequest(
		ctx,
		r.Client,
		s.Namespace,
		s.Name,
		"DemoVirtualMachineSnapshot",
		demoSnapshotOwnerReference(demov1alpha1.SchemeGroupVersion.String(), "DemoVirtualMachineSnapshot", s.Name, s.UID),
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	mcpName, ready, failed, msg, err := demoManifestCheckpointReady(ctx, r.Client, mcr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if failed {
		if err := patchDemoVirtualMachineSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, "ManifestCheckpointFailed", msg); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if !ready {
		if err := patchDemoVirtualMachineSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, snapshot.ReasonSubtreeManifestCapturePending, msg); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
	}
	if err := patchDemoVirtualMachineSnapshotContentManifestCheckpoint(ctx, r.Client, contentName, mcpName); err != nil {
		return ctrl.Result{}, err
	}

	childRefs, err := r.ensureDemoVirtualMachineChildren(ctx, s)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := patchDemoVirtualMachineSnapshotChildrenRefs(ctx, r.Client, req.NamespacedName, childRefs); err != nil {
		return ctrl.Result{}, err
	}
	if err := patchDemoVirtualMachineSnapshotContentChildrenFromRefs(ctx, r.Client, contentName, s.Namespace, childRefs); err != nil {
		return ctrl.Result{}, err
	}

	sum, err := usecase.SummarizeChildrenSnapshotRefsForParentReadyE6(ctx, r.Client, childRefs, s.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if sum.HasFailed {
		if err := patchDemoVirtualMachineSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, snapshot.ReasonChildSnapshotFailed, usecase.JoinNonEmpty(sum.FailedMessages, "; ")); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if sum.HasPending {
		if err := patchDemoVirtualMachineSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionFalse, snapshot.ReasonChildSnapshotPending, usecase.JoinNonEmpty(sum.PendingParts, "; ")); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
	}
	if err := patchDemoVirtualMachineSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionTrue, snapshot.ReasonCompleted, fmt.Sprintf("demo VM snapshot materialized (ManifestCheckpoint %s) and all child snapshots are ready", mcpName)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *DemoVirtualMachineSnapshotReconciler) ensureSnapshotContent(ctx context.Context, snap *demov1alpha1.DemoVirtualMachineSnapshot, contentName string) error {
	existing := &demov1alpha1.DemoVirtualMachineSnapshotContent{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	content := &demov1alpha1.DemoVirtualMachineSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: contentName},
		Spec: demov1alpha1.DemoVirtualMachineSnapshotContentSpec{
			SnapshotRef: storagev1alpha1.SnapshotSubjectRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       "DemoVirtualMachineSnapshot",
				Name:       snap.Name,
				Namespace:  snap.Namespace,
				UID:        snap.UID,
			},
		},
	}
	return r.Client.Create(ctx, content)
}

func (r *DemoVirtualMachineSnapshotReconciler) ensureDemoVirtualMachineChildren(ctx context.Context, vm *demov1alpha1.DemoVirtualMachineSnapshot) ([]storagev1alpha1.NamespaceSnapshotChildRef, error) {
	childName := demoVirtualMachineDiskSnapshotName(vm.Namespace, vm.Name)
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
					"DemoVirtualMachineSnapshot",
					vm.Name,
					vm.UID,
				)},
			},
			Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
				ParentSnapshotRef: demov1alpha1.SnapshotParentRef{
					APIVersion: demov1alpha1.SchemeGroupVersion.String(),
					Kind:       "DemoVirtualMachineSnapshot",
					Name:       vm.Name,
				},
			},
		}
		if err := r.Client.Create(ctx, child); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return []storagev1alpha1.NamespaceSnapshotChildRef{{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       "DemoVirtualDiskSnapshot",
		Name:       childName,
	}}, nil
}

func patchDemoVirtualMachineSnapshotChildrenRefs(
	ctx context.Context,
	c client.Client,
	parent types.NamespacedName,
	desired []storagev1alpha1.NamespaceSnapshotChildRef,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualMachineSnapshot{}
		if err := c.Get(ctx, parent, o); err != nil {
			return err
		}
		if namespaceSnapshotChildRefsEqualIgnoreOrder(desired, o.Status.ChildrenSnapshotRefs) {
			return nil
		}
		base := o.DeepCopy()
		o.Status.ChildrenSnapshotRefs = append([]storagev1alpha1.NamespaceSnapshotChildRef(nil), desired...)
		return c.Status().Patch(ctx, o, client.MergeFrom(base))
	})
}

func patchDemoVirtualMachineSnapshotContentChildrenFromRefs(
	ctx context.Context,
	c client.Client,
	contentName string,
	parentNamespace string,
	refs []storagev1alpha1.NamespaceSnapshotChildRef,
) error {
	var desired []storagev1alpha1.NamespaceSnapshotContentChildRef
	for _, ref := range refs {
		if ref.APIVersion != demov1alpha1.SchemeGroupVersion.String() || ref.Kind != "DemoVirtualDiskSnapshot" {
			continue
		}
		disk := &demov1alpha1.DemoVirtualDiskSnapshot{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: parentNamespace, Name: ref.Name}, disk); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if disk.Status.BoundSnapshotContentName != "" {
			desired = append(desired, storagev1alpha1.NamespaceSnapshotContentChildRef{Name: disk.Status.BoundSnapshotContentName})
		}
	}
	sortNamespaceSnapshotContentChildRefs(desired)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		m := &demov1alpha1.DemoVirtualMachineSnapshotContent{}
		if err := c.Get(ctx, client.ObjectKey{Name: contentName}, m); err != nil {
			return err
		}
		if namespaceSnapshotContentChildRefsEqualIgnoreOrder(desired, m.Status.ChildrenSnapshotContentRefs) {
			return nil
		}
		base := m.DeepCopy()
		m.Status.ChildrenSnapshotContentRefs = append([]storagev1alpha1.NamespaceSnapshotContentChildRef(nil), desired...)
		return c.Status().Patch(ctx, m, client.MergeFrom(base))
	})
}

func patchDemoVirtualMachineSnapshotContentManifestCheckpoint(
	ctx context.Context,
	c client.Client,
	contentName string,
	mcpName string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		content := &demov1alpha1.DemoVirtualMachineSnapshotContent{}
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
