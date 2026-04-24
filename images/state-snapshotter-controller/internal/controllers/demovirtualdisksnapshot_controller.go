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
	"time"

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

// DemoVirtualDiskSnapshotReconciler wires a demo disk snapshot into the root NamespaceSnapshot graph
// (merge-only childrenSnapshotRefs / childrenSnapshotContentRefs). PR5a: optional PVC ref in spec (identity only; no CSI).
type DemoVirtualDiskSnapshotReconciler struct {
	Client client.Client
}

// +kubebuilder:rbac:groups=demo.state-snapshotter.deckhouse.io,resources=demovirtualdisksnapshots,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=demo.state-snapshotter.deckhouse.io,resources=demovirtualdisksnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=demo.state-snapshotter.deckhouse.io,resources=demovirtualdisksnapshotcontents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=demo.state-snapshotter.deckhouse.io,resources=demovirtualdisksnapshotcontents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshots,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshotcontents,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.deckhouse.io,resources=namespacesnapshotcontents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=demo.state-snapshotter.deckhouse.io,resources=demovirtualmachinesnapshots,verbs=get;list;watch
// +kubebuilder:rbac:groups=demo.state-snapshotter.deckhouse.io,resources=demovirtualmachinesnapshots/status,verbs=get;update;patch

func AddDemoVirtualDiskSnapshotControllerToManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&demov1alpha1.DemoVirtualDiskSnapshot{}).
		Complete(&DemoVirtualDiskSnapshotReconciler{Client: mgr.GetClient()})
}

func demoVirtualDiskSnapshotContentName(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	return "demodiskc-" + hex.EncodeToString(sum[:10])
}

func resolveDiskParentKey(s *demov1alpha1.DemoVirtualDiskSnapshot) (string, types.NamespacedName, error) {
	ref := s.Spec.ParentSnapshotRef
	if ref.APIVersion == "" {
		return "", types.NamespacedName{}, fmt.Errorf("spec.parentSnapshotRef.apiVersion is required")
	}
	if ref.Kind == "" {
		return "", types.NamespacedName{}, fmt.Errorf("spec.parentSnapshotRef.kind is required")
	}
	if ref.Name == "" {
		return "", types.NamespacedName{}, fmt.Errorf("spec.parentSnapshotRef.name is required")
	}
	switch ref.Kind {
	case "NamespaceSnapshot":
		if ref.APIVersion != storagev1alpha1.SchemeGroupVersion.String() {
			return "", types.NamespacedName{}, fmt.Errorf("spec.parentSnapshotRef.apiVersion %q is not supported for NamespaceSnapshot parent", ref.APIVersion)
		}
	case "DemoVirtualMachineSnapshot":
		if ref.APIVersion != demov1alpha1.SchemeGroupVersion.String() {
			return "", types.NamespacedName{}, fmt.Errorf("spec.parentSnapshotRef.apiVersion %q is not supported for DemoVirtualMachineSnapshot parent", ref.APIVersion)
		}
	default:
		return "", types.NamespacedName{}, fmt.Errorf("spec.parentSnapshotRef.kind %q is not supported", ref.Kind)
	}
	return ref.Kind, types.NamespacedName{Namespace: s.Namespace, Name: ref.Name}, nil
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

	parentKind, parentKey, err := resolveDiskParentKey(s)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch parentKind {
	case "NamespaceSnapshot":
		return r.reconcileUnderNamespaceSnapshot(ctx, s, parentKey)
	case "DemoVirtualMachineSnapshot":
		return r.reconcileUnderParentVM(ctx, s, parentKey)
	default:
		return ctrl.Result{}, fmt.Errorf("unsupported parent kind %q", parentKind)
	}
}

// patchDemoVirtualDiskSnapshotReadyStub sets Ready=True as a minimal demo stub so generic E6 can
// observe success on the registered DemoVirtualDiskSnapshot kind (not product capture/MCP).
func patchDemoVirtualDiskSnapshotReadyStub(ctx context.Context, c client.Client, diskKey types.NamespacedName) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualDiskSnapshot{}
		if err := c.Get(ctx, diskKey, o); err != nil {
			return err
		}
		if o.Status.BoundSnapshotContentName == "" {
			return nil
		}
		if rc := meta.FindStatusCondition(o.Status.Conditions, snapshot.ConditionReady); rc != nil && rc.Status == metav1.ConditionTrue {
			return nil
		}
		// Demo*Snapshot CRDs use status subresource, so status.conditions must be written via Status().Patch/Update.
		stBase := o.DeepCopy()
		meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             snapshot.ReasonCompleted,
			Message:            "demo disk snapshot graph wiring complete (stub)",
			ObservedGeneration: o.Generation,
		})
		if err := c.Status().Patch(ctx, o, client.MergeFrom(stBase)); err != nil {
			return err
		}
		if err := c.Get(ctx, diskKey, o); err != nil {
			return err
		}
		if o.Annotations != nil && o.Annotations["snapshot.deckhouse.io/demo-stub"] == "true" {
			return nil
		}
		metaBase := o.DeepCopy()
		if o.Annotations == nil {
			o.Annotations = map[string]string{}
		}
		o.Annotations["snapshot.deckhouse.io/demo-stub"] = "true"
		return c.Patch(ctx, o, client.MergeFrom(metaBase))
	})
	return err
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

