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

package genericbinder

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func rootSnapshotFixture(spec map[string]interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
	obj.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))
	obj.SetNamespace("ns1")
	obj.SetName("root")
	return obj
}

// The root content takes the policy the root Snapshot's creator chose. Omitting the field must keep
// meaning Retain: that is what every capture tree was created with before the field existed.
func TestRootContentDeletionPolicy(t *testing.T) {
	domainRoot := &unstructured.Unstructured{Object: map[string]interface{}{
		// A domain kind with a field of the same name: only the core Snapshot's field is part of the contract.
		"spec": map[string]interface{}{"deletionPolicy": storagev1alpha1.SnapshotContentDeletionPolicyDelete},
	}}
	domainRoot.SetGroupVersionKind(domainSnapshotGVK)
	domainRoot.SetName("widget")

	tests := []struct {
		name string
		obj  *unstructured.Unstructured
		want string
	}{
		{name: "Snapshot without the field", obj: rootSnapshotFixture(map[string]interface{}{}), want: storagev1alpha1.SnapshotContentDeletionPolicyRetain},
		{name: "Snapshot, Capture, Retain", obj: rootSnapshotFixture(map[string]interface{}{"mode": "Capture", "deletionPolicy": "Retain"}), want: storagev1alpha1.SnapshotContentDeletionPolicyRetain},
		{name: "Snapshot, Capture, Delete", obj: rootSnapshotFixture(map[string]interface{}{"mode": "Capture", "deletionPolicy": "Delete"}), want: storagev1alpha1.SnapshotContentDeletionPolicyDelete},
		{name: "another root kind keeps Retain", obj: domainRoot, want: storagev1alpha1.SnapshotContentDeletionPolicyRetain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rootContentDeletionPolicy(tt.obj)
			if err != nil {
				t.Fatalf("rootContentDeletionPolicy: %v", err)
			}
			if got != tt.want {
				t.Fatalf("rootContentDeletionPolicy() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A spec the typed Snapshot cannot hold is an error, not a silent Retain: parking a tree its creator asked
// to have reclaimed is the one outcome this field exists to prevent.
func TestRootContentDeletionPolicyRefusesAnUnreadableSpec(t *testing.T) {
	obj := rootSnapshotFixture(map[string]interface{}{"deletionPolicy": int64(1)})
	if _, err := rootContentDeletionPolicy(obj); err == nil {
		t.Fatal("rootContentDeletionPolicy accepted a spec.deletionPolicy that is not a string")
	}
}

// Every child content copies the policy of its parent's content, so one choice on the root reaches the
// whole tree, orphan-PVC VolumeSnapshot nodes and domain children alike.
func TestChildContentDeletionPolicy(t *testing.T) {
	parent := func(policy string) *storagev1alpha1.SnapshotContent {
		return &storagev1alpha1.SnapshotContent{Spec: storagev1alpha1.SnapshotContentSpec{DeletionPolicy: policy}}
	}
	tests := []struct {
		name   string
		parent *storagev1alpha1.SnapshotContent
		want   string
	}{
		{name: "parent Delete", parent: parent(storagev1alpha1.SnapshotContentDeletionPolicyDelete), want: storagev1alpha1.SnapshotContentDeletionPolicyDelete},
		{name: "parent Retain", parent: parent(storagev1alpha1.SnapshotContentDeletionPolicyRetain), want: storagev1alpha1.SnapshotContentDeletionPolicyRetain},
		{name: "parent without the field", parent: parent(""), want: storagev1alpha1.SnapshotContentDeletionPolicyRetain},
		{name: "no parent content", parent: nil, want: storagev1alpha1.SnapshotContentDeletionPolicyRetain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := childContentDeletionPolicy(tt.parent); got != tt.want {
				t.Fatalf("childContentDeletionPolicy() = %q, want %q", got, tt.want)
			}
		})
	}
}
