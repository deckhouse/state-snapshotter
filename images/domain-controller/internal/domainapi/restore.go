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
	"sort"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/domain-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/domain-controller/internal/controllers/demo"
	"github.com/deckhouse/state-snapshotter/images/domain-controller/internal/logger"
	"github.com/deckhouse/state-snapshotter/images/domain-controller/internal/usecase/restore"
	domainsdk "github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/transform"
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
// SnapshotContent / ManifestCheckpoint: each node's un-transformed own (single-node) BASE manifests come
// from the core aggregated API server's per-CR manifests-download (CoreBaseManifestsFetcher), and the
// domain-specific restore mutation is applied in-process via the demo restore transformer (equivalent to
// demo.TransformObject / CoveredPVCNames). Restore is per-CR (C9): the service recurses the domain run
// tree (status.childrenSnapshotRefs) itself, compiling one node at a time, rather than transforming a
// single whole-subtree dump.
type RestoreService struct {
	reader      client.Reader
	core        CoreBaseManifestsFetcher
	transformer domainsdk.Transformer
	log         logger.LoggerInterface
}

// NewRestoreService builds the domain restore service. reader reads demo snapshot CRs (readiness gate +
// children recursion); core fetches each node's own base manifests; the transformer is the in-process
// demo restore transformer.
func NewRestoreService(reader client.Reader, core CoreBaseManifestsFetcher, log logger.LoggerInterface) *RestoreService {
	return &RestoreService{
		reader:      reader,
		core:        core,
		transformer: demo.NewRestoreTransformer(),
		log:         log,
	}
}

// ManifestsWithDataRestoration returns the restore-ready manifests for a demo snapshot subtree, compiled
// per-CR (C9): it recurses the domain run tree from the addressed node, fetching each node's own base
// from core's per-CR manifests-download, sanitizing for restore (same shared sanitizer the core compiler
// uses) and applying the demo restore mutation in-process (cover domain-owned PVCs, point each restored
// DemoVirtualDisk at its own owning DemoVirtualDiskSnapshot via spec.dataSource). targetNamespace
// defaults to the source namespace when empty.
func (s *RestoreService) ManifestsWithDataRestoration(ctx context.Context, resource, namespace, name, targetNamespace string) ([]byte, error) {
	out, err := s.restoreNode(ctx, resource, namespace, name, targetNamespace, map[string]struct{}{})
	if err != nil {
		return nil, err
	}
	if s.log != nil {
		s.log.Debug("[domainapi] compiled manifests-with-data-restoration", "resource", resource, "namespace", namespace, "name", name, "objects", len(out))
	}
	return marshalObjects(out)
}

