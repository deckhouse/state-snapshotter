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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestShouldSkipObject_PVCWithOwnerRef_ExplicitTargetNotSkipped(t *testing.T) {
	pvc := &unstructured.Unstructured{}
	pvc.SetAPIVersion("v1")
	pvc.SetKind("PersistentVolumeClaim")
	pvc.SetName("data")
	pvc.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "StatefulSet",
		Name:       "sts",
		UID:        "abc",
	}})
	if len(pvc.GetOwnerReferences()) == 0 {
		t.Fatal("test fixture: ownerReferences not set on unstructured PVC")
	}
	if !ShouldSkipObject(pvc, nil) {
		t.Fatal("PVC with ownerRef should be skipped without explicit target context")
	}
	if ShouldSkipObjectWithContext(pvc, nil, CaptureFilterContext{ExplicitTarget: true}) {
		t.Fatal("explicit MCR PVC target must not be skipped solely for ownerReferences")
	}
}

func TestShouldSkipObject_PVCExplicitTargetRequiresCoreV1APIVersion(t *testing.T) {
	pvc := &unstructured.Unstructured{}
	pvc.SetAPIVersion("storage.example.com/v1")
	pvc.SetKind("PersistentVolumeClaim")
	pvc.SetName("data")
	pvc.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "StatefulSet",
		Name:       "sts",
		UID:        "abc",
	}})
	if !ShouldSkipObjectWithContext(pvc, nil, CaptureFilterContext{ExplicitTarget: true}) {
		t.Fatal("non-core v1 PVC must not get scoped override even with ExplicitTarget")
	}
}

func TestShouldSkipObject_PVCStillSkippedForTmpPrefixWithExplicitTarget(t *testing.T) {
	pvc := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "PersistentVolumeClaim",
			"metadata": map[string]interface{}{
				"name": "tmp-de-foo",
			},
		},
	}
	if !ShouldSkipObjectWithContext(pvc, nil, CaptureFilterContext{ExplicitTarget: true}) {
		t.Fatal("tmp-de PVC must remain excluded even as explicit target")
	}
}

func TestShouldSkipObject_PodWithOwnerRefStillSkippedWithExplicitTarget(t *testing.T) {
	pod := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name": "p",
				"ownerReferences": []interface{}{
					map[string]interface{}{
						"apiVersion": "apps/v1",
						"kind":       "ReplicaSet",
						"name":       "rs",
						"uid":        "abc",
					},
				},
			},
		},
	}
	if !ShouldSkipObjectWithContext(pod, nil, CaptureFilterContext{ExplicitTarget: true}) {
		t.Fatal("Pod must stay skipped (ephemeralKinds) even with ExplicitTarget")
	}
}
