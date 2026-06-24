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

package volumecapture

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	vcpkg "github.com/deckhouse/state-snapshotter/images/domain-controller/pkg/volumecapture"
)

// NewVolumeCaptureRequestObject builds an unstructured VCR for create.
func NewVolumeCaptureRequestObject(namespace, name string, ownerRef metav1.OwnerReference, targets []vcpkg.Target) *unstructured.Unstructured {
	specTargets := make([]interface{}, 0, len(targets))
	for _, t := range targets {
		specTargets = append(specTargets, map[string]interface{}{
			"uid":        t.UID,
			"apiVersion": t.APIVersion,
			"kind":       t.Kind,
			"name":       t.Name,
			"namespace":  t.Namespace,
		})
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": vcpkg.VolumeCaptureRequestGVK.Group + "/" + vcpkg.VolumeCaptureRequestGVK.Version,
		"kind":       vcpkg.VolumeCaptureRequestGVK.Kind,
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
			"ownerReferences": []interface{}{
				ownerRefToMap(ownerRef),
			},
		},
		"spec": map[string]interface{}{
			"mode":    vcpkg.VolumeCaptureModeSnapshot,
			"targets": specTargets,
		},
	}}
	obj.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	return obj
}

func ownerRefToMap(ref metav1.OwnerReference) map[string]interface{} {
	m := map[string]interface{}{
		"apiVersion": ref.APIVersion,
		"kind":       ref.Kind,
		"name":       ref.Name,
		"uid":        string(ref.UID),
	}
	if ref.Controller != nil {
		m["controller"] = *ref.Controller
	}
	return m
}

// ParseVolumeCaptureTargets reads spec.targets[] from a VCR object.
func ParseVolumeCaptureTargets(obj *unstructured.Unstructured) ([]vcpkg.Target, error) {
	raw, found, err := unstructured.NestedSlice(obj.Object, "spec", "targets")
	if err != nil {
		return nil, err
	}
	if !found || len(raw) == 0 {
		return nil, nil
	}
	out := make([]vcpkg.Target, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("spec.targets[%d]: expected object", i)
		}
		out = append(out, vcpkg.Target{
			UID:        nestedString(m, "uid"),
			APIVersion: nestedString(m, "apiVersion"),
			Kind:       nestedString(m, "kind"),
			Name:       nestedString(m, "name"),
			Namespace:  nestedString(m, "namespace"),
		})
	}
	return out, nil
}

func nestedString(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

// VolumeCaptureTargetsEqual compares targets in uid order.
func VolumeCaptureTargetsEqual(a, b []vcpkg.Target) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]vcpkg.Target(nil), a...)
	bb := append([]vcpkg.Target(nil), b...)
	sortTargets(aa)
	sortTargets(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

func sortTargets(ts []vcpkg.Target) {
	for i := 0; i < len(ts); i++ {
		for j := i + 1; j < len(ts); j++ {
			if ts[j].UID < ts[i].UID {
				ts[i], ts[j] = ts[j], ts[i]
			}
		}
	}
}
