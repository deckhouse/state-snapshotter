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

package namespacemanifest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

// State-snapshotter own machinery API groups/kinds that discovery will enumerate inside the target
// namespace but that MUST NOT enter the snapshot manifest inventory (capturing them would be
// self-referential and break restore). Snapshot/SnapshotContent are excluded via mechanism 1
// (snapshot-kind set from the live GVKRegistry); these are the remaining request/transfer objects.
const (
	ownMachineryGroupStateSnapshotter = "state-snapshotter.deckhouse.io"
	ownMachineryGroupStorage          = "storage.deckhouse.io"
)

// storageMachineryKinds are the storage.deckhouse.io kinds that are state-snapshotter machinery
// (not user desired-state). The Snapshot kind from the same group is excluded by mechanism 1.
var storageMachineryKinds = map[string]struct{}{
	"VolumeCaptureRequest": {},
	"VolumeRestoreRequest": {},
	"DataExport":           {},
	"DataImport":           {},
}

// SnapshotMachineryGVKs is the set of snapshot/content GVKs created by the snapshotter itself
// (mechanism 1, kind-level dedup). Built by the caller from the live snapshot.GVKRegistry; the
// package does not import the registry to avoid an import cycle. Membership is matched by
// GroupKind (version-agnostic) so a served version mismatch does not leak our own machinery.
type SnapshotMachineryGVKs map[schema.GroupVersionKind]struct{}

func (s SnapshotMachineryGVKs) containsGroupKind(gk schema.GroupKind) bool {
	for gvk := range s {
		if gvk.GroupKind() == gk {
			return true
		}
	}
	return false
}

// BuildManifestCaptureTargets enumerates every namespaced resource type via discovery and lists each
// in the target namespace, keeping only objects that pass ShouldIncludeNamespaceObject (default-include;
// drop controller-owned dependents, control-plane noise, snapshot machinery and own machinery).
//
// Error handling is fail-closed and three-classed (see Snapshot controller capture flow):
//   - Forbidden listing / partial discovery (broken aggregated APIService) -> the affected GVRs are
//     returned in unreadable (capture is incomplete; the caller degrades transiently and requeues).
//   - discovery-not-found for a type that genuinely disappeared -> silently skipped.
//   - any other list/discovery error -> returned wrapped (%w) so the caller can classify it as a
//     transient apiserver hiccup (requeue) or a structural/terminal failure.
//
// The returned target slice is sorted by (APIVersion, Kind, Name) for stable MCR spec and drift checks.
func BuildManifestCaptureTargets(
	ctx context.Context,
	dyn dynamic.Interface,
	disco discovery.DiscoveryInterface,
	namespace string,
	snapshotKinds SnapshotMachineryGVKs,
) (targets []ManifestTarget, unreadable []schema.GroupVersionResource, err error) {
	if dyn == nil {
		return nil, nil, fmt.Errorf("namespacemanifest: dynamic client is nil")
	}
	if disco == nil {
		return nil, nil, fmt.Errorf("namespacemanifest: discovery client is nil")
	}

	gvrs, unreadable, err := enumerateNamespacedGVRs(disco)
	if err != nil {
		return nil, unreadable, err
	}

	seen := make(map[string]struct{})
	for _, gvr := range gvrs {
		lst, lerr := dyn.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if lerr != nil {
			if isDiscoveryNotFound(lerr) {
				continue
			}
			if apierrors.IsForbidden(lerr) {
				unreadable = append(unreadable, gvr)
				continue
			}
			return nil, unreadable, fmt.Errorf("list %s in namespace %s: %w", gvr.String(), namespace, lerr)
		}
		for i := range lst.Items {
			item := lst.Items[i]
			key := objectKey(&item)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			if !ShouldIncludeNamespaceObject(&item, snapshotKinds) {
				continue
			}
			apiVersion := item.GetAPIVersion()
			if apiVersion == "" {
				apiVersion = gvr.GroupVersion().String()
			}
			kind := item.GetKind()
			if kind == "" {
				continue
			}
			targets = append(targets, ManifestTarget{
				APIVersion: apiVersion,
				Kind:       kind,
				Name:       item.GetName(),
			})
		}
	}

	sortManifestTargets(targets)
	return targets, unreadable, nil
}

// enumerateNamespacedGVRs returns every namespaced GVR (preferred version) that supports both list and
// get and is not a subresource. Partial discovery failures (broken aggregated APIServices) are returned
// in unreadable instead of being silently dropped, so the caller fails closed.
func enumerateNamespacedGVRs(disco discovery.DiscoveryInterface) (gvrs []schema.GroupVersionResource, unreadable []schema.GroupVersionResource, err error) {
	lists, derr := disco.ServerPreferredNamespacedResources()
	if derr != nil {
		var groupErr *discovery.ErrGroupDiscoveryFailed
		if errors.As(derr, &groupErr) {
			for gv := range groupErr.Groups {
				unreadable = append(unreadable, schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: "*"})
			}
			// fall through: lists still holds the resources that were discovered successfully.
		} else {
			return nil, unreadable, fmt.Errorf("discover namespaced resources: %w", derr)
		}
	}
	for _, list := range lists {
		if list == nil {
			continue
		}
		gv, perr := schema.ParseGroupVersion(list.GroupVersion)
		if perr != nil {
			return nil, unreadable, fmt.Errorf("parse discovery groupVersion %q: %w", list.GroupVersion, perr)
		}
		for i := range list.APIResources {
			res := list.APIResources[i]
			if strings.Contains(res.Name, "/") {
				continue // subresource (e.g. pods/status)
			}
			if !hasVerb(res.Verbs, "list") || !hasVerb(res.Verbs, "get") {
				continue
			}
			gvrs = append(gvrs, schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: res.Name})
		}
	}
	sort.Slice(gvrs, func(i, j int) bool {
		if gvrs[i].Group != gvrs[j].Group {
			return gvrs[i].Group < gvrs[j].Group
		}
		if gvrs[i].Version != gvrs[j].Version {
			return gvrs[i].Version < gvrs[j].Version
		}
		return gvrs[i].Resource < gvrs[j].Resource
	})
	return gvrs, unreadable, nil
}

