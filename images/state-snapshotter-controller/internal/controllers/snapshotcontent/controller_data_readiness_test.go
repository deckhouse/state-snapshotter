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

package snapshotcontent

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

const vscAPIVersion = "snapshot.storage.k8s.io/v1"

func TestResolveDataReadinessEmptyDataRefs(t *testing.T) {
	r, content := dataReadinessFixture(t)
	ready, reason, msg, err := r.resolveDataReadiness(context.Background(), content)
	if err != nil {
		t.Fatalf("resolveDataReadiness: %v", err)
	}
	if !ready || reason != "" || msg != "" {
		t.Fatalf("expected ready with empty dataRefs, got ready=%v reason=%q msg=%q", ready, reason, msg)
	}
}

func TestResolveDataReadinessVSCMissing(t *testing.T) {
	r, content := dataReadinessFixture(t, dataBinding("missing-vsc"))
	ready, reason, msg, err := r.resolveDataReadiness(context.Background(), content)
	if err != nil {
		t.Fatalf("resolveDataReadiness: %v", err)
	}
	if ready || reason != snapshot.ReasonArtifactMissing {
		t.Fatalf("expected ArtifactMissing, got ready=%v reason=%q msg=%q", ready, reason, msg)
	}
	if !strings.Contains(msg, "missing-vsc") {
		t.Fatalf("expected missing VSC name in message, got %q", msg)
	}
}

func TestResolveDataReadinessVSCNotReadyToUse(t *testing.T) {
	r, content := dataReadinessFixture(t,
		dataBinding("vsc-pending"),
		withVSC("vsc-pending", false),
	)
	ready, reason, msg, err := r.resolveDataReadiness(context.Background(), content)
	if err != nil {
		t.Fatalf("resolveDataReadiness: %v", err)
	}
	if ready || reason != snapshot.ReasonDataCapturePending {
		t.Fatalf("expected DataCapturePending, got ready=%v reason=%q msg=%q", ready, reason, msg)
	}
	if !strings.Contains(msg, "0/1 ready") || !strings.Contains(msg, "vsc-pending") {
		t.Fatalf("expected progress count and pending name in message, got %q", msg)
	}
}

// VSC exists but has no status.readyToUse field at all (e.g. freshly provisioned): treated the same
// as readyToUse=false -> DataCapturePending (pending, never terminal).
func TestResolveDataReadinessVSCReadyToUseMissing(t *testing.T) {
	r, content := dataReadinessFixture(t,
		dataBinding("vsc-noready"),
		withVSCNoReadyToUse("vsc-noready"),
	)
	ready, reason, msg, err := r.resolveDataReadiness(context.Background(), content)
	if err != nil {
		t.Fatalf("resolveDataReadiness: %v", err)
	}
	if ready || reason != snapshot.ReasonDataCapturePending {
		t.Fatalf("expected DataCapturePending for missing readyToUse, got ready=%v reason=%q msg=%q", ready, reason, msg)
	}
	if !strings.Contains(msg, "0/1 ready") || !strings.Contains(msg, "vsc-noready") {
		t.Fatalf("expected progress count and pending name in message, got %q", msg)
	}
}

func TestResolveDataReadinessOneVSCReady(t *testing.T) {
	r, content := dataReadinessFixture(t,
		dataBinding("vsc-ready"),
		withVSC("vsc-ready", true),
	)
	ready, reason, msg, err := r.resolveDataReadiness(context.Background(), content)
	if err != nil {
		t.Fatalf("resolveDataReadiness: %v", err)
	}
	if !ready || reason != "" || msg != "" {
		t.Fatalf("expected ready, got ready=%v reason=%q msg=%q", ready, reason, msg)
	}
}

// Variant A: a SnapshotContent carries ≤1 dataRef, so the multi-dataRef-on-one-content readiness cases
// (former TestResolveDataReadinessTwoVSC*) are no longer representable — multi-volume aggregation is
// covered by ChildrenReady over child volume content nodes, exercised in the content aggregation tests.

// A VSC that is being deleted (deletionTimestamp set) must not keep the parent Ready=True even though
// it still exists and reports readyToUse=true. Classified terminal ArtifactMissing (INV-FAIL-PROP).
func TestResolveDataReadinessVSCDeleting(t *testing.T) {
	r, content := dataReadinessFixture(t,
		dataBinding("vsc-deleting"),
		withVSCDeleting("vsc-deleting"),
	)
	ready, reason, msg, err := r.resolveDataReadiness(context.Background(), content)
	if err != nil {
		t.Fatalf("resolveDataReadiness: %v", err)
	}
	if ready || reason != snapshot.ReasonArtifactMissing {
		t.Fatalf("expected ArtifactMissing for deleting VSC, got ready=%v reason=%q msg=%q", ready, reason, msg)
	}
	if !strings.Contains(msg, "vsc-deleting") {
		t.Fatalf("expected deleting VSC name in message, got %q", msg)
	}
}

func TestResolveDataReadinessUnknownArtifactKind(t *testing.T) {
	binding := dataBinding("other-artifact")
	binding.Artifact.Kind = "UnknownVolumeBackend"
	r, content := dataReadinessFixture(t, binding)
	ready, reason, msg, err := r.resolveDataReadiness(context.Background(), content)
	if err != nil {
		t.Fatalf("resolveDataReadiness: %v", err)
	}
	if ready || reason != snapshot.ReasonDataArtifactNotSupported {
		t.Fatalf("expected DataArtifactNotSupported, got ready=%v reason=%q msg=%q", ready, reason, msg)
	}
	if !strings.Contains(msg, "UnknownVolumeBackend") || !strings.Contains(msg, "pvc-1") {
		t.Fatalf("expected kind and targetUID in message, got %q", msg)
	}
}

