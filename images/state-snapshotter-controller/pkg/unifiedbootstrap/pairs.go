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

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
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

// DedicatedSnapshotControllerKinds lists Snapshot root kinds reconciled IN-PROCESS by a dedicated
// controller instead of the generic GenericSnapshotBinderController. Unified runtime sync must not call
// AddWatchForPair for these. Only the namespace-root "Snapshot" is in-process; out-of-process domain
// kinds (e.g. the PoC demo domain, or virtualization) are NOT listed here — they enter discovery purely
// through eligible CustomSnapshotDefinition resources and flow through the generic domain-capture path
// (see unifiedruntime.Syncer.Sync).
//
// Order is significant: it is the deferred-activation dependency order (children before parents) consumed
// by unifiedruntime.Syncer.activateDedicatedControllersLocked. A child kind must precede any parent kind
// whose in-process controller Watches it, so the child's typed informer + field index are registered
// before the parent Watch starts that informer (controller-runtime rejects IndexField after an informer
// has started). Do not reorder without preserving children-before-parents.
var DedicatedSnapshotControllerKinds = []string{
	"Snapshot",
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

// DomainCaptureSnapshotKinds lists IN-PROCESS dedicated Snapshot kinds whose domain controller plans
// capture out-of-band (creates MCR/VCR/children, publishes captureState.domainSpecificController incl.
// phase) but whose cluster-scoped SnapshotContent is owned by the GenericSnapshotBinderController
// (content-ownership commit 2, D1). It is a strict subset of DedicatedSnapshotControllerKinds.
//
// wave5: the namespace-root "Snapshot" is the only member ("dogfooding" — the root reconciler drives
// capture through the same snapshotsdk as external domains and no longer owns its SnapshotContent; the
// generic binder creates/binds/mirrors the root content, chases its MCR->MCP, and mirrors Ready). See
// docs/wave5-namespace-domain-design.md. Out-of-process domain kinds (PoC demo, virtualization) are NOT
// listed here; they are marked domain-capture at runtime from their CustomSnapshotDefinition (the generic
// else branch in unifiedruntime.Syncer.Sync), exactly like the built-in CSI VolumeSnapshot pair.
var DomainCaptureSnapshotKinds = []string{
	"Snapshot",
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
// The rule is CSD-ORIGIN driven, not a hardcoded kind list: a kind that is registered in the live GVK
// registry (reg) but is NOT one of the built-in in-process pairs (DefaultGraphRegistryBuiltInPairs: the
// namespace-root "Snapshot" and the CSI "VolumeSnapshot") can only have entered discovery through an
// eligible CustomSnapshotDefinition. Such a kind is an external domain that serves its own restore
// apiserver, so it must be delegated. This treats the PoC demo domain and a real virtualization domain
// identically — the core carries no domain-specific kind names.
//
// The built-in kinds are excluded on purpose: the namespace-root "Snapshot" is a domain-CAPTURE kind but
// its RESTORE is served by THIS core apiserver (delegating it back would be an unbounded self-call → 500),
// and the built-in CSI "VolumeSnapshot" restore is core behavior too. A nil reg (registry not yet
// populated) and unregistered kinds return false — never delegate an unknown kind.
func IsOutOfProcessDomainSnapshotKind(reg *snapshot.GVKRegistry, kind string) bool {
	if reg == nil {
		return false
	}
	for _, p := range DefaultGraphRegistryBuiltInPairs() {
		if p.Snapshot.Kind == kind {
			return false
		}
	}
	_, err := reg.ResolveSnapshotGVK(kind)
	return err == nil
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
	return findResolvedPair(DefaultSnapshotPair().Snapshot, snapGVKs, contentGVKs)
}

// StartupBuiltInVolumeSnapshotPair returns the built-in CSI VolumeSnapshot pair (BuiltInVolumeSnapshotPair)
// from the resolved parallel slices, if present (ok=false when the CSI VolumeSnapshot CRD is absent, so the
// pair never resolved). Boot-wiring parallel to StartupDomainCaptureRootPair but for a NON-dedicated kind:
//
// FilterGenericSnapshotGVKPairs keeps VolumeSnapshot (it is not a dedicated kind), so the generic binder
// already watches it at startup via genericSnapshotGVKs — no separate watch registration is needed here.
// What IS missing at boot is the domain-capture MARK: without it the binder would treat VolumeSnapshot as a
// fully-generic kind and eagerly create + bind a SnapshotContent shell before the out-of-process
// storage-foundation VolumeSnapshot domain controller claims the object (a dual content writer). The only
// compensating mark — unifiedruntime.Syncer.Sync's "CSD-derived kind => domain-capture" else-branch — runs
// on CSD reconciles, never at pod boot, and would not fire at all in a cluster with zero CSDs. So main must
// MarkDomainCaptureKind for this pair at boot; the later Sync re-asserts it idempotently.
func StartupBuiltInVolumeSnapshotPair(snapGVKs, contentGVKs []schema.GroupVersionKind) (snap, content schema.GroupVersionKind, ok bool) {
	return findResolvedPair(BuiltInVolumeSnapshotPair().Snapshot, snapGVKs, contentGVKs)
}

// findResolvedPair returns the (snapshot, content) GVKs for target from the parallel resolved slices.
// ok is false when target is absent or the slices are mismatched.
func findResolvedPair(target schema.GroupVersionKind, snapGVKs, contentGVKs []schema.GroupVersionKind) (snap, content schema.GroupVersionKind, ok bool) {
	if len(snapGVKs) != len(contentGVKs) {
		return schema.GroupVersionKind{}, schema.GroupVersionKind{}, false
	}
	for i := range snapGVKs {
		if snapGVKs[i] == target {
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
