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

package domainapi

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/demo"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// Domain snapshot resources (lowercase plural) this controller serves.
const (
	ResourceDemoVirtualDiskSnapshot    = "demovirtualdisksnapshots"
	ResourceDemoVirtualMachineSnapshot = "demovirtualmachinesnapshots"
)

// IsDomainSnapshotResource reports whether the given lowercase plural resource is owned by this domain
// controller.
func IsDomainSnapshotResource(resource string) bool {
	switch resource {
	case ResourceDemoVirtualDiskSnapshot, ResourceDemoVirtualMachineSnapshot:
		return true
	default:
		return false
	}
}

// RestoreService compiles manifests-with-data-restoration for demo snapshot kinds. It NEVER reads
// SnapshotContent / ManifestCheckpoint: the un-transformed BASE manifests come from the core aggregated
// API server (CoreBaseManifestsFetcher), and the domain-specific restore mutation is applied
// in-process via the demo restore transformer (equivalent to demo.TransformObject / CoveredPVCNames).
type RestoreService struct {
	reader      client.Reader
	core        CoreBaseManifestsFetcher
	transformer restore.DomainRestoreTransformer
	log         logger.LoggerInterface
}

// NewRestoreService builds the domain restore service. reader reads demo snapshot CRs (to resolve the
// disk -> disk-snapshot ownership for VM subtrees); core fetches base manifests; the transformer is the
// in-process demo restore transformer.
func NewRestoreService(reader client.Reader, core CoreBaseManifestsFetcher, log logger.LoggerInterface) *RestoreService {
	return &RestoreService{
		reader:      reader,
		core:        core,
		transformer: demo.NewRestoreTransformer(),
		log:         log,
	}
}

// BaseManifests returns the (un-transformed) aggregated base manifests for a demo snapshot, proxied
// from the core API server. It backs the plain /manifests subresource.
func (s *RestoreService) BaseManifests(ctx context.Context, resource, namespace, name string) ([]byte, error) {
	base, err := s.core.BaseManifests(ctx, resource, namespace, name)
	if err != nil {
		return nil, err
	}
	return marshalObjects(base)
}

// ManifestsWithDataRestoration returns the restore-ready manifests for a demo snapshot: the core base
// manifests, sanitized for restore (same shared sanitizer the core compiler uses) with the demo restore
// mutation applied in-process (cover domain-owned PVCs, point restored DemoVirtualDisks at their owning
// DemoVirtualDiskSnapshot via spec.dataSource). targetNamespace defaults to the source namespace when
// empty.
func (s *RestoreService) ManifestsWithDataRestoration(ctx context.Context, resource, namespace, name, targetNamespace string) ([]byte, error) {
	base, err := s.core.BaseManifests(ctx, resource, namespace, name)
	if err != nil {
		return nil, err
	}
	owners, err := s.resolveDiskOwners(ctx, resource, namespace, name)
	if err != nil {
		return nil, err
	}
	out, err := s.applyTransform(base, namespace, targetNamespace, owners)
	if err != nil {
		return nil, err
	}
	if s.log != nil {
		s.log.Debug("[domainapi] compiled manifests-with-data-restoration", "resource", resource, "namespace", namespace, "name", name, "objects", len(out))
	}
	return marshalObjects(out)
}

// diskOwnerResolver maps a captured DemoVirtualDisk name to the DemoVirtualDiskSnapshot that owns it.
// For a leaf disk snapshot the captured disk's name is not needed up front, so defaultOwner is set to
// the disk snapshot itself; for a VM subtree the mapping is resolved per child disk snapshot.
type diskOwnerResolver struct {
	byDiskName   map[string]string
	defaultOwner string
}

func (r diskOwnerResolver) ownerFor(diskName string) string {
	if r.byDiskName != nil {
		if owner, ok := r.byDiskName[diskName]; ok {
			return owner
		}
	}
	return r.defaultOwner
}

func (s *RestoreService) resolveDiskOwners(ctx context.Context, resource, namespace, name string) (diskOwnerResolver, error) {
	switch resource {
	case ResourceDemoVirtualDiskSnapshot:
		// A leaf disk snapshot owns whichever DemoVirtualDisk it captured.
		return diskOwnerResolver{defaultOwner: name}, nil
	case ResourceDemoVirtualMachineSnapshot:
		return s.resolveVMDiskOwners(ctx, namespace, name)
	default:
		return diskOwnerResolver{}, fmt.Errorf("unsupported domain snapshot resource %q", resource)
	}
}

func (s *RestoreService) resolveVMDiskOwners(ctx context.Context, namespace, name string) (diskOwnerResolver, error) {
	vm := &demov1alpha1.DemoVirtualMachineSnapshot{}
	if err := s.reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, vm); err != nil {
		return diskOwnerResolver{}, fmt.Errorf("get DemoVirtualMachineSnapshot %s/%s: %w", namespace, name, err)
	}
	owners := map[string]string{}
	for i := range vm.Status.ChildrenSnapshotRefs {
		ref := vm.Status.ChildrenSnapshotRefs[i]
		if ref.Kind != controllercommon.KindDemoVirtualDiskSnapshot {
			continue
		}
		disk := &demov1alpha1.DemoVirtualDiskSnapshot{}
		if err := s.reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, disk); err != nil {
			if apierrors.IsNotFound(err) {
				if s.log != nil {
					s.log.Warning("[domainapi] child DemoVirtualDiskSnapshot not found; its disk will be restored without a data source", "namespace", namespace, "diskSnapshot", ref.Name, "vmSnapshot", name)
				}
				continue
			}
			return diskOwnerResolver{}, fmt.Errorf("get child DemoVirtualDiskSnapshot %s/%s: %w", namespace, ref.Name, err)
		}
		diskName := diskSnapshotSourceName(disk)
		if diskName == "" {
			if s.log != nil {
				s.log.Warning("[domainapi] child DemoVirtualDiskSnapshot has no resolvable source disk; restored disk will have no data source", "namespace", namespace, "diskSnapshot", ref.Name, "vmSnapshot", name)
			}
			continue
		}
		owners[diskName] = ref.Name
	}
	return diskOwnerResolver{byDiskName: owners}, nil
}

