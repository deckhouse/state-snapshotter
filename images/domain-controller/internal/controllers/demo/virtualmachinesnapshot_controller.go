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
	"errors"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/domain-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/domain-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

// DemoVirtualMachineSnapshotReconciler owns demo VM DOMAIN planning only: sourceRef validation, the
// owned-disk child snapshot graph, the per-snapshot manifest-capture request (MCR), and the planning
// barrier. All Kubernetes transport (child adoption, owner references, optimistic-locked status patches,
// the barrier condition) is delegated to the snapshot SDK (pkg/snapshotsdk), which is delete-free: it
// publishes the child set as the authoritative snapshot topology and never deletes children. The VM
// snapshot is manifest-only (captures no data); it never touches the cluster-scoped SnapshotContent.
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
	// Content-free for SNAPSHOT reconcilers: NO SnapshotContent watch/informer here. The core
	// GenericSnapshotBinderController owns all SnapshotContent work for this DomainCaptureSnapshotKind.
	// The child DemoVirtualDiskSnapshot watch stays so the parent re-plans when a child changes.
	return ctrl.NewControllerManagedBy(mgr).
		For(&demov1alpha1.DemoVirtualMachineSnapshot{}).
		Watches(&demov1alpha1.DemoVirtualDiskSnapshot{}, handler.EnqueueRequestsFromMapFunc(mapDemoDiskSnapshotToParentVM)).
		Complete(&DemoVirtualMachineSnapshotReconciler{
			Client:    mgr.GetClient(),
			APIReader: mgr.GetAPIReader(),
			Config:    cfg,
		})
}

func (r *DemoVirtualMachineSnapshotReconciler) capture() snapshotsdk.CaptureSDK {
	return snapshotsdk.New(r.Client, snapshotsdk.NewStorageFoundationProvider(r.Client))
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

	// Deletion is handled by higher-level lifecycle (no finalizers here). Materialization-only.
	if s.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// Import mode: spec.source.import switches this VM snapshot off capture. The domain controller does NO
	// capture planning (no source-VM lookup, no children planning, no MCR) — the live DemoVirtualMachine
	// and its disks may be absent on import. The common controller materializes the backing SnapshotContent
	// from the uploaded manifests and child refs. Domain planning is trivially complete for an import node.
	if s.IsImportMode() {
		return ctrl.Result{}, nil
	}

	adapter := demoVirtualMachineSnapshotAdapter{snap: s}
	sdk := r.capture()

	resolution := resolveDemoSnapshotSource(controllercommon.KindDemoVirtualMachine, s.Spec.SourceRef)
	if resolution.Reason != "" {
		return ctrl.Result{}, sdk.MarkNotReady(ctx, adapter, snapshotsdk.NotReadyStatus{Reason: snapshotsdk.Reason(resolution.Reason), Message: resolution.Message})
	}
	source := &demov1alpha1.DemoVirtualMachine{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: resolution.Name}, source); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, sdk.MarkNotReady(ctx, adapter, snapshotsdk.NotReadyStatus{
			Reason:  snapshotsdk.Reason(demoReasonSourceNotFound),
			Message: fmt.Sprintf("%s %q not found", controllercommon.KindDemoVirtualMachine, resolution.Name),
		})
	}

	// Children planning: the domain decides which disks the VM owns and builds the desired child snapshot
	// objects; the SDK adopts them and publishes status.childrenSnapshotRefs (delete-free). The set
	// becomes the authoritative, immutable snapshot topology once the planning barrier is committed
	// (ChildrenSnapshotReady=True): after that a different desired child set (e.g. after a restart with
	// changed discovery) is rejected as terminal topology drift, never repaired by deletion. Detached
	// leftovers are reclaimed by ownerRef GC when this parent is deleted.
	children, err := r.planDemoVirtualMachineChildren(ctx, s, source)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := sdk.EnsureChildren(ctx, adapter, children); err != nil {
		reason := snapshotsdk.Reason(storagev1alpha1.ReasonCreateChildFailed)
		if errors.Is(err, snapshotsdk.ErrTopologyDrift) {
			reason = snapshotsdk.Reason(storagev1alpha1.ReasonTopologyDrift)
		}
		if perr := sdk.MarkPlanningFailed(ctx, adapter, reason, err); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{}, err
	}

	// Manifest capture: ensure the per-snapshot MCR (VM is manifest-only, captures no data) and publish its name.
	if err := sdk.EnsureManifestCapture(ctx, adapter, snapshotsdk.ManifestCaptureSpec{
		Targets: []snapshotsdk.ManifestTarget{{
			APIVersion: demov1alpha1.SchemeGroupVersion.String(),
			Kind:       controllercommon.KindDemoVirtualMachine,
			Name:       source.Name,
		}},
	}); err != nil {
		if errors.Is(err, snapshotsdk.ErrManifestDrift) {
			if perr := sdk.MarkPlanningFailed(ctx, adapter, snapshotsdk.Reason(storagev1alpha1.ReasonManifestDrift), err); perr != nil {
				return ctrl.Result{}, perr
			}
		}
		return ctrl.Result{}, err
	}

	// Planning barrier: children planned/published and MCR created. The common controller waits on this
	// before taking over SnapshotContent (creation, children/MCP projection, Ready mirror).
	return ctrl.Result{}, sdk.MarkPlanningReady(ctx, adapter, "child planning complete")
}

