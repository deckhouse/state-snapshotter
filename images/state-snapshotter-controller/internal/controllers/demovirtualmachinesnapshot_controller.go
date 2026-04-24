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

// DemoVirtualMachineSnapshotReconciler wires a demo VM snapshot into the root NamespaceSnapshot graph (PR5b).
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

func AddDemoVirtualMachineSnapshotControllerToManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&demov1alpha1.DemoVirtualMachineSnapshot{}).
		Complete(&DemoVirtualMachineSnapshotReconciler{Client: mgr.GetClient()})
}

func demoVirtualMachineSnapshotContentName(namespace, name string) string {
	sum := sha256.Sum256([]byte("vm:" + namespace + "/" + name))
	return "demovmc-" + hex.EncodeToString(sum[:10])
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

	parentKey := types.NamespacedName{Namespace: s.Namespace, Name: parentRef.Name}
	parent := &storagev1alpha1.NamespaceSnapshot{}
	if err := r.Client.Get(ctx, parentKey, parent); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
		}
		return ctrl.Result{}, err
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

	wantSnap := []storagev1alpha1.NamespaceSnapshotChildRef{{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       "DemoVirtualMachineSnapshot",
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

	if err := patchDemoVirtualMachineSnapshotReadyStub(ctx, r.Client, req.NamespacedName); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// patchDemoVirtualMachineSnapshotReadyStub sets Ready=True so generic E6 parent aggregation can observe terminal success
// on the registered DemoVirtualMachineSnapshot kind (minimal demo stub; real domains drive this from capture/MCP).
func patchDemoVirtualMachineSnapshotReadyStub(ctx context.Context, c client.Client, vmKey types.NamespacedName) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualMachineSnapshot{}
		if err := c.Get(ctx, vmKey, o); err != nil {
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
			Message:            "demo VM snapshot graph wiring complete (stub)",
			ObservedGeneration: o.Generation,
		})
		if err := c.Status().Patch(ctx, o, client.MergeFrom(stBase)); err != nil {
			return err
		}
		if err := c.Get(ctx, vmKey, o); err != nil {
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

func patchDemoVirtualMachineSnapshotChildRefsMerge(
	ctx context.Context,
	c client.Client,
	parent types.NamespacedName,
	upsert []storagev1alpha1.NamespaceSnapshotChildRef,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualMachineSnapshot{}
		if err := c.Get(ctx, parent, o); err != nil {
			return err
		}
		next := mergeNamespaceSnapshotChildRefs(o.Status.ChildrenSnapshotRefs, upsert)
		if namespaceSnapshotChildRefsEqualIgnoreOrder(next, o.Status.ChildrenSnapshotRefs) {
			return nil
		}
		o.Status.ChildrenSnapshotRefs = next
		return c.Status().Update(ctx, o)
	})
}

func patchDemoVirtualMachineSnapshotContentChildRefsMerge(
	ctx context.Context,
	c client.Client,
	contentName string,
	upsert []storagev1alpha1.NamespaceSnapshotContentChildRef,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		m := &demov1alpha1.DemoVirtualMachineSnapshotContent{}
		if err := c.Get(ctx, client.ObjectKey{Name: contentName}, m); err != nil {
			return err
		}
		next := mergeNamespaceSnapshotContentChildRefs(m.Status.ChildrenSnapshotContentRefs, upsert)
		if namespaceSnapshotContentChildRefsEqualIgnoreOrder(next, m.Status.ChildrenSnapshotContentRefs) {
			return nil
		}
		m.Status.ChildrenSnapshotContentRefs = next
		return c.Status().Update(ctx, m)
	})
}