// ShouldIncludeNamespaceObject is the single inclusion rule for namespace snapshot capture (Stage A).
// Default-include: an object enters the snapshot unless a provable exclusion signal applies.
//
// Exclusion signals:
//   - controller ownerReference (derived/runtime state recreated by its owner after restore);
//   - control-plane noise denylist (regenerated by controllers, not user desired-state);
//   - snapshot machinery (mechanism 1: kinds the snapshotter creates itself, from snapshotKinds + CSI);
//   - own state-snapshotter machinery (request/transfer objects).
func ShouldIncludeNamespaceObject(u *unstructured.Unstructured, snapshotKinds SnapshotMachineryGVKs) bool {
	if hasControllerOwnerReference(u) {
		return false
	}
	gvk := u.GroupVersionKind()
	if isControlPlaneNoise(u, gvk) {
		return false
	}
	if isSnapshotMachineryKind(gvk, snapshotKinds) {
		return false
	}
	if isOwnMachineryKind(gvk) {
		return false
	}
	return true
}

// isControlPlaneNoise reports control-plane-regenerated objects that are never user desired-state.
// Both kind-level (whole type) and object-level (specific well-known names/types) signals are used.
func isControlPlaneNoise(u *unstructured.Unstructured, gvk schema.GroupVersionKind) bool {
	switch gvk.Group {
	case "":
		switch gvk.Kind {
		case "Event":
			return true
		case "Endpoints":
			return true
		case "ConfigMap":
			return u.GetName() == "kube-root-ca.crt"
		case "ServiceAccount":
			return u.GetName() == "default"
		case "Secret":
			return unstructuredSecretType(u) == "kubernetes.io/service-account-token"
		}
	case "events.k8s.io":
		return gvk.Kind == "Event"
	case "coordination.k8s.io":
		return gvk.Kind == "Lease"
	}
	return false
}

func unstructuredSecretType(u *unstructured.Unstructured) string {
	t, _, _ := unstructured.NestedString(u.Object, "type")
	return t
}

// isSnapshotMachineryKind reports kinds created by the snapshotter itself (mechanism 1): the live
// snapshot-kind set plus the CSI VolumeSnapshot data leg (which the orphan-PVC path creates).
func isSnapshotMachineryKind(gvk schema.GroupVersionKind, snapshotKinds SnapshotMachineryGVKs) bool {
	if gvk.Group == csiSnapshotGroup && gvk.Kind == kindVolumeSnapshot {
		return true
	}
	return snapshotKinds.containsGroupKind(gvk.GroupKind())
}

// isOwnMachineryKind reports state-snapshotter request/transfer objects that discovery enumerates in
// the namespace. Snapshot (storage.deckhouse.io) is handled by mechanism 1, not here.
func isOwnMachineryKind(gvk schema.GroupVersionKind) bool {
	if gvk.Group == ownMachineryGroupStateSnapshotter {
		return true
	}
	if gvk.Group == ownMachineryGroupStorage {
		_, ok := storageMachineryKinds[gvk.Kind]
		return ok
	}
	return false
}

// csiSnapshotGroup / kindVolumeSnapshot mirror pkg/snapshot constants without importing it (cycle-free).
const (
	csiSnapshotGroup   = "snapshot.storage.k8s.io"
	kindVolumeSnapshot = "VolumeSnapshot"
)

func hasVerb(verbs metav1.Verbs, want string) bool {
	for _, v := range verbs {
		if v == want {
			return true
		}
	}
	return false
}

// hasControllerOwnerReference reports whether the object is a dependent managed by a controller
// (it carries an ownerReference with controller=true). Such objects are recreated by their owning
// controller and MUST NOT enter the snapshot manifest inventory. Objects created directly by a user
// (no controller owner) are kept.
func hasControllerOwnerReference(u *unstructured.Unstructured) bool {
	for _, ref := range u.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

func sortManifestTargets(targets []ManifestTarget) {
	sort.Slice(targets, func(i, j int) bool {
		a, b := targets[i], targets[j]
		if a.APIVersion != b.APIVersion {
			return a.APIVersion < b.APIVersion
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
}

// ManifestTarget is a minimal capture target (mirrors api/v1alpha1.ManifestTarget for the controller layer).
type ManifestTarget struct {
	APIVersion string
	Kind       string
	Name       string
}

func objectKey(u *unstructured.Unstructured) string {
	return fmt.Sprintf("%s|%s|%s|%s", u.GetAPIVersion(), u.GetKind(), u.GetNamespace(), u.GetName())
}

func isDiscoveryNotFound(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsNotFound(err) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "could not find the requested resource") ||
		strings.Contains(s, "the server could not find the requested resource")
}