// planDemoVirtualMachineChildren builds the desired set of child DemoVirtualDiskSnapshot objects for the
// disks owned by the VM. Owner references, adoption, and ref derivation are the SDK's job (delete-free;
// the child set becomes the immutable snapshot topology once the planning barrier is committed); the
// domain only authors the child object identity and its immutable spec.sourceRef.
func (r *DemoVirtualMachineSnapshotReconciler) planDemoVirtualMachineChildren(
	ctx context.Context,
	vm *demov1alpha1.DemoVirtualMachineSnapshot,
	source *demov1alpha1.DemoVirtualMachine,
) ([]snapshotsdk.ChildSpec, error) {
	disks := &demov1alpha1.DemoVirtualDiskList{}
	if err := r.Client.List(ctx, disks, client.InNamespace(vm.Namespace)); err != nil {
		return nil, err
	}
	sort.Slice(disks.Items, func(i, j int) bool {
		return disks.Items[i].Name < disks.Items[j].Name
	})

	var children []snapshotsdk.ChildSpec
	for i := range disks.Items {
		disk := &disks.Items[i]
		if !demoDiskOwnedByVM(disk, source) {
			continue
		}
		childName := demoVirtualMachineDiskSnapshotName(vm.Namespace, vm.Name, disk.Name)
		children = append(children, snapshotsdk.ChildSpec{
			Object: &demov1alpha1.DemoVirtualDiskSnapshot{
				ObjectMeta: metav1.ObjectMeta{
					Name:      childName,
					Namespace: vm.Namespace,
				},
				// spec.sourceRef is the single source-of-truth for what the child disk snapshot captures;
				// the CRD enforces its immutability, so it is set once at creation and never rewritten.
				Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
					SourceRef: &demov1alpha1.SnapshotSourceRef{
						APIVersion: demov1alpha1.SchemeGroupVersion.String(),
						Kind:       controllercommon.KindDemoVirtualDisk,
						Name:       disk.Name,
					},
				},
			},
		})
	}
	return children, nil
}

// demoDiskOwnedByVM resolves the snapshot-tree parent->child link from the VM side:
// DemoVirtualMachine.spec.virtualDiskName names the owned disk (VM -> Disk -> PVC). The disk no longer
// carries a back-reference to the VM, so topology flows strictly downward.
func demoDiskOwnedByVM(disk *demov1alpha1.DemoVirtualDisk, vm *demov1alpha1.DemoVirtualMachine) bool {
	return vm.Spec.VirtualDiskName != "" && vm.Spec.VirtualDiskName == disk.Name
}
