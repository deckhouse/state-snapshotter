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
	"k8s.io/apimachinery/pkg/types"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
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

// ParseVolumeCaptureDataRefs reads status.dataRefs[] from a VCR object.
func ParseVolumeCaptureDataRefs(obj *unstructured.Unstructured) ([]vcpkg.DataBinding, error) {
	raw, found, err := unstructured.NestedSlice(obj.Object, "status", "dataRefs")
	if err != nil {
		return nil, err
	}
	if !found || len(raw) == 0 {
		return nil, nil
	}
	out := make([]vcpkg.DataBinding, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("status.dataRefs[%d]: expected object", i)
		}
		targetMap, _ := m["target"].(map[string]interface{})
		artifactMap, _ := m["artifact"].(map[string]interface{})
		out = append(out, vcpkg.DataBinding{
			TargetUID: nestedString(m, "targetUID"),
			Target: vcpkg.Target{
				UID:        nestedString(targetMap, "uid"),
				APIVersion: nestedString(targetMap, "apiVersion"),
				Kind:       nestedString(targetMap, "kind"),
				Name:       nestedString(targetMap, "name"),
				Namespace:  nestedString(targetMap, "namespace"),
			},
			Artifact: vcpkg.ArtifactRef{
				APIVersion: nestedString(artifactMap, "apiVersion"),
				Kind:       nestedString(artifactMap, "kind"),
				Name:       nestedString(artifactMap, "name"),
			},
		})
	}
	return out, nil
}

func parseReadyCondition(obj *unstructured.Unstructured) (status, reason, message string, ok bool) {
	raw, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return "", "", "", false
	}
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if nestedString(m, "type") != vcpkg.ConditionTypeReady {
			continue
		}
		return nestedString(m, "status"), nestedString(m, "reason"), nestedString(m, "message"), true
	}
	return "", "", "", false
}

func nestedString(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

// SnapshotDataBindingsFromVCRStatus maps parsed VCR dataRefs to SnapshotContent bindings.
func SnapshotDataBindingsFromVCRStatus(refs []vcpkg.DataBinding) []storagev1alpha1.SnapshotDataBinding {
	if len(refs) == 0 {
		return nil
	}
	out := make([]storagev1alpha1.SnapshotDataBinding, 0, len(refs))
	for _, ref := range refs {
		out = append(out, storagev1alpha1.SnapshotDataBinding{
			TargetUID: ref.TargetUID,
			Target: storagev1alpha1.SnapshotSubjectRef{
				APIVersion: ref.Target.APIVersion,
				Kind:       ref.Target.Kind,
				Name:       ref.Target.Name,
				Namespace:  ref.Target.Namespace,
				UID:        types.UID(ref.Target.UID),
			},
			Artifact: storagev1alpha1.SnapshotDataArtifactRef{
				APIVersion: ref.Artifact.APIVersion,
				Kind:       ref.Artifact.Kind,
				Name:       ref.Artifact.Name,
			},
		})
	}
	return out
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
