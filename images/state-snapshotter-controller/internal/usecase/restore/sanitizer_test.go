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

func TestSanitizeForRestore_StripsRuntimeAndRewritesNamespace(t *testing.T) {
	cm := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":              "app-config",
			"namespace":         "source-ns",
			"uid":               "uid-1",
			"resourceVersion":   "123",
			"generation":        int64(2),
			"creationTimestamp": "2026-01-01T00:00:00Z",
			"managedFields":     []interface{}{map[string]interface{}{"manager": "kubectl"}},
			"ownerReferences":   []interface{}{map[string]interface{}{"kind": "Foo"}},
			"finalizers":        []interface{}{"x"},
		},
		"data":   map[string]interface{}{"k": "v"},
		"status": map[string]interface{}{"observed": true},
	}}

	out, keep := sanitizeForRestore(cm, "restore-ns")
	if !keep {
		t.Fatal("expected ConfigMap to be kept")
	}
	if out.GetNamespace() != "restore-ns" {
		t.Fatalf("namespace = %q, want restore-ns", out.GetNamespace())
	}
	meta, _ := out.Object["metadata"].(map[string]interface{})
	for _, f := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields", "ownerReferences", "finalizers"} {
		if _, ok := meta[f]; ok {
			t.Fatalf("metadata.%s should have been stripped", f)
		}
	}
	if _, ok := out.Object["status"]; ok {
		t.Fatal("status should have been stripped")
	}
	if data, _ := out.Object["data"].(map[string]interface{}); data["k"] != "v" {
		t.Fatal("data must be preserved")
	}
}

func TestSanitizeForRestore_DropsClusterScoped(t *testing.T) {
	clusterRole := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRole",
		"metadata":   map[string]interface{}{"name": "viewer"},
	}}
	if _, keep := sanitizeForRestore(clusterRole, "restore-ns"); keep {
		t.Fatal("cluster-scoped object (no namespace) must be dropped")
	}
}

func TestSanitizeForRestore_DropsControlPlaneKinds(t *testing.T) {
	for _, kind := range []string{"VolumeSnapshot", "VolumeSnapshotContent", "VolumeRestoreRequest", "Snapshot", "SnapshotContent", "DemoVirtualDiskSnapshot"} {
		obj := unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "x/v1",
			"kind":       kind,
			"metadata":   map[string]interface{}{"name": "n", "namespace": "source-ns"},
		}}
		if _, keep := sanitizeForRestore(obj, "restore-ns"); keep {
			t.Fatalf("control-plane kind %q must be dropped", kind)
		}
	}
}

func TestSanitizeForRestore_StripsKindSpecificFields(t *testing.T) {
	pvc := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   map[string]interface{}{"name": "data", "namespace": "source-ns"},
		"spec": map[string]interface{}{
			"volumeName":    "pv-1",
			"dataSource":    map[string]interface{}{"kind": "VolumeSnapshot", "name": "old"},
			"dataSourceRef": map[string]interface{}{"kind": "VolumeSnapshot", "name": "old"},
			"accessModes":   []interface{}{"ReadWriteOnce"},
			"resources":     map[string]interface{}{"requests": map[string]interface{}{"storage": "1Gi"}},
		},
	}}
	out, keep := sanitizeForRestore(pvc, "restore-ns")
	if !keep {
		t.Fatal("PVC must be kept")
	}
	spec, _ := out.Object["spec"].(map[string]interface{})
	for _, f := range []string{"volumeName", "dataSource", "dataSourceRef"} {
		if _, ok := spec[f]; ok {
			t.Fatalf("PVC spec.%s should have been stripped", f)
		}
	}
	if _, ok := spec["accessModes"]; !ok {
		t.Fatal("PVC spec.accessModes must be preserved")
	}

	svc := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   map[string]interface{}{"name": "web", "namespace": "source-ns"},
		"spec": map[string]interface{}{
			"clusterIP":      "10.0.0.1",
			"clusterIPs":     []interface{}{"10.0.0.1"},
			"ipFamilies":     []interface{}{"IPv4"},
			"ipFamilyPolicy": "SingleStack",
			"selector":       map[string]interface{}{"app": "web"},
		},
	}}
	svcOut, _ := sanitizeForRestore(svc, "restore-ns")
	svcSpec, _ := svcOut.Object["spec"].(map[string]interface{})
	for _, f := range []string{"clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy"} {
		if _, ok := svcSpec[f]; ok {
			t.Fatalf("Service spec.%s should have been stripped", f)
		}
	}
	if _, ok := svcSpec["selector"]; !ok {
		t.Fatal("Service spec.selector must be preserved")
	}
}