// diskSnapshotSourceName returns the captured DemoVirtualDisk name for a disk snapshot, preferring the
// generic source-identity annotation (source-of-truth for root-planned snapshots) and falling back to
// spec.sourceRef (manual API-compat).
func diskSnapshotSourceName(disk *demov1alpha1.DemoVirtualDiskSnapshot) string {
	if identity, err := controllercommon.DecodeSnapshotSourceIdentityAnnotations(disk.Annotations); err == nil {
		if identity.Kind == controllercommon.KindDemoVirtualDisk && identity.Name != "" {
			return identity.Name
		}
	}
	if disk.Spec.SourceRef.Kind == controllercommon.KindDemoVirtualDisk && disk.Spec.SourceRef.Name != "" {
		return disk.Spec.SourceRef.Name
	}
	return ""
}

// applyTransform turns the core base manifests into apply-ready restore manifests: it re-attaches the
// effective namespace (core's /manifests is namespace-relative), runs the shared restore sanitizer
// (strip server fields/status/control-plane kinds, same as the core compiler), drops domain-covered
// PVCs, and applies the demo restore mutation to each DemoVirtualDisk under its owning disk snapshot.
func (s *RestoreService) applyTransform(base []unstructured.Unstructured, namespace, targetNamespace string, owners diskOwnerResolver) ([]unstructured.Unstructured, error) {
	effectiveNS := targetNamespace
	if effectiveNS == "" {
		effectiveNS = namespace
	}
	// CoveredPVCNames is read from the raw base (it only inspects DemoVirtualDisk.spec, untouched by
	// namespace re-attachment below).
	covered := s.transformer.CoveredPVCNames(nil, base)

	out := make([]unstructured.Unstructured, 0, len(base))
	for i := range base {
		obj := base[i]
		// Core's /manifests strips metadata.namespace (namespace-relative). All base objects were
		// namespaced (cluster-scoped objects are dropped upstream), so re-attach the effective namespace
		// before sanitizing — the shared sanitizer drops namespace-less objects as cluster-scoped.
		obj.SetNamespace(effectiveNS)

		sanitized, keep := restore.SanitizeForRestore(obj, effectiveNS)
		if !keep {
			continue
		}
		if isPersistentVolumeClaim(sanitized) {
			if _, ok := covered[sanitized.GetName()]; ok {
				continue
			}
			// A PVC not covered by a domain disk has no data leg here (the sanitizer stripped any
			// dataSource/dataSourceRef and the domain path does not do generic orphan-PVC -> VolumeSnapshot
			// binding). It would restore empty. This is unreachable in the current demo model (every demo
			// PVC is disk-covered); warn rather than emit a silent data-less PVC.
			if s.log != nil {
				s.log.Warning("[domainapi] emitting uncovered PVC with no data binding; it will be restored empty", "namespace", effectiveNS, "pvc", sanitized.GetName())
			}
		}
		if isDemoVirtualDisk(sanitized) {
			owner := owners.ownerFor(sanitized.GetName())
			if owner == "" {
				if s.log != nil {
					s.log.Warning("[domainapi] DemoVirtualDisk has no owning disk snapshot; restored without a data source", "namespace", effectiveNS, "disk", sanitized.GetName())
				}
			} else {
				node := &restore.RestoreNode{
					SnapshotRef: snapshot.ObjectRef{
						APIVersion: demov1alpha1.SchemeGroupVersion.String(),
						Kind:       controllercommon.KindDemoVirtualDiskSnapshot,
						Name:       owner,
						Namespace:  effectiveNS,
					},
				}
				if _, err := s.transformer.TransformObject(node, &sanitized, nil); err != nil {
					return nil, fmt.Errorf("transform DemoVirtualDisk %s: %w", sanitized.GetName(), err)
				}
			}
		}
		out = append(out, sanitized)
	}
	return out, nil
}

func isPersistentVolumeClaim(obj unstructured.Unstructured) bool {
	return obj.GetKind() == "PersistentVolumeClaim" && obj.GetAPIVersion() == "v1"
}

func isDemoVirtualDisk(obj unstructured.Unstructured) bool {
	return obj.GetKind() == controllercommon.KindDemoVirtualDisk &&
		obj.GetAPIVersion() == demov1alpha1.SchemeGroupVersion.String()
}

// marshalObjects deduplicates by GVK/namespace/name and marshals the objects as a JSON array. The core
// /manifests base is already deduped upstream (it fails closed on duplicates), so this is a defensive
// last-writer-wins guard rather than a primary dedup.
func marshalObjects(objs []unstructured.Unstructured) ([]byte, error) {
	seen := make(map[string]struct{}, len(objs))
	deduped := make([]unstructured.Unstructured, 0, len(objs))
	for i := range objs {
		o := objs[i]
		key := o.GetAPIVersion() + "/" + o.GetKind() + "/" + o.GetNamespace() + "/" + o.GetName()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, o)
	}
	if len(deduped) == 0 {
		return []byte("[]"), nil
	}
	return json.Marshal(deduped)
}
