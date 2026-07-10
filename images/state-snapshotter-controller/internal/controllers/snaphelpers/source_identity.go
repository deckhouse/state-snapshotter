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

package snaphelpers

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// SnapshotSourceIdentity is the namespace-local identity of a source object captured by a snapshot.
// The generic planner uses it to deduplicate coverage across the run tree. It is keyed by
// apiVersion/kind/namespace/name (name-only, no UID): a snapshot's spec.sourceRef is the
// source-of-truth and carries no UID, and namespace is implicit (the source object lives in the same
// namespace as the snapshot).
type SnapshotSourceIdentity struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
}

// SnapshotSourceIdentityFromObject derives the identity of a live source object from its standard
// fields (apiVersion/kind/namespace/name).
func SnapshotSourceIdentityFromObject(obj *unstructured.Unstructured) (SnapshotSourceIdentity, error) {
	identity := SnapshotSourceIdentity{
		APIVersion: obj.GroupVersionKind().GroupVersion().String(),
		Kind:       obj.GroupVersionKind().Kind,
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
	}
	if err := identity.Validate(); err != nil {
		return SnapshotSourceIdentity{}, err
	}
	return identity, nil
}

// Validate ensures all identity components are present.
func (i SnapshotSourceIdentity) Validate() error {
	if i.APIVersion == "" {
		return fmt.Errorf("apiVersion is required")
	}
	if i.Kind == "" {
		return fmt.Errorf("kind is required")
	}
	if i.Namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	if i.Name == "" {
		return fmt.Errorf("name is required")
	}
	return nil
}
