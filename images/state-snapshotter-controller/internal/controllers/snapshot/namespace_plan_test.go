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

package snapshot

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snaphelpers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
)

// namespaceManifestSpec maps the namespace target set 1:1 into ManifestCaptureSpec.Targets (order
// preserved; the SDK dedups/sorts + merges owned-PVC later).
func TestNamespaceManifestSpec(t *testing.T) {
	in := []namespacemanifest.ManifestTarget{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-a"},
		{APIVersion: "apps/v1", Kind: "Deployment", Name: "dep-b"},
	}
	got := namespaceManifestSpec(in)
	if len(got.Targets) != 2 {
		t.Fatalf("Targets len = %d, want 2 (%+v)", len(got.Targets), got.Targets)
	}
	if got.Targets[0].APIVersion != "v1" || got.Targets[0].Kind != "ConfigMap" || got.Targets[0].Name != "cm-a" {
		t.Errorf("Targets[0] = %+v", got.Targets[0])
	}
	if got.Targets[1].APIVersion != "apps/v1" || got.Targets[1].Kind != "Deployment" || got.Targets[1].Name != "dep-b" {
		t.Errorf("Targets[1] = %+v", got.Targets[1])
	}
}

// An empty target set yields an empty (non-nil) slice — no accidental nil that a downstream marshaler
// might treat differently.
func TestNamespaceManifestSpec_Empty(t *testing.T) {
	got := namespaceManifestSpec(nil)
	if got.Targets == nil {
		t.Fatalf("Targets must be non-nil, got nil")
	}
	if len(got.Targets) != 0 {
		t.Errorf("Targets = %+v, want empty", got.Targets)
	}
}

// buildNamespaceChildSpec builds a child snapshot object carrying kind/name/namespace + the immutable
// spec.sourceRef, with the correct GVK and WITHOUT an owner reference (the SDK stamps adoption).
func TestBuildNamespaceChildSpec(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "sds-unified-snapshots-poc.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualMachineSnapshot"}
	src := controllercommon.SnapshotSourceIdentity{
		APIVersion: "sds-unified-snapshots-poc.deckhouse.io/v1alpha1",
		Kind:       "DemoVirtualMachine",
		Namespace:  "my-app",
		Name:       "vm-a",
	}
	spec := buildNamespaceChildSpec("my-app", "nss-child-abc", gvk, src)

	child, ok := spec.Object.(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("ChildSpec.Object is %T, want *unstructured.Unstructured", spec.Object)
	}
	if child.GroupVersionKind() != gvk {
		t.Errorf("GVK = %v, want %v", child.GroupVersionKind(), gvk)
	}
	if child.GetName() != "nss-child-abc" || child.GetNamespace() != "my-app" {
		t.Errorf("name/namespace = %s/%s, want nss-child-abc/my-app", child.GetNamespace(), child.GetName())
	}
	if len(child.GetOwnerReferences()) != 0 {
		t.Errorf("child must carry no owner reference (SDK adopts), got %+v", child.GetOwnerReferences())
	}
	ref, found, err := unstructured.NestedStringMap(child.Object, "spec", "sourceRef")
	if err != nil || !found {
		t.Fatalf("spec.sourceRef missing (found=%v err=%v)", found, err)
	}
	if ref["apiVersion"] != src.APIVersion || ref["kind"] != src.Kind || ref["name"] != src.Name {
		t.Errorf("spec.sourceRef = %+v, want {%s,%s,%s}", ref, src.APIVersion, src.Kind, src.Name)
	}
	// The child sourceRef never carries a namespace (namespace is implicit = parent's).
	if _, hasNS := ref["namespace"]; hasNS {
		t.Errorf("spec.sourceRef must not carry a namespace, got %+v", ref)
	}
}
