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

package snapshot

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GVKRegistry provides mapping between Snapshot and SnapshotContent GVKs.
//
// This is a centralized source of truth for GVK resolution in dynamic controllers.
//
// IMPORTANT: Interface Stability Contract
//
// This interface is a formal contract defined in unified-snapshots-test-plan.md.
// The public methods (Register*, Resolve*) MUST NOT be changed without updating:
//   - unified-snapshots-test-plan.md (PACKAGE INTERFACES section)
//
// Contract Rules:
//   - Registration MUST be idempotent
//   - Resolution MUST be deterministic
//   - Errors MUST be informative
//
// See: unified-snapshots-test-plan.md (INTERFACE: pkg/snapshot.GVKRegistry)
type GVKRegistry struct {
	// snapshotGVKs maps snapshot Kind -> GVK (Kind must be globally unique across groups/versions)
	// This is a strict contract of the unified snapshot mechanism.
	snapshotGVKs map[string]schema.GroupVersionKind
	// contentGVKs maps content Kind -> GVK (Kind must be globally unique across groups/versions)
	// This follows the same uniqueness contract as snapshot Kind.
	contentGVKs map[string]schema.GroupVersionKind
	// snapshotKindByContentGroupKind maps "group/kind" (content) -> snapshot Kind
	snapshotKindByContentGroupKind map[string]string
	// contentGVKBySnapshotKind maps snapshot Kind -> explicit content GVK
	contentGVKBySnapshotKind map[string]schema.GroupVersionKind
}

// NewGVKRegistry creates a new GVK registry.
func NewGVKRegistry() *GVKRegistry {
	return &GVKRegistry{
		snapshotGVKs:                   make(map[string]schema.GroupVersionKind),
		contentGVKs:                    make(map[string]schema.GroupVersionKind),
		snapshotKindByContentGroupKind: make(map[string]string),
		contentGVKBySnapshotKind:       make(map[string]schema.GroupVersionKind),
	}
}

// RegisterSnapshotGVK registers a Snapshot GVK.
//
// Contract: Idempotent - registering the same GVK twice is allowed and has no effect.
// Example: RegisterSnapshotGVK("VirtualMachineSnapshot", "virtualization.deckhouse.io/v1alpha1")
//
// See: unified-snapshots-test-plan.md (TEST CASE: RegisterSnapshotGVK - Idempotency)
func (r *GVKRegistry) RegisterSnapshotGVK(kind string, apiVersion string) error {
	gvk := parseGVK(kind, apiVersion)
	if existing, ok := r.snapshotGVKs[kind]; ok {
		if existing.GroupVersion().String() != gvk.GroupVersion().String() {
			return fmt.Errorf("Snapshot Kind %q is already registered for %s; cannot register %s",
				kind, existing.GroupVersion().String(), gvk.GroupVersion().String())
		}
		return nil
	}
	r.snapshotGVKs[kind] = gvk
	return nil
}

// RegisterSnapshotContentGVK registers a SnapshotContent GVK.
//
// Contract: Idempotent - registering the same GVK twice is allowed and has no effect.
// Example: RegisterSnapshotContentGVK("VirtualMachineSnapshotContent", "virtualization.deckhouse.io/v1alpha1")
func (r *GVKRegistry) RegisterSnapshotContentGVK(kind string, apiVersion string) error {
	gvk := parseGVK(kind, apiVersion)
	if existing, ok := r.contentGVKs[kind]; ok {
		if existing.GroupVersion().String() != gvk.GroupVersion().String() {
			return fmt.Errorf("SnapshotContent Kind %q is already registered for %s; cannot register %s",
				kind, existing.GroupVersion().String(), gvk.GroupVersion().String())
		}
		return nil
	}
	r.contentGVKs[kind] = gvk
	r.registerDefaultContentMapping(gvk)
	return nil
}

// RegisterSnapshotContentMapping registers an explicit mapping between Snapshot and SnapshotContent GVKs.
// This is the escape hatch for cases where Content Kind is not SnapshotKind+"Content".
func (r *GVKRegistry) RegisterSnapshotContentMapping(snapshotKind, snapshotAPIVersion, contentKind, contentAPIVersion string) error {
	if err := r.RegisterSnapshotGVK(snapshotKind, snapshotAPIVersion); err != nil {
		return err
	}
	if err := r.RegisterSnapshotContentGVK(contentKind, contentAPIVersion); err != nil {
		return err
	}

	contentGVK := parseGVK(contentKind, contentAPIVersion)

	if existing, ok := r.contentGVKBySnapshotKind[snapshotKind]; ok {
		if existing != contentGVK {
			return fmt.Errorf("Snapshot Kind %q already mapped to Content %s; cannot map to %s",
				snapshotKind, existing.String(), contentGVK.String())
		}
	} else {
		r.contentGVKBySnapshotKind[snapshotKind] = contentGVK
	}

	groupKind := groupKindKey(contentGVK)
	// Explicit mapping overrides any default mapping derived from "Content" suffix.
	r.snapshotKindByContentGroupKind[groupKind] = snapshotKind

	return nil
}