func (r *DemoVirtualDiskSnapshotReconciler) reconcileUnderNamespaceSnapshot(
	ctx context.Context,
	s *demov1alpha1.DemoVirtualDiskSnapshot,
	parentKey types.NamespacedName,
) (ctrl.Result, error) {
	parent := &storagev1alpha1.NamespaceSnapshot{}
	if err := r.Client.Get(ctx, parentKey, parent); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	contentName := demoVirtualDiskSnapshotContentName(s.Namespace, s.Name)
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
	wantSnap := []storagev1alpha1.NamespaceSnapshotChildRef{{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       "DemoVirtualDiskSnapshot",
		Name:       s.Name,
	}}
	if err := patchRootNamespaceSnapshotChildRefsMerge(ctx, r.Client, parentKey, wantSnap); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Client.Get(ctx, parentKey, parent); err != nil {
		return ctrl.Result{}, err
	}
	parentNSC := parent.Status.BoundSnapshotContentName
	if parentNSC == "" {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	wantContent := []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: contentName}}
	if err := patchNamespaceSnapshotContentChildRefsMerge(ctx, r.Client, parentNSC, wantContent); err != nil {
		return ctrl.Result{}, err
	}
	if err := patchDemoVirtualDiskSnapshotReadyStub(ctx, r.Client, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *DemoVirtualDiskSnapshotReconciler) reconcileUnderParentVM(ctx context.Context, s *demov1alpha1.DemoVirtualDiskSnapshot, vmKey types.NamespacedName) (ctrl.Result, error) {
	vm := &demov1alpha1.DemoVirtualMachineSnapshot{}
	if err := r.Client.Get(ctx, vmKey, vm); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	contentName := demoVirtualDiskSnapshotContentName(s.Namespace, s.Name)
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

	wantSnap := []storagev1alpha1.NamespaceSnapshotChildRef{{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       "DemoVirtualDiskSnapshot",
		Name:       s.Name,
	}}
	if err := patchDemoVirtualMachineSnapshotChildRefsMerge(ctx, r.Client, vmKey, wantSnap); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Client.Get(ctx, vmKey, vm); err != nil {
		return ctrl.Result{}, err
	}
	vmNSC := vm.Status.BoundSnapshotContentName
	if vmNSC == "" {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	wantContent := []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: contentName}}
	if err := patchDemoVirtualMachineSnapshotContentChildRefsMerge(ctx, r.Client, vmNSC, wantContent); err != nil {
		return ctrl.Result{}, err
	}

	if err := patchDemoVirtualDiskSnapshotReadyStub(ctx, r.Client, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func patchRootNamespaceSnapshotChildRefsMerge(
	ctx context.Context,
	c client.Client,
	parent types.NamespacedName,
	upsert []storagev1alpha1.NamespaceSnapshotChildRef,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &storagev1alpha1.NamespaceSnapshot{}
		if err := c.Get(ctx, parent, o); err != nil {
			return err
		}
		next := mergeNamespaceSnapshotChildRefs(o.Status.ChildrenSnapshotRefs, upsert)
		if namespaceSnapshotChildRefsEqualIgnoreOrder(next, o.Status.ChildrenSnapshotRefs) {
			return nil
		}
		o.Status.ChildrenSnapshotRefs = next
		o.Status.ObservedGeneration = o.Generation
		return c.Status().Update(ctx, o)
	})
}

func patchNamespaceSnapshotContentChildRefsMerge(
	ctx context.Context,
	c client.Client,
	contentName string,
	upsert []storagev1alpha1.NamespaceSnapshotContentChildRef,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		nsc := &storagev1alpha1.NamespaceSnapshotContent{}
		if err := c.Get(ctx, client.ObjectKey{Name: contentName}, nsc); err != nil {
			return err
		}
		next := mergeNamespaceSnapshotContentChildRefs(nsc.Status.ChildrenSnapshotContentRefs, upsert)
		if namespaceSnapshotContentChildRefsEqualIgnoreOrder(next, nsc.Status.ChildrenSnapshotContentRefs) {
			return nil
		}
		nsc.Status.ChildrenSnapshotContentRefs = next
		return c.Status().Update(ctx, nsc)
	})
}
