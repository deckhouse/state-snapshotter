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

package v1alpha1

import (
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestNamespaceSnapshot_NoLegacyContentField(t *testing.T) {
	ns := NamespaceSnapshot{}

	data, err := json.Marshal(ns.Status)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := m["boundSnapshotContentName"]; ok {
		t.Fatal("legacy field boundSnapshotContentName must not exist on NamespaceSnapshot status JSON")
	}
}

func TestNamespaceSnapshotContent_Registered(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	gvk := schema.GroupVersionKind{
		Group:   APIGroup,
		Version: APIVersion,
		Kind:    "NamespaceSnapshotContent",
	}

	if _, err := scheme.New(gvk); err != nil {
		t.Fatalf("scheme.New NamespaceSnapshotContent: %v", err)
	}
}
