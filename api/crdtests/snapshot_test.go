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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

const (
	snapshotSpecImmutable     = `spec: Invalid value: "object": spec is immutable`
	snapshotResourceSelector  = "spec.resourceSelector is not supported: a Snapshot captures its whole namespace. Remove the field and do not use -l/--selector with d8 snapshot create; to keep objects out of a snapshot, label them state-snapshotter.deckhouse.io/exclude"
	snapshotBoundNameReplaced = "Snapshot status.boundSnapshotContentName is set-once: the bound SnapshotContent name cannot be replaced"
	snapshotBoundNameRemoved  = "Snapshot status.boundSnapshotContentName is set-once: the bound SnapshotContent name cannot be removed"
)

func snapshot(name string) *storagev1alpha1.Snapshot {
	return &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
	}
}

func readyCondition(status metav1.ConditionStatus) metav1.Condition {
	return metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             "Waiting",
		LastTransitionTime: metav1.Now(),
	}
}

// The removed spec.resourceSelector is never set through the Go type, so a
// Snapshot that carries it is written as unstructured or as a merge patch.
func snapshotWithSelector(name string, selector map[string]any) *unstructured.Unstructured {
	s := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"resourceSelector": selector},
	}}
	s.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind(storagev1alpha1.SnapshotKind))
	s.SetNamespace(testNamespace)
	s.SetName(name)
	return s
}

func TestSnapshotCreate(t *testing.T) {
	t.Run("Capture", func(t *testing.T) {
		create(t, snapshot("create-capture"))
	})
	t.Run("Import", func(t *testing.T) {
		s := snapshot("create-import")
		s.Spec.Mode = storagev1alpha1.SnapshotModeImport
		create(t, s)
	})
	for _, tt := range []struct {
		name     string
		selector map[string]any
	}{
		{name: "resourceSelector", selector: map[string]any{"matchLabels": map[string]any{"app": "db"}}},
		{name: "empty resourceSelector", selector: map[string]any{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := snapshotWithSelector(objectName("create", tt.name), tt.selector)
			err := k8sClient.Create(context.Background(), s)
			if err == nil {
				t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), s) })
			}
			expectAdmission(t, err, snapshotResourceSelector)
		})
	}
}

func TestSnapshotSpecUpdate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mutate  func(*storagev1alpha1.Snapshot)
		wantErr []string
	}{
		{
			name:    "mode",
			mutate:  func(s *storagev1alpha1.Snapshot) { s.Spec.Mode = storagev1alpha1.SnapshotModeImport },
			wantErr: []string{snapshotSpecImmutable},
		},
		{
			name:   "labels are mutable",
			mutate: func(s *storagev1alpha1.Snapshot) { s.Labels = map[string]string{"team": "storage"} },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := snapshot(objectName("spec", tt.name))
			create(t, s)
			change(t, s, writeObject, tt.mutate, tt.wantErr...)
		})
	}

	t.Run("resourceSelector added", func(t *testing.T) {
		s := snapshot("spec-resourceselector-added")
		create(t, s)
		patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"resourceSelector":{"matchLabels":{"app":"db"}}}}`))
		expectAdmission(t, k8sClient.Patch(context.Background(), s, patch), snapshotSpecImmutable, snapshotResourceSelector)
	})
}

func TestSnapshotBoundContentName(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mutate  func(*storagev1alpha1.Snapshot)
		wantErr []string
	}{
		{
			name:    "replaced",
			mutate:  func(s *storagev1alpha1.Snapshot) { s.Status.BoundSnapshotContentName = "content-b" },
			wantErr: []string{snapshotBoundNameReplaced},
		},
		{
			name:    "removed",
			mutate:  func(s *storagev1alpha1.Snapshot) { s.Status.BoundSnapshotContentName = "" },
			wantErr: []string{snapshotBoundNameRemoved},
		},
		{
			name: "the rest of status stays mutable",
			mutate: func(s *storagev1alpha1.Snapshot) {
				s.Status.Conditions = []metav1.Condition{readyCondition(metav1.ConditionTrue)}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := snapshot(objectName("bound", tt.name))
			create(t, s)
			s = change(t, s, writeStatus, func(s *storagev1alpha1.Snapshot) {
				s.Status.Conditions = []metav1.Condition{readyCondition(metav1.ConditionFalse)}
			})
			s = change(t, s, writeStatus, func(s *storagev1alpha1.Snapshot) { s.Status.BoundSnapshotContentName = "content-a" })
			change(t, s, writeStatus, tt.mutate, tt.wantErr...)
		})
	}
}