func TestResolveDataReadinessInvalidArtifactRef(t *testing.T) {
	binding := dataBinding("")
	binding.Artifact.Name = ""
	r, content := dataReadinessFixture(t, binding)
	ready, reason, msg, err := r.resolveDataReadiness(context.Background(), content)
	if err != nil {
		t.Fatalf("resolveDataReadiness: %v", err)
	}
	if ready || reason != snapshot.ReasonDataArtifactInvalid {
		t.Fatalf("expected DataArtifactInvalid, got ready=%v reason=%q msg=%q", ready, reason, msg)
	}
}

func TestResolveDataReadinessRejectsVolumeCaptureRequest(t *testing.T) {
	binding := dataBinding("vcr-1")
	binding.Artifact.Kind = "VolumeCaptureRequest"
	r, content := dataReadinessFixture(t, binding)
	ready, reason, _, err := r.resolveDataReadiness(context.Background(), content)
	if err != nil {
		t.Fatalf("resolveDataReadiness: %v", err)
	}
	if ready || reason != snapshot.ReasonDataArtifactNotSupported {
		t.Fatalf("expected DataArtifactNotSupported for VCR ref, got ready=%v reason=%q", ready, reason)
	}
}

type dataReadinessOption func(*dataReadinessFixtureState)

type dataReadinessFixtureState struct {
	bindings []snapshot.DataBindingRef
	vscs     []client.Object
}

func withVSC(name string, readyToUse bool) dataReadinessOption {
	return func(s *dataReadinessFixtureState) {
		s.vscs = append(s.vscs, volumeSnapshotContentObject(name, readyToUse))
	}
}

// withVSCNoReadyToUse adds a VolumeSnapshotContent that has no status.readyToUse field at all.
func withVSCNoReadyToUse(name string) dataReadinessOption {
	return func(s *dataReadinessFixtureState) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "snapshot.storage.k8s.io",
			Version: "v1",
			Kind:    kindVolumeSnapshotContent,
		})
		obj.SetName(name)
		s.vscs = append(s.vscs, obj)
	}
}

// withVSCDeleting adds a VolumeSnapshotContent that still exists and reports readyToUse=true but
// carries a deletionTimestamp (a finalizer is required by the fake client to keep it present). It must
// be classified ArtifactMissing — a deleting durable artifact is not healthy.
func withVSCDeleting(name string) dataReadinessOption {
	return func(s *dataReadinessFixtureState) {
		obj := volumeSnapshotContentObject(name, true)
		obj.SetFinalizers([]string{snapshot.FinalizerArtifactProtect})
		now := metav1.NewTime(time.Now())
		obj.SetDeletionTimestamp(&now)
		s.vscs = append(s.vscs, obj)
	}
}

func dataBinding(vscName string) snapshot.DataBindingRef {
	return snapshot.DataBindingRef{
		TargetUID: "pvc-1",
		Artifact: snapshot.ObjectRef{
			APIVersion: vscAPIVersion,
			Kind:       kindVolumeSnapshotContent,
			Name:       vscName,
		},
	}
}

func dataReadinessFixture(t *testing.T, opts ...interface{}) (*SnapshotContentController, *unstructured.Unstructured) {
	t.Helper()
	state := &dataReadinessFixtureState{}
	for _, opt := range opts {
		switch v := opt.(type) {
		case snapshot.DataBindingRef:
			state.bindings = append(state.bindings, v)
		case dataReadinessOption:
			v(state)
		default:
			t.Fatalf("unsupported fixture option %T", opt)
		}
	}

	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	objs := append([]client.Object{}, state.vscs...)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	content := commonSnapshotContentWithDataRef(state.bindings)
	return r, content
}

// commonSnapshotContentWithDataRef builds a SnapshotContent carrying at most one status.dataRef
// (Variant A: a node has ≤1 data artifact; multi-volume aggregation is via child content nodes, not a
// dataRefs[] list). A second binding cannot be represented on one content, so only the first is written.
func commonSnapshotContentWithDataRef(bindings []snapshot.DataBindingRef) *unstructured.Unstructured {
	status := map[string]interface{}{}
	if len(bindings) > 0 {
		b := bindings[0]
		status["data"] = map[string]interface{}{
			"sourceRef": map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "PersistentVolumeClaim",
				"name":       "pvc",
				"namespace":  "default",
				"uid":        b.TargetUID,
			},
			"artifactRef": map[string]interface{}{
				"apiVersion": b.Artifact.APIVersion,
				"kind":       b.Artifact.Kind,
				"name":       b.Artifact.Name,
			},
		}
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "SnapshotContent",
		"metadata": map[string]interface{}{
			"name": "content-data-readiness",
		},
		"status": status,
	}}
	obj.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	return obj
}

func volumeSnapshotContentObject(name string, readyToUse bool) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "snapshot.storage.k8s.io",
		Version: "v1",
		Kind:    kindVolumeSnapshotContent,
	})
	obj.SetName(name)
	if err := unstructured.SetNestedField(obj.Object, readyToUse, "status", "readyToUse"); err != nil {
		panic(err)
	}
	return obj
}
