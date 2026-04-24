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

func rootNamespaceSnapshotKey(snap *demov1alpha1.DemoVirtualDiskSnapshot) types.NamespacedName {
	ref := snap.Spec.RootNamespaceSnapshotRef
	ns := ref.Namespace
	if ns == "" {
		ns = snap.Namespace
	}
	return types.NamespacedName{Namespace: ns, Name: ref.Name}
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

	ref := s.Spec.RootNamespaceSnapshotRef
	if ref.Name == "" {
		return ctrl.Result{}, fmt.Errorf("spec.rootNamespaceSnapshotRef.name is required")
	}
	if ref.Kind != "" && ref.Kind != "NamespaceSnapshot" {
		return ctrl.Result{}, fmt.Errorf("spec.rootNamespaceSnapshotRef.kind %q is not supported (only NamespaceSnapshot)", ref.Kind)
	}

	if s.Spec.ParentDemoVirtualMachineSnapshotRef != nil && s.Spec.ParentDemoVirtualMachineSnapshotRef.Name != "" {
		return r.reconcileUnderParentVM(ctx, s)
	}

	rootKey := rootNamespaceSnapshotKey(s)
	root := &storagev1alpha1.NamespaceSnapshot{}
	if err := r.Client.Get(ctx, rootKey, root); err != nil {
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
	if err := patchRootNamespaceSnapshotChildRefsMerge(ctx, r.Client, rootKey, wantSnap); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Client.Get(ctx, rootKey, root); err != nil {
		return ctrl.Result{}, err
	}
	rootNSC := root.Status.BoundSnapshotContentName
	if rootNSC == "" {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	wantContent := []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: contentName}}
	if err := patchNamespaceSnapshotContentChildRefsMerge(ctx, r.Client, rootNSC, wantContent); err != nil {
		return ctrl.Result{}, err
	}

	if err := patchDemoVirtualDiskSnapshotReadyStub(ctx, r.Client, req.NamespacedName); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
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

func snapshotSubjectRootRefsMatch(a, b storagev1alpha1.SnapshotSubjectRef, defaultNamespace string) bool {
	nsA, nsB := a.Namespace, b.Namespace
	if nsA == "" {
		nsA = defaultNamespace
	}
	if nsB == "" {
		nsB = defaultNamespace
	}
	return a.Name == b.Name && nsA == nsB
}

func (r *DemoVirtualDiskSnapshotReconciler) reconcileUnderParentVM(ctx context.Context, s *demov1alpha1.DemoVirtualDiskSnapshot) (ctrl.Result, error) {
	pref := s.Spec.ParentDemoVirtualMachineSnapshotRef
	if pref.Kind != "" && pref.Kind != "DemoVirtualMachineSnapshot" {
		return ctrl.Result{}, fmt.Errorf("spec.parentDemoVirtualMachineSnapshotRef.kind %q is not supported (only DemoVirtualMachineSnapshot)", pref.Kind)
	}
	vmNS := pref.Namespace
	if vmNS == "" {
		vmNS = s.Namespace
	}
	vmKey := types.NamespacedName{Namespace: vmNS, Name: pref.Name}
	vm := &demov1alpha1.DemoVirtualMachineSnapshot{}
	if err := r.Client.Get(ctx, vmKey, vm); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	if !snapshotSubjectRootRefsMatch(s.Spec.RootNamespaceSnapshotRef, vm.Spec.RootNamespaceSnapshotRef, s.Namespace) {
		return ctrl.Result{}, fmt.Errorf("spec.rootNamespaceSnapshotRef does not match parent DemoVirtualMachineSnapshot root ref")
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