// restoreNode compiles ONE domain snapshot node per-CR (Variant B): it enforces the readiness gate, then
// recurses into the node's domain children (status.childrenSnapshotRefs), then fetches the node's OWN
// base manifests from core's per-CR manifests-download and applies the demo restore transform. Output is
// post-order (children before parent), so leaf disks precede the VM that depends on them — friendlier for
// a straight apply. visited guards against a run-tree cycle.
//
// The readiness gate is fail-closed per node: a snapshot's Ready mirrors its bound SnapshotContent.Ready
// (ManifestsReady && VolumesReady && ChildrenReady), so requiring each visited node Ready=True prevents
// restoring stale data from a node that is mid-recapture.
func (s *RestoreService) restoreNode(ctx context.Context, resource, namespace, name, targetNamespace string, visited map[string]struct{}) ([]unstructured.Unstructured, error) {
	key := resource + "/" + name
	if _, ok := visited[key]; ok {
		return nil, fmt.Errorf("domain snapshot run-tree cycle at %s/%s", resource, name)
	}
	visited[key] = struct{}{}

	children, err := s.loadDomainNode(ctx, resource, namespace, name)
	if err != nil {
		return nil, err
	}
	// Deterministic sibling order (Kind, Name) so the compiled output is reproducible across runs,
	// matching the core resolver which sorts children before recursing.
	sort.Slice(children, func(i, j int) bool {
		if children[i].Kind != children[j].Kind {
			return children[i].Kind < children[j].Kind
		}
		return children[i].Name < children[j].Name
	})

	out := make([]unstructured.Unstructured, 0)
	for i := range children {
		ref := children[i]
		childResource, ok := domainResourceForKind(ref.Kind)
		if !ok {
			// The domain apiserver recurses only its own domain children. A non-domain child under a
			// domain node is not part of the demo model; fail closed rather than silently dropping its
			// subtree (core owns generic/own kinds and never descends into a domain boundary).
			return nil, fmt.Errorf("domain snapshot %s %s/%s has unsupported child kind %q", resource, namespace, name, ref.Kind)
		}
		childObjs, err := s.restoreNode(ctx, childResource, namespace, ref.Name, targetNamespace, visited)
		if err != nil {
			return nil, err
		}
		out = append(out, childObjs...)
	}

	base, err := s.core.NodeBaseManifests(ctx, resource, namespace, name)
	if err != nil {
		return nil, err
	}
	// A disk snapshot node owns the DemoVirtualDisk in its OWN base and points it at itself
	// (spec.dataSource -> this disk snapshot). A VM node carries no disk in its own base, so the owner is
	// unused there.
	ownerSnapshotName := ""
	if resource == ResourceDemoVirtualDiskSnapshot {
		ownerSnapshotName = name
	}
	own, err := s.applyTransform(base, namespace, targetNamespace, ownerSnapshotName)
	if err != nil {
		return nil, err
	}
	out = append(out, own...)
	return out, nil
}

// domainResourceForKind maps a snapshot Kind (as carried in status.childrenSnapshotRefs) to its
// lowercase plural domain resource. ok is false for any non-domain kind.
func domainResourceForKind(kind string) (resource string, ok bool) {
	switch kind {
	case controllercommon.KindDemoVirtualMachineSnapshot:
		return ResourceDemoVirtualMachineSnapshot, true
	case controllercommon.KindDemoVirtualDiskSnapshot:
		return ResourceDemoVirtualDiskSnapshot, true
	default:
		return "", false
	}
}

// loadDomainNode reads the addressed domain snapshot CR once, enforces the fail-closed readiness gate,
// and returns its direct domain children (status.childrenSnapshotRefs) for the per-CR recursion. A
// missing Ready condition (e.g. mid-reconcile) counts as not ready: restore must never compile from an
// unfinished node. A disk snapshot is a leaf and has no children.
func (s *RestoreService) loadDomainNode(ctx context.Context, resource, namespace, name string) ([]storagev1alpha1.SnapshotChildRef, error) {
	var (
		conditions []metav1.Condition
		children   []storagev1alpha1.SnapshotChildRef
	)
	switch resource {
	case ResourceDemoVirtualDiskSnapshot:
		obj := &demov1alpha1.DemoVirtualDiskSnapshot{}
		if err := s.reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
			return nil, fmt.Errorf("get DemoVirtualDiskSnapshot %s/%s: %w", namespace, name, err)
		}
		conditions = obj.Status.Conditions
	case ResourceDemoVirtualMachineSnapshot:
		obj := &demov1alpha1.DemoVirtualMachineSnapshot{}
		if err := s.reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
			return nil, fmt.Errorf("get DemoVirtualMachineSnapshot %s/%s: %w", namespace, name, err)
		}
		conditions = obj.Status.Conditions
		children = obj.Status.ChildrenSnapshotRefs
	default:
		return nil, fmt.Errorf("unsupported domain snapshot resource %q", resource)
	}
	if !meta.IsStatusConditionTrue(conditions, storagev1alpha1.ConditionReady) {
		return nil, fmt.Errorf("domain snapshot %s %s/%s is not Ready", resource, namespace, name)
	}
	return children, nil
}

