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

func TestSanitizeForRestore_StripsRestoreBreakingAnnotations(t *testing.T) {
	pvc := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":      "data",
			"namespace": "source-ns",
			"annotations": map[string]interface{}{
				"kubectl.kubernetes.io/last-applied-configuration": "{\"x\":1}",
				"pv.kubernetes.io/bind-completed":                  "yes",
				"pv.kubernetes.io/bound-by-controller":             "yes",
				"volume.kubernetes.io/selected-node":               "node-1",
				"app.kubernetes.io/name":                           "keep",
			},
		},
		"spec": map[string]interface{}{"accessModes": []interface{}{"ReadWriteOnce"}},
	}}
	out, keep := sanitizeForRestore(pvc, "restore-ns")
	if !keep {
		t.Fatal("PVC must be kept")
	}
	anns, _, _ := unstructured.NestedStringMap(out.Object, "metadata", "annotations")
	for _, k := range restoreBreakingAnnotations {
		if _, ok := anns[k]; ok {
			t.Fatalf("annotation %q must be stripped", k)
		}
	}
	if anns["app.kubernetes.io/name"] != "keep" {
		t.Fatal("unrelated annotation must be preserved")
	}
}

func TestSanitizeForRestore_DropsEmptyAnnotationsMap(t *testing.T) {
	cm := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      "cfg",
			"namespace": "source-ns",
			"annotations": map[string]interface{}{
				"kubectl.kubernetes.io/last-applied-configuration": "{}",
			},
		},
	}}
	out, _ := sanitizeForRestore(cm, "restore-ns")
	if _, found, _ := unstructured.NestedMap(out.Object, "metadata", "annotations"); found {
		t.Fatal("annotations map should be removed when it becomes empty")
	}
}

func TestSanitizeForRestore_StripsServiceNodePortAndLoadBalancerIP(t *testing.T) {
	svc := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   map[string]interface{}{"name": "web", "namespace": "source-ns"},
		"spec": map[string]interface{}{
			"type":           "LoadBalancer",
			"loadBalancerIP": "1.2.3.4",
			"ports": []interface{}{
				map[string]interface{}{"port": int64(80), "targetPort": int64(8080), "nodePort": int64(30080)},
				map[string]interface{}{"port": int64(443), "nodePort": int64(30443)},
			},
		},
	}}
	out, _ := sanitizeForRestore(svc, "restore-ns")
	if _, ok, _ := unstructured.NestedString(out.Object, "spec", "loadBalancerIP"); ok {
		t.Fatal("Service spec.loadBalancerIP must be stripped")
	}
	ports, _, _ := unstructured.NestedSlice(out.Object, "spec", "ports")
	if len(ports) != 2 {
		t.Fatalf("expected 2 ports preserved, got %d", len(ports))
	}
	for _, p := range ports {
		pm, _ := p.(map[string]interface{})
		if _, ok := pm["nodePort"]; ok {
			t.Fatal("Service spec.ports[].nodePort must be stripped")
		}
		if _, ok := pm["port"]; !ok {
			t.Fatal("Service spec.ports[].port must be preserved")
		}
	}
}

func TestSanitizeForRestore_RewritesRoleBindingServiceAccountNamespace(t *testing.T) {
	rb := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "RoleBinding",
		"metadata":   map[string]interface{}{"name": "rb", "namespace": "source-ns"},
		"roleRef":    map[string]interface{}{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "r"},
		"subjects": []interface{}{
			map[string]interface{}{"kind": "ServiceAccount", "name": "sa", "namespace": "source-ns"},
			map[string]interface{}{"kind": "User", "name": "alice", "apiGroup": "rbac.authorization.k8s.io"},
		},
	}}
	out, keep := sanitizeForRestore(rb, "restore-ns")
	if !keep {
		t.Fatal("RoleBinding must be kept")
	}
	subjects, _, _ := unstructured.NestedSlice(out.Object, "subjects")
	sa, _ := subjects[0].(map[string]interface{})
	if sa["namespace"] != "restore-ns" {
		t.Fatalf("ServiceAccount subject namespace = %v, want restore-ns", sa["namespace"])
	}
	user, _ := subjects[1].(map[string]interface{})
	if _, ok := user["namespace"]; ok {
		t.Fatal("User subject must not get a namespace")
	}
}