// ResolveSnapshotGVK resolves Snapshot GVK from Kind.
//
// Contract: Deterministic - same Kind always returns same GVK (if registered).
// Returns error if not found.
//
// See: unified-snapshots-test-plan.md (TEST CASE: ResolveSnapshotGVK - Unknown Kind Returns Error)
func (r *GVKRegistry) ResolveSnapshotGVK(kind string) (schema.GroupVersionKind, error) {
	gvk, ok := r.snapshotGVKs[kind]
	if !ok {
		return schema.GroupVersionKind{}, fmt.Errorf("Snapshot GVK not found for kind: %s", kind)
	}
	return gvk, nil
}

// ResolveSnapshotContentGVK resolves SnapshotContent GVK from Snapshot Kind.
//
// Derives Content GVK from Snapshot Kind (e.g., VirtualMachineSnapshot -> VirtualMachineSnapshotContent).
// Contract: Deterministic - same Snapshot Kind always returns same Content GVK.
//
// Fallback behavior:
//  1. Try to find registered Content GVK
//  2. If not found, derive from Snapshot GVK (add "Content" suffix)
func (r *GVKRegistry) ResolveSnapshotContentGVK(snapshotKind string) (schema.GroupVersionKind, error) {
	if mapped, ok := r.contentGVKBySnapshotKind[snapshotKind]; ok {
		return mapped, nil
	}
	// First, try to find Snapshot GVK
	snapshotGVK, err := r.ResolveSnapshotGVK(snapshotKind)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("failed to resolve Snapshot GVK: %w", err)
	}

	// Derive Content Kind from Snapshot Kind
	contentKind := snapshotKind + "Content"

	// Try to find registered Content GVK
	if contentGVK, ok := r.contentGVKs[contentKind]; ok {
		return contentGVK, nil
	}

	// Fallback: derive from Snapshot GVK
	return schema.GroupVersionKind{
		Group:   snapshotGVK.Group,
		Version: snapshotGVK.Version,
		Kind:    contentKind,
	}, nil
}

// ResolveSnapshotKindByContentGVK resolves Snapshot Kind from Content GVK.
// This is used for content -> snapshot mapping in watch handlers.
func (r *GVKRegistry) ResolveSnapshotKindByContentGVK(contentGVK schema.GroupVersionKind) (string, error) {
	groupKind := groupKindKey(contentGVK)
	if kind, ok := r.snapshotKindByContentGroupKind[groupKind]; ok {
		return kind, nil
	}

	// Fallback: derive by suffix if content kind ends with "Content"
	if strings.HasSuffix(contentGVK.Kind, "Content") {
		snapshotKind := strings.TrimSuffix(contentGVK.Kind, "Content")
		snapshotGVK, err := r.ResolveSnapshotGVK(snapshotKind)
		if err != nil {
			return "", fmt.Errorf("Snapshot Kind not registered for Content GVK: %s", contentGVK.String())
		}
		if snapshotGVK.Group != contentGVK.Group {
			return "", fmt.Errorf("Snapshot Kind %q registered in group %q does not match Content group %q",
				snapshotKind, snapshotGVK.Group, contentGVK.Group)
		}
		return snapshotKind, nil
	}

	return "", fmt.Errorf("Snapshot Kind not found for Content GVK: %s", contentGVK.String())
}

func (r *GVKRegistry) registerDefaultContentMapping(contentGVK schema.GroupVersionKind) {
	if !strings.HasSuffix(contentGVK.Kind, "Content") {
		return
	}
	snapshotKind := strings.TrimSuffix(contentGVK.Kind, "Content")
	if snapshotKind == "" {
		return
	}
	groupKind := groupKindKey(contentGVK)
	if existing, ok := r.snapshotKindByContentGroupKind[groupKind]; ok && existing != snapshotKind {
		return
	}
	r.snapshotKindByContentGroupKind[groupKind] = snapshotKind
}

func groupKindKey(gvk schema.GroupVersionKind) string {
	return fmt.Sprintf("%s/%s", gvk.Group, gvk.Kind)
}

// parseGVK parses GVK from kind and apiVersion
// apiVersion format: "group/version" or "version" (for core APIs)
func parseGVK(kind, apiVersion string) schema.GroupVersionKind {
	if idx := strings.Index(apiVersion, "/"); idx != -1 {
		return schema.GroupVersionKind{
			Group:   apiVersion[:idx],
			Version: apiVersion[idx+1:],
			Kind:    kind,
		}
	}
	// Core API (e.g., "v1")
	return schema.GroupVersionKind{
		Group:   "",
		Version: apiVersion,
		Kind:    kind,
	}
}