// applyTransform turns one node's own base manifests into apply-ready restore manifests: it re-attaches
// the effective namespace (core's manifests-download is namespace-relative), runs the shared restore
// sanitizer (strip server fields/status/control-plane kinds, same as the core compiler), drops
// domain-covered PVCs, and points each DemoVirtualDisk in this node's base at ownerSnapshotName via
// spec.dataSource. ownerSnapshotName is this node's own disk snapshot name for a disk node, and "" for a
// VM node (whose own base carries no disk).
func (s *RestoreService) applyTransform(base []unstructured.Unstructured, namespace, targetNamespace, ownerSnapshotName string) ([]unstructured.Unstructured, error) {
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
		// Core's manifests-download strips metadata.namespace (namespace-relative). All base objects were
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
			// A PVC not covered by a domain disk has no data capture here (the sanitizer stripped any
			// dataSource/dataSourceRef and the domain path does not do generic orphan-PVC -> VolumeSnapshot
			// binding). It would restore empty. This is unreachable in the current demo model (every demo
			// PVC is disk-covered). Fail closed rather than emit a silent data-less PVC, matching the core
			// compiler's contract so the guarantee "restore never emits a data-less PVC" holds uniformly
			// across the delegation boundary.
			return nil, fmt.Errorf("uncovered PVC %s/%s has no data binding; refusing to emit a data-less PVC", effectiveNS, sanitized.GetName())
		}
		if isDemoVirtualDisk(sanitized) {
			owner := ownerSnapshotName
			if owner == "" {
				// Defensive: in the per-CR model a disk node always passes its own snapshot name as
				// ownerSnapshotName, so owner == "" only happens if a VM node's own base unexpectedly
				// carries a disk. The disk's PVC (if any) was dropped above as "covered", so emitting the
				// disk without a spec.dataSource would silently restore it as an empty volume. Fail closed
				// when the disk carries a data capture (a covered PVC), matching the core compiler's contract
				// that restore never emits a data-less object. A disk with no PVC captures no data and is
				// safe to emit untouched.
				if pvcName, _, _ := unstructured.NestedString(sanitized.Object, "spec", "persistentVolumeClaimName"); pvcName != "" {
					return nil, fmt.Errorf("DemoVirtualDisk %s/%s captures data (PVC %q) but has no resolvable owning DemoVirtualDiskSnapshot; refusing to emit a data-less disk", effectiveNS, sanitized.GetName(), pvcName)
				}
				if s.log != nil {
					s.log.Warning("[domainapi] DemoVirtualDisk has no PVC to capture as data and no owning disk snapshot; restored without a data source", "namespace", effectiveNS, "disk", sanitized.GetName())
				}
			} else {
				node := &domainsdk.RestoreNode{
					SnapshotRef: storagev1alpha1.ObjectRef{
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

// marshalObjects marshals the per-CR recursion output as a JSON array, failing closed on a duplicate
// object identity (apiVersion/kind/namespace/name). Each node's own base from core's manifests-download
// is already deduped intra-node upstream; across nodes the per-CR model accumulates disjoint objects, so
// a collision means two nodes captured the same identity (possibly with different content). It is
// treated as a contract violation rather than silently collapsed — mirroring the core restore compiler's
// fail-closed dedup so the "restore never silently picks one of two same-identity objects" guarantee
// holds uniformly across the core↔domain delegation boundary.
func marshalObjects(objs []unstructured.Unstructured) ([]byte, error) {
	seen := make(map[string]struct{}, len(objs))
	out := make([]unstructured.Unstructured, 0, len(objs))
	for i := range objs {
		o := objs[i]
		key := o.GetAPIVersion() + "/" + o.GetKind() + "/" + o.GetNamespace() + "/" + o.GetName()
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("duplicate object in domain restore output: %s", key)
		}
		seen[key] = struct{}{}
		out = append(out, o)
	}
	if len(out) == 0 {
		return []byte("[]"), nil
	}
	return json.Marshal(out)
}
