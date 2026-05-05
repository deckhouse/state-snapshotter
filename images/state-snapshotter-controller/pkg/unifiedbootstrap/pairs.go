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

package unifiedbootstrap

import (
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ResolveAvailableUnifiedPairs returns bootstrap/DSC pairs where both sides exist in the API (RESTMapper).
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
var DedicatedSnapshotControllerKinds = []string{
	"NamespaceSnapshot",
	// PR5a demo domain: reconciled by DemoVirtualDiskSnapshotReconciler, not GenericSnapshotBinderController.
	"DemoVirtualDiskSnapshot",
	// PR5b demo domain: reconciled by DemoVirtualMachineSnapshotReconciler, not GenericSnapshotBinderController.
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
