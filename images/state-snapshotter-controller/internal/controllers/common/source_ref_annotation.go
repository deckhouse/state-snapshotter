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

package common

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type SnapshotSourceIdentity struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

func SnapshotSourceIdentityFromObject(obj *unstructured.Unstructured) (SnapshotSourceIdentity, error) {
	identity := SnapshotSourceIdentity{
		APIVersion: obj.GroupVersionKind().GroupVersion().String(),
		Kind:       obj.GroupVersionKind().Kind,
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
		UID:        string(obj.GetUID()),
	}
	if err := identity.Validate(); err != nil {
		return SnapshotSourceIdentity{}, err
	}
	return identity, nil
}

func EncodeSnapshotSourceIdentity(identity SnapshotSourceIdentity) (string, error) {
	if err := identity.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func DecodeSnapshotSourceIdentityAnnotation(obj *unstructured.Unstructured) (SnapshotSourceIdentity, error) {
	return DecodeSnapshotSourceIdentityAnnotations(obj.GetAnnotations())
}

func DecodeSnapshotSourceIdentityAnnotations(annotations map[string]string) (SnapshotSourceIdentity, error) {
	value := annotations[AnnotationKeySourceRef]
	if value == "" {
		return SnapshotSourceIdentity{}, fmt.Errorf("missing %s annotation", AnnotationKeySourceRef)
	}
	var identity SnapshotSourceIdentity
	if err := json.Unmarshal([]byte(value), &identity); err != nil {
		return SnapshotSourceIdentity{}, fmt.Errorf("parse %s annotation: %w", AnnotationKeySourceRef, err)
	}
	if err := identity.Validate(); err != nil {
		return SnapshotSourceIdentity{}, fmt.Errorf("invalid %s annotation: %w", AnnotationKeySourceRef, err)
	}
	return identity, nil
}

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
	if i.UID == "" {
		return fmt.Errorf("uid is required")
	}
	return nil
}

func (i SnapshotSourceIdentity) GVK() (schema.GroupVersionKind, error) {
	gv, err := schema.ParseGroupVersion(i.APIVersion)
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	return gv.WithKind(i.Kind), nil
}

func (i SnapshotSourceIdentity) MatchesObject(obj *unstructured.Unstructured) bool {
	return i.APIVersion == obj.GroupVersionKind().GroupVersion().String() &&
		i.Kind == obj.GroupVersionKind().Kind &&
		i.Namespace == obj.GetNamespace() &&
		i.Name == obj.GetName() &&
		i.UID == string(obj.GetUID())
}

func (i SnapshotSourceIdentity) ObjectKey() types.NamespacedName {
	return types.NamespacedName{Namespace: i.Namespace, Name: i.Name}
}
