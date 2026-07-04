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
// barrier. All Kubernetes transport (child adoption, owner references, orphan GC, optimistic-locked status
// patches, the barrier condition) is delegated to the snapshot SDK (pkg/snapshotsdk). The VM snapshot is
// manifest-only (no data leg); it never touches the cluster-scoped SnapshotContent.
type DemoVirtualMachineSnapshotReconciler struct {
	Client    client.Client
	APIReader client.Reader
	Config    *config.Options
}

func AddDemoVirtualMachineSnapshotControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
	// RBAC is not generated from kubebuilder markers in this module.
	// Static controller RBAC is defined in templates/controller/rbac-for-us.yaml.
	// Domain/custom RBAC is granted externally by Deckhouse RBAC controller/hook
	// before AccessGranted=True is set on CSD.
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
	return snapshotsdk.New(r.Client, r.APIReader, snapshotsdk.NewStorageFoundationProvider(r.Client))
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

	// StaticBind mode (recycle-bin restore, wave4B): this VM snapshot binds to a pre-provisioned, surviving
	// SnapshotContent (spec.source.snapshotContentName) instead of capturing. The domain controller does NO
	// capture planning (no source-VM lookup, no children planning, no MCR); the core validates the
	// back-binding and mirrors Ready/excludedRefs from the existing content, and re-creates the surviving
	// child subtree as StaticBind. Domain planning is trivially complete for a static-bind node.
	if s.IsStaticBind() {
		return ctrl.Result{}, nil
	}

	adapter := demoVirtualMachineSnapshotAdapter{snap: s}
	sdk := r.capture()

	resolution := resolveDemoSnapshotSource(controllercommon.KindDemoVirtualMachine, s.Spec.SourceRef)
	if resolution.Reason != "" {
		return ctrl.Result{}, sdk.Reject(ctx, adapter, snapshotsdk.FailSpec{Reason: snapshotsdk.Reason(resolution.Reason), Message: resolution.Message})
	}
	source := &demov1alpha1.DemoVirtualMachine{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: resolution.Name}, source); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, sdk.Reject(ctx, adapter, snapshotsdk.FailSpec{
			Reason:  snapshotsdk.Reason(demoReasonSourceNotFound),
			Message: fmt.Sprintf("%s %q not found", controllercommon.KindDemoVirtualMachine, resolution.Name),
		})
	}

	// Publish the captured live source's full reference (top-level status.snapshotSource). d8-cli reads it
	// as a self-contained block to rebuild the import-mode source. Not part of the readiness formula.
	if err := sdk.PublishSnapshotSource(ctx, adapter, snapshotsdk.SnapshotSource{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualMachine,
		Name:       source.Name,
		Namespace:  source.Namespace,
		UID:        source.UID,
	}); err != nil {
		return ctrl.Result{}, err
	}

	// Children planning: the domain decides which disks the VM owns, honors the absolute exclude veto, and
	// builds the desired child snapshot objects from the kept disks; the SDK adopts them and publishes
	// status.childrenSnapshotRefs. The vetoed disks are handed back as excludedRefs (direct exclusions) and
	// published into captureState.domainSpecificController.excludedRefs in the same status patch.
	children, excluded, err := r.planDemoVirtualMachineChildren(ctx, s, source)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := sdk.EnsureChildren(ctx, adapter, children, excluded); err != nil {
		if perr := sdk.Fail(ctx, adapter, snapshotsdk.Reason(storagev1alpha1.ReasonCreateChildFailed), err); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{}, err
	}

	// Manifest leg: ensure the per-snapshot MCR (VM is manifest-only, no data leg) and publish its name.
	if err := sdk.EnsureManifestCapture(ctx, adapter, snapshotsdk.ManifestCaptureSpec{
		TargetAPIVersion: demov1alpha1.SchemeGroupVersion.String(),
		TargetKind:       controllercommon.KindDemoVirtualMachine,
		TargetName:       source.Name,
	}); err != nil {
		return ctrl.Result{}, err
	}

	// Barrier 1 (Planned): children planned/published and the VM MCR created. The common controller waits
	// on phase>=Planned before taking over SnapshotContent (creation, children/MCP projection, Ready mirror).
	if err := sdk.MarkPlanned(ctx, adapter); err != nil {
		return ctrl.Result{}, err
	}

	// Barrier 2 (Finished): switch on the SDK-derived capture outcome for the VM's own (manifest-only) leg.
	switch outcome := snapshotsdk.CoreCaptureOutcome(adapter); outcome.Outcome {
	case snapshotsdk.CaptureOutcomeFailed:
		return ctrl.Result{}, sdk.Reject(ctx, adapter, snapshotsdk.FailSpec{
			Reason:  snapshotsdk.Reason(outcome.Reason),
			Message: outcome.Message,
		})
	case snapshotsdk.CaptureOutcomeCaptured:
		// The VM's own manifest leg is captured. A VM aggregator additionally waits for every child disk's
		// data leg to be captured before confirming consistency: this showcases fs freeze/unfreeze — the
		// guest is unfrozen only after the disk snapshots are actually taken. Timing is driven off the
		// fine-grained per-child dataCaptured latch, not a coarse child Ready rollup.
		allCaptured, err := r.allChildrenCaptured(ctx, s)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !allCaptured {
			return ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
		}
		// (Showcase) the VM filesystem would be unfrozen here now that all disk data is captured.
		return ctrl.Result{}, sdk.ConfirmConsistent(ctx, adapter)
	default:
		// Capturing: wait for the core to finish; woken by the status watch, poll as a fallback.
		return ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
	}
}

