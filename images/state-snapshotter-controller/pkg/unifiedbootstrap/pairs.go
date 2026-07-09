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

package unifiedbootstrap

import (
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ResolveAvailableUnifiedPairs returns bootstrap/CSD pairs where both sides exist in the API (RESTMapper).
func ResolveAvailableUnifiedPairs(mapper meta.RESTMapper, pairs []UnifiedGVKPair, log logr.Logger) []UnifiedGVKPair {
	var out []UnifiedGVKPair
	for _, p := range pairs {
		if _, err := mapper.RESTMapping(p.Snapshot.GroupKind(), p.Snapshot.Version); err != nil {
			log.Info("skipping unified snapshot GVK pair: snapshot kind not available in API",
				"snapshot", p.Snapshot.String(), "snapshotContent", p.SnapshotContent.String(), "err", err)
			continue
		}
		if _, err := mapper.RESTMapping(p.SnapshotContent.GroupKind(), p.SnapshotContent.Version); err != nil {
			log.Info("skipping unified snapshot GVK pair: snapshot content kind not available in API",
				"snapshot", p.Snapshot.String(), "snapshotContent", p.SnapshotContent.String(), "err", err)
			continue
		}
		out = append(out, p)
	}
	return out
}

// PairsFromSnapshotGVKs builds pairs using common SnapshotContent for the content side (minimal bootstrap / tests).
func PairsFromSnapshotGVKs(snapshotGVKs []schema.GroupVersionKind) []UnifiedGVKPair {
	out := make([]UnifiedGVKPair, 0, len(snapshotGVKs))
	for _, gvk := range snapshotGVKs {
		out = append(out, UnifiedGVKPair{
			Snapshot:        gvk,
			SnapshotContent: CommonSnapshotContentGVK(),
		})
	}
	return out
}

// PairsExcludingSnapshotKinds drops pairs whose Snapshot.Kind is in skip.
func PairsExcludingSnapshotKinds(pairs []UnifiedGVKPair, skipKinds ...string) []UnifiedGVKPair {
	skip := make(map[string]struct{}, len(skipKinds))
	for _, k := range skipKinds {
		skip[k] = struct{}{}
	}
	var out []UnifiedGVKPair
	for _, p := range pairs {
		if _, drop := skip[p.Snapshot.Kind]; drop {
			continue
		}
		out = append(out, p)
	}
	return out
}

// DedicatedSnapshotControllerKinds lists Snapshot root kinds reconciled outside the generic
// GenericSnapshotBinderController. Unified runtime sync must not call AddWatchForPair for these.
//
// Order is significant: it is the deferred-activation dependency order (children before parents)
// consumed by unifiedruntime.Syncer.activateDedicatedControllersLocked. A child kind must precede any
// parent kind whose controller Watches it, so the child's typed informer + field index are registered
// before the parent Watch starts that informer (controller-runtime rejects IndexField after an informer
// has started). DemoVirtualDiskSnapshot therefore must stay before DemoVirtualMachineSnapshot (the VM
// controller Watches the disk snapshot type). Do not reorder without preserving children-before-parents.
var DedicatedSnapshotControllerKinds = []string{
	"Snapshot",
	// PR5a demo domain: reconciled by DemoVirtualDiskSnapshotReconciler, not GenericSnapshotBinderController.
	"DemoVirtualDiskSnapshot",
	// PR5b demo domain: reconciled by DemoVirtualMachineSnapshotReconciler, not GenericSnapshotBinderController.
	// Activated after DemoVirtualDiskSnapshot because its controller Watches DemoVirtualDiskSnapshot.
	"DemoVirtualMachineSnapshot",
}

// IsDedicatedSnapshotControllerKind reports whether kind is handled by a dedicated reconciler.
func IsDedicatedSnapshotControllerKind(kind string) bool {
	for _, k := range DedicatedSnapshotControllerKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// DomainCaptureSnapshotKinds lists dedicated Snapshot kinds whose domain controller plans capture
// out-of-band (creates MCR/VCR/children, publishes captureState.domainSpecificController incl. phase) but
// whose cluster-scoped SnapshotContent is owned by the GenericSnapshotBinderController (content-ownership
// commit 2, D1). They are a strict subset of DedicatedSnapshotControllerKinds: the dedicated planning
// controller is still activated for them, AND the generic binder additionally watches them (gated by
// MarkDomainCaptureKind) to create/project/mirror their SnapshotContent.
//
// wave5: the namespace-root "Snapshot" is now in this set too ("dogfooding" — the root reconciler drives
// capture through the same snapshotsdk as external/demo domains and no longer owns its SnapshotContent;
// the generic binder creates/binds/mirrors the root content, chases its MCR->MCP, and mirrors Ready).
// See docs/wave5-namespace-domain-design.md.
var DomainCaptureSnapshotKinds = []string{
	"Snapshot",
	"DemoVirtualDiskSnapshot",
	"DemoVirtualMachineSnapshot",
}

// IsDomainCaptureSnapshotKind reports whether kind is dedicated for planning but uses the generic binder
// for SnapshotContent ownership.
func IsDomainCaptureSnapshotKind(kind string) bool {
	for _, k := range DomainCaptureSnapshotKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// IsOutOfProcessDomainSnapshotKind reports whether kind is owned by a SEPARATE, out-of-process domain
// controller that serves its own aggregated restore apiserver, so the core restore compiler must
// DELEGATE that node's subtree to the domain apiserver instead of compiling it in-process.
//
// This is the domain-capture set (IsDomainCaptureSnapshotKind) MINUS the built-in root "Snapshot".
// Since wave5 the namespace-root "Snapshot" is a domain-CAPTURE kind (planned via the in-process SDK,
// its content owned by the generic binder), but its RESTORE is served by THIS core apiserver. Treating
// the root as an out-of-process domain node makes the restore compiler delegate it back to core's own
// manifests-with-data-restoration endpoint — an unbounded self-call that surfaces as a 500. Only the
// demo/external kinds are genuinely out-of-process for restore, so the root is excluded here.
func IsOutOfProcessDomainSnapshotKind(kind string) bool {
	return kind != DefaultSnapshotPair().Snapshot.Kind && IsDomainCaptureSnapshotKind(kind)
}

// FilterGenericSnapshotGVKPairs returns parallel slices with dedicated snapshot kinds removed.
func FilterGenericSnapshotGVKPairs(snapGVKs, contentGVKs []schema.GroupVersionKind) (snapOut, contentOut []schema.GroupVersionKind) {
	if len(snapGVKs) != len(contentGVKs) {
		return nil, nil
	}
	for i := range snapGVKs {
		if IsDedicatedSnapshotControllerKind(snapGVKs[i].Kind) {
			continue
		}
		snapOut = append(snapOut, snapGVKs[i])
		contentOut = append(contentOut, contentGVKs[i])
	}
	return snapOut, contentOut
}

// StartupDomainCaptureRootPair returns the built-in root Snapshot pair (DefaultSnapshotPair) from the
// resolved parallel slices, if present. ok is false when the root pair is not in the resolved set
// (CRDs absent) or the slices are mismatched.
//
// The generic binder MUST watch this pair at startup. Since wave5 the namespace-root "Snapshot" is a
// domain-capture kind whose cluster-scoped SnapshotContent is created/bound/mirrored by the generic
// binder (not by the root reconciler). But FilterGenericSnapshotGVKPairs strips every dedicated kind
// (root included), and the only compensating registration — unifiedruntime.Syncer.Sync — runs on CSD
// reconciles, never at pod boot. So without an explicit startup registration the binder never watches
// the root Snapshot and root SnapshotContent is never created (the root capture path silently hangs
// pre-bind). Unlike demo domain-capture kinds (which must stay deferred to Sync until their CSD grants
// RBAC, to avoid a cache-sync deadlock), the built-in root is always present and needs no gating, so it
// is safe to register directly at startup. Registration is idempotent, so a later Sync is a no-op.
func StartupDomainCaptureRootPair(snapGVKs, contentGVKs []schema.GroupVersionKind) (snap, content schema.GroupVersionKind, ok bool) {
	if len(snapGVKs) != len(contentGVKs) {
		return schema.GroupVersionKind{}, schema.GroupVersionKind{}, false
	}
	root := DefaultSnapshotPair().Snapshot
	for i := range snapGVKs {
		if snapGVKs[i] == root {
			return snapGVKs[i], contentGVKs[i], true
		}
	}
	return schema.GroupVersionKind{}, schema.GroupVersionKind{}, false
}

// FilterGenericSnapshotContentGVKs drops content GVKs whose snapshot side is handled by a dedicated
// reconciler. Common SnapshotContent may still be kept through other generic pairs after dedupe.
func FilterGenericSnapshotContentGVKs(snapshotGVKs, contentGVKs []schema.GroupVersionKind) []schema.GroupVersionKind {
	if len(snapshotGVKs) != len(contentGVKs) {
		return nil
	}
	var out []schema.GroupVersionKind
	for i := range snapshotGVKs {
		if IsDedicatedSnapshotControllerKind(snapshotGVKs[i].Kind) {
			continue
		}
		out = append(out, contentGVKs[i])
	}
	return out
}

// AppendGVKIfMissing appends gvk only when the same Group/Version/Kind is not already present.
func AppendGVKIfMissing(gvks []schema.GroupVersionKind, gvk schema.GroupVersionKind) []schema.GroupVersionKind {
	for _, existing := range gvks {
		if existing == gvk {
			return gvks
		}
	}
	return append(gvks, gvk)
}

// DedupeSnapshotContentGVKs returns unique SnapshotContent GVKs preserving first-seen order.
func DedupeSnapshotContentGVKs(pairs []UnifiedGVKPair) []schema.GroupVersionKind {
	seen := make(map[string]struct{})
	var out []schema.GroupVersionKind
	for _, p := range pairs {
		k := p.SnapshotContent.String()
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, p.SnapshotContent)
	}
	return out
}
