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

package restore

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestSanitizeForRestore_PreservesFinalizersStripsOwnerRefs pins the domain-controller sanitizer to the
// same restore policy as the core sanitizer: finalizers are preserved (intent), while ownerReferences,
// uid, and status are stripped (apply-ready). This copy had no tests before, so its behavior would
// otherwise be unpinned.
func TestSanitizeForRestore_PreservesFinalizersStripsOwnerRefs(t *testing.T) {
	cm := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":            "app-config",
			"namespace":       "source-ns",
			"uid":             "uid-1",
			"ownerReferences": []interface{}{map[string]interface{}{"kind": "Foo", "name": "owner", "uid": "owner-uid"}},
			"finalizers":      []interface{}{"kubernetes.io/pvc-protection", "custom.io/keep"},
		},
		"data":   map[string]interface{}{"k": "v"},
		"status": map[string]interface{}{"observed": true},
	}}

	out, keep := SanitizeForRestore(cm, "restore-ns")
	if !keep {
		t.Fatal("expected ConfigMap to be kept")
	}
	if out.GetNamespace() != "restore-ns" {
		t.Fatalf("namespace = %q, want restore-ns", out.GetNamespace())
	}

	meta, _ := out.Object["metadata"].(map[string]interface{})
	if meta == nil {
		t.Fatal("metadata missing")
	}
	for _, f := range []string{"uid", "ownerReferences"} {
		if _, ok := meta[f]; ok {
			t.Fatalf("metadata.%s should have been stripped", f)
		}
	}
	if _, ok := out.Object["status"]; ok {
		t.Fatal("status should have been stripped")
	}

	fin, found, _ := unstructured.NestedStringSlice(out.Object, "metadata", "finalizers")
	if !found {
		t.Fatal("finalizers must be preserved on restore, not stripped")
	}
	wantFin := map[string]bool{"kubernetes.io/pvc-protection": true, "custom.io/keep": true}
	if len(fin) != len(wantFin) {
		t.Fatalf("finalizers = %v, want %v", fin, wantFin)
	}
	for _, f := range fin {
		if !wantFin[f] {
			t.Fatalf("unexpected finalizer %q (all captured finalizers must be preserved)", f)
		}
	}
}
