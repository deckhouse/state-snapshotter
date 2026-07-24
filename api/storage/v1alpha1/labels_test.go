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

package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDeleteProtectedKeyValue(t *testing.T) {
	if LabelDeleteProtected != "state-snapshotter.deckhouse.io/delete-protected" {
		t.Fatalf("unexpected key: %s", LabelDeleteProtected)
	}
	if LabelDeleteProtectedValue != "true" {
		t.Fatalf("unexpected value: %s", LabelDeleteProtectedValue)
	}
}

func TestStampDeleteProtectedAllocatesAndIsIdempotent(t *testing.T) {
	obj := &metav1.ObjectMeta{} // nil labels
	StampDeleteProtected(obj)
	if !IsDeleteProtected(obj) {
		t.Fatalf("expected protected after stamp, labels=%#v", obj.GetLabels())
	}
	// Idempotent: second stamp keeps a single key with the same value and preserves siblings.
	obj.Labels["foo"] = "bar"
	StampDeleteProtected(obj)
	if !IsDeleteProtected(obj) || obj.Labels["foo"] != "bar" {
		t.Fatalf("stamp must be idempotent and preserve other labels, got %#v", obj.GetLabels())
	}
}

func TestIsDeleteProtectedRejectsOtherValues(t *testing.T) {
	obj := &metav1.ObjectMeta{Labels: map[string]string{LabelDeleteProtected: "false"}}
	if IsDeleteProtected(obj) {
		t.Fatalf("value 'false' must not count as protected")
	}
	empty := &metav1.ObjectMeta{}
	if IsDeleteProtected(empty) {
		t.Fatalf("absent label must not count as protected")
	}
}