// allChildrenCaptured reports whether every child disk snapshot has all its declared capture legs
// captured (per-child captureState.commonController). It reads the fine-grained latch rather than the
// child's Ready rollup, so the VM can time consistency (fs unfreeze) precisely against disk data capture.
// A missing/unstamped child counts as not-yet-captured to avoid a premature confirm.
func (r *DemoVirtualMachineSnapshotReconciler) allChildrenCaptured(ctx context.Context, s *demov1alpha1.DemoVirtualMachineSnapshot) (bool, error) {
	for i := range s.Status.ChildrenSnapshotRefs {
		ref := s.Status.ChildrenSnapshotRefs[i]
		if ref.APIVersion != demov1alpha1.SchemeGroupVersion.String() || ref.Kind != controllercommon.KindDemoVirtualDisk {
			continue
		}
		child := &demov1alpha1.DemoVirtualDiskSnapshot{}
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: ref.Name}, child); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if !childCoreCaptureState(child).AllLegsCaptured() {
			return false, nil
		}
	}
	return true, nil
}

// childCoreCaptureState reads a child disk snapshot's core-written leg latches into the SDK view.
func childCoreCaptureState(child *demov1alpha1.DemoVirtualDiskSnapshot) snapshotsdk.CoreCaptureState {
	return coreCaptureStateFrom(child.Status.CaptureState)
}

// planDemoVirtualMachineChildren builds the desired set of child DemoVirtualDiskSnapshot objects for the
// disks owned by the VM, honoring the absolute exclude veto. It returns the kept children plus the direct
// exclusion refs for owned disks carrying state-snapshotter.deckhouse.io/exclude: a vetoed disk gets no
// child snapshot (and hence no VCR/MCR), and the VM snapshot proceeds without it — an incomplete VM image
// is accepted by design (no consistency-group machinery; the operator owns that trade-off). Owner
// references, adoption, ref derivation, and excludedRefs publication are the SDK's job; the domain only
// authors the child object identity and its immutable spec.sourceRef, and partitions the source disks.
func (r *DemoVirtualMachineSnapshotReconciler) planDemoVirtualMachineChildren(
	ctx context.Context,
	vm *demov1alpha1.DemoVirtualMachineSnapshot,
	source *demov1alpha1.DemoVirtualMachine,
) ([]snapshotsdk.ChildSpec, []snapshotsdk.ExcludedObjectRef, error) {
	disks := &demov1alpha1.DemoVirtualDiskList{}
	if err := r.Client.List(ctx, disks, client.InNamespace(vm.Namespace)); err != nil {
		return nil, nil, err
	}
	sort.Slice(disks.Items, func(i, j int) bool {
		return disks.Items[i].Name < disks.Items[j].Name
	})

	// Collect the disks the VM owns as source objects, then split off the vetoed ones. The veto is applied
	// here (in the domain enumerator) because the SDK sees only built child specs, not source labels.
	var owned []client.Object
	for i := range disks.Items {
		disk := &disks.Items[i]
		if demoDiskOwnedByVM(disk, source) {
			owned = append(owned, disk)
		}
	}
	kept, excluded := snapshotsdk.PartitionExcluded(owned)

	children := make([]snapshotsdk.ChildSpec, 0, len(kept))
	for _, o := range kept {
		disk := o.(*demov1alpha1.DemoVirtualDisk)
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

	// The excluded refs point at the SOURCE disks (the shadow of childrenSnapshotRefs), not at child
	// snapshot objects (none exist for a vetoed disk).
	excludedRefs := make([]snapshotsdk.ExcludedObjectRef, 0, len(excluded))
	for _, o := range excluded {
		excludedRefs = append(excludedRefs, snapshotsdk.ExcludedObjectRef{
			APIVersion: demov1alpha1.SchemeGroupVersion.String(),
			Kind:       controllercommon.KindDemoVirtualDisk,
			Name:       o.GetName(),
		})
	}
	return children, excludedRefs, nil
}

// demoDiskOwnedByVM resolves the snapshot-tree parent->child link from the VM side:
// DemoVirtualMachine.spec.virtualDiskName names the owned disk (VM -> Disk -> PVC). The disk no longer
// carries a back-reference to the VM, so topology flows strictly downward.
func demoDiskOwnedByVM(disk *demov1alpha1.DemoVirtualDisk, vm *demov1alpha1.DemoVirtualMachine) bool {
	return vm.Spec.VirtualDiskName != "" && vm.Spec.VirtualDiskName == disk.Name
}
