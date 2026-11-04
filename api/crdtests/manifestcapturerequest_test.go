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

package crdtests

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

const (
	mcrTargetsRequired  = "spec.targets must list at least one object to capture (at minimum the snapshotted object's own manifest)"
	mcrTargetsImmutable = "spec.targets is immutable: the capture plan is frozen once the ManifestCaptureRequest is created"
)

func manifestCaptureRequest(name string, targets ...ssv1alpha1.ManifestTarget) *ssv1alpha1.ManifestCaptureRequest {
	return &ssv1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       ssv1alpha1.ManifestCaptureRequestSpec{Targets: targets},
	}
}

func configMapTarget(name string) ssv1alpha1.ManifestTarget {
	return ssv1alpha1.ManifestTarget{APIVersion: "v1", Kind: "ConfigMap", Name: name}
}

func TestManifestCaptureRequestCreate(t *testing.T) {
	t.Run("one target", func(t *testing.T) {
		create(t, manifestCaptureRequest("create-one-target", configMapTarget("a")))
	})
	t.Run("targets omitted", func(t *testing.T) {
		err := k8sClient.Create(context.Background(), manifestCaptureRequest("create-targets-omitted"))
		expectAdmission(t, err, mcrTargetsRequired)
	})
	// The typed client drops an empty list, so it is sent as unstructured.
	t.Run("targets empty", func(t *testing.T) {
		mcr := &unstructured.Unstructured{Object: map[string]any{
			"spec": map[string]any{"targets": []any{}},
		}}
		mcr.SetGroupVersionKind(ssv1alpha1.SchemeGroupVersion.WithKind("ManifestCaptureRequest"))
		mcr.SetNamespace(testNamespace)
		mcr.SetName("create-targets-empty")
		expectAdmission(t, k8sClient.Create(context.Background(), mcr), mcrTargetsRequired)
	})
}

func TestManifestCaptureRequestUpdate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		write   writer
		mutate  func(*ssv1alpha1.ManifestCaptureRequest)
		wantErr []string
	}{
		{
			name:    "target replaced",
			write:   writeObject,
			mutate:  func(m *ssv1alpha1.ManifestCaptureRequest) { m.Spec.Targets[0].Name = "b" },
			wantErr: []string{mcrTargetsImmutable},
		},
		{
			name:  "target appended",
			write: writeObject,
			mutate: func(m *ssv1alpha1.ManifestCaptureRequest) {
				m.Spec.Targets = append(m.Spec.Targets, configMapTarget("b"))
			},
			wantErr: []string{mcrTargetsImmutable},
		},
		{
			name:   "labels are mutable",
			write:  writeObject,
			mutate: func(m *ssv1alpha1.ManifestCaptureRequest) { m.Labels = map[string]string{"team": "storage"} },
		},
		{
			name:   "status is mutable",
			write:  writeStatus,
			mutate: func(m *ssv1alpha1.ManifestCaptureRequest) { m.Status.CheckpointName = "mcp" },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mcr := manifestCaptureRequest(objectName("update", tt.name), configMapTarget("a"))
			create(t, mcr)
			change(t, mcr, tt.write, tt.mutate, tt.wantErr...)
		})
	}
}
