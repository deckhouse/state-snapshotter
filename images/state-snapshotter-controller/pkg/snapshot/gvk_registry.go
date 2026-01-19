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
	// snapshotGVKs maps snapshot Kind -> GVK
	snapshotGVKs map[string]schema.GroupVersionKind
	// contentGVKs maps content Kind -> GVK
	contentGVKs map[string]schema.GroupVersionKind
}

// NewGVKRegistry creates a new GVK registry.
func NewGVKRegistry() *GVKRegistry {
	return &GVKRegistry{
		snapshotGVKs: make(map[string]schema.GroupVersionKind),
		contentGVKs:  make(map[string]schema.GroupVersionKind),
	}
}

// RegisterSnapshotGVK registers a Snapshot GVK.
//
// Contract: Idempotent - registering the same GVK twice is allowed and has no effect.
// Example: RegisterSnapshotGVK("VirtualMachineSnapshot", "virtualization.deckhouse.io/v1alpha1")
//
// See: unified-snapshots-test-plan.md (TEST CASE: RegisterSnapshotGVK - Idempotency)
func (r *GVKRegistry) RegisterSnapshotGVK(kind string, apiVersion string) error {
	gvk, err := parseGVK(kind, apiVersion)
	if err != nil {
		return fmt.Errorf("failed to parse Snapshot GVK: %w", err)
	}
	r.snapshotGVKs[kind] = gvk
	return nil
}

// RegisterSnapshotContentGVK registers a SnapshotContent GVK.
//
// Contract: Idempotent - registering the same GVK twice is allowed and has no effect.
// Example: RegisterSnapshotContentGVK("VirtualMachineSnapshotContent", "virtualization.deckhouse.io/v1alpha1")
func (r *GVKRegistry) RegisterSnapshotContentGVK(kind string, apiVersion string) error {
	gvk, err := parseGVK(kind, apiVersion)
	if err != nil {
		return fmt.Errorf("failed to parse SnapshotContent GVK: %w", err)
	}
	r.contentGVKs[kind] = gvk
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

// parseGVK parses GVK from kind and apiVersion
// apiVersion format: "group/version" or "version" (for core APIs)
func parseGVK(kind, apiVersion string) (schema.GroupVersionKind, error) {
	var gvk schema.GroupVersionKind
	if idx := strings.Index(apiVersion, "/"); idx != -1 {
		gvk = schema.GroupVersionKind{
			Group:   apiVersion[:idx],
			Version: apiVersion[idx+1:],
			Kind:    kind,
		}
	} else {
		// Core API (e.g., "v1")
		gvk = schema.GroupVersionKind{
			Group:   "",
			Version: apiVersion,
			Kind:    kind,
		}
	}
	return gvk, nil
}
