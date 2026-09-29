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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

const (
	projTestMCRName = "mcr-1"
	projTestMCPName = "mcp-1"
	projTestMCPAlt  = "mcp-2"
)

func captureManifestProjScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add snapshotter scheme: %v", err)
	}
	return scheme
}

func captureOwnerWithMCR(mcrName string) *unstructured.Unstructured {
	owner := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "Snapshot",
		"metadata":   map[string]interface{}{"namespace": projTestNS, "name": "snap"},
	}}
	if mcrName != "" {
		_ = unstructured.SetNestedField(owner.Object, mcrName, "status", "captureState", "domainSpecificController", "manifestCaptureRequestName")
	}
	return owner
}

func contentObjWithMCP(name string) *unstructured.Unstructured {
	o := projContentObj()
	if name != "" {
		_ = unstructured.SetNestedField(o.Object, name, "status", "manifestCheckpointName")
	}
	return o
}

func TestReconcileManifestCheckpointNameProjection_FirstPublish(t *testing.T) {
	ctx := context.Background()
	scheme := captureManifestProjScheme(t)
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: projTestContent}}
	mcr := &ssv1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: projTestNS, Name: projTestMCRName},
		Status:     ssv1alpha1.ManifestCaptureRequestStatus{CheckpointName: projTestMCPName},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(content, mcr).
		Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	requeue, err := r.reconcileManifestCheckpointNameProjection(ctx, contentObjWithMCP(""), captureOwnerWithMCR(projTestMCRName), projTestNS, true)
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if requeue {
		t.Fatalf("first publish must not requeue")
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: projTestContent}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.ManifestCheckpointName != projTestMCPName {
		t.Fatalf("first publish got %q want %q", got.Status.ManifestCheckpointName, projTestMCPName)
	}
}

func TestReconcileManifestCheckpointNameProjection_HoldsPublishedNameOnNewMCR(t *testing.T) {
	ctx := context.Background()
	scheme := captureManifestProjScheme(t)
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: projTestContent},
		Status:     storagev1alpha1.SnapshotContentStatus{ManifestCheckpointName: projTestMCPName},
	}
	mcr := &ssv1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: projTestNS, Name: projTestMCRName},
		Status:     ssv1alpha1.ManifestCaptureRequestStatus{CheckpointName: projTestMCPAlt},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(content, mcr).
		Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	requeue, err := r.reconcileManifestCheckpointNameProjection(ctx, contentObjWithMCP(projTestMCPName), captureOwnerWithMCR(projTestMCRName), projTestNS, true)
	if err != nil {
		t.Fatalf("divergence must not abort reconcile, got %v", err)
	}
	if requeue {
		t.Fatalf("holding a published pointer must not requeue")
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: projTestContent}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.ManifestCheckpointName != projTestMCPName {
		t.Fatalf("published pointer must not move, got %q want %q", got.Status.ManifestCheckpointName, projTestMCPName)
	}
}
