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
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func encodeChunk(objects []map[string]interface{}) (string, string) {
	jsonData, _ := json.Marshal(objects)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(jsonData)
	_ = gz.Close()
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	hash := sha256.Sum256(buf.Bytes())
	return encoded, hex.EncodeToString(hash[:])
}

// TestBuildManifestsWithDataRestoration_NamespaceRootOrphanPVC exercises the full restore compiler
// chain (resolve run tree -> load MCP -> sanitize -> transform -> marshal) for a namespace root with
// a ConfigMap and an orphan PVC, restored into a different namespace.
func TestBuildManifestsWithDataRestoration_NamespaceRootOrphanPVC(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = ssv1alpha1.AddToScheme(scheme)
	_ = storagev1alpha1.AddToScheme(scheme)
	vsGVK := schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}
	scheme.AddKnownTypeWithName(vsGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: vsGVK.Group, Version: vsGVK.Version, Kind: "VolumeSnapshotList"}, &unstructured.UnstructuredList{})

	log, _ := logger.NewLogger("error")
	ctx := context.Background()

	// Variant A: the root MCP holds only the ConfigMap; the orphan PVC manifest lives on its own child
	// volume node's MCP (mcp-orphan), reachable via the VolumeSnapshot handle's boundSnapshotContentName.
	rootData, rootChecksum := encodeChunk([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "cfg", "namespace": "source-ns"}, "data": map[string]interface{}{"k": "v"}},
	})
	rootChunk := &ssv1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: "chunk-root-0"},
		Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: "mcp-root", Index: 0, Data: rootData, Checksum: rootChecksum, ObjectsCount: 1,
		},
	}
	rootMCP := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-root", UID: types.UID("uid-mcp-root")},
		Spec:       ssv1alpha1.ManifestCheckpointSpec{},
		Status: ssv1alpha1.ManifestCheckpointStatus{
			Chunks:       []ssv1alpha1.ChunkInfo{{Name: "chunk-root-0", Index: 0, Checksum: rootChecksum}},
			TotalObjects: 1,
		},
	}
	meta.SetStatusCondition(&rootMCP.Status.Conditions, metav1.Condition{
		Type: ssv1alpha1.ManifestCheckpointConditionTypeReady, Status: metav1.ConditionTrue,
		Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
	})

	orphanData, orphanChecksum := encodeChunk([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]interface{}{"name": "orphan", "namespace": "source-ns", "uid": "uid-orphan"}, "spec": map[string]interface{}{"accessModes": []interface{}{"ReadWriteOnce"}, "volumeName": "pv-x"}},
	})
	orphanChunk := &ssv1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: "chunk-orphan-0"},
		Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: "mcp-orphan", Index: 0, Data: orphanData, Checksum: orphanChecksum, ObjectsCount: 1,
		},
	}
	orphanMCP := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-orphan", UID: types.UID("uid-mcp-orphan")},
		Spec:       ssv1alpha1.ManifestCheckpointSpec{},
		Status: ssv1alpha1.ManifestCheckpointStatus{
			Chunks:       []ssv1alpha1.ChunkInfo{{Name: "chunk-orphan-0", Index: 0, Checksum: orphanChecksum}},
			TotalObjects: 1,
		},
	}
	meta.SetStatusCondition(&orphanMCP.Status.Conditions, metav1.Condition{
		Type: ssv1alpha1.ManifestCheckpointConditionTypeReady, Status: metav1.ConditionTrue,
		Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
	})

	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ManifestCheckpointName: "mcp-root",
		},
	}
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed"})

	orphanContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content-vol-orphan"},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: "snapshot.storage.k8s.io/v1",
				Kind:       "VolumeSnapshot",
				Namespace:  "source-ns",
				Name:       "vs-orphan",
			},
		},
		Status: storagev1alpha1.SnapshotContentStatus{
			ManifestCheckpointName: "mcp-orphan",
			Data: &storagev1alpha1.SnapshotDataBinding{
				Source:   storagev1alpha1.SnapshotSubjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "orphan", Namespace: "source-ns", UID: "uid-orphan"},
				Artifact: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-orphan"},
			},
		},
	}
	meta.SetStatusCondition(&orphanContent.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed"})

	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "source-ns"},
		Status: storagev1alpha1.SnapshotStatus{
			BoundSnapshotContentName: "root-content",
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{
				{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "vs-orphan"},
			},
		},
	}
	meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed"})

	vs := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata":   map[string]interface{}{"name": "vs-orphan", "namespace": "source-ns"},
		"status":     map[string]interface{}{"boundVolumeSnapshotContentName": "vsc-orphan", "boundSnapshotContentName": "root-content-vol-orphan", "readyToUse": true},
	}}

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(rootChunk, rootMCP, orphanChunk, orphanMCP, content, orphanContent, snap, vs).Build()
	arch := usecase.NewArchiveService(cl, cl, log)
	svc := NewService(cl, arch, nil, nil)

	out, err := svc.BuildManifestsWithDataRestoration(ctx, Options{
		SnapshotName: "snap", SnapshotNamespace: "source-ns", TargetNamespace: "restore-ns",
	})
	if err != nil {
		t.Fatalf("BuildManifestsWithDataRestoration: %v", err)
	}

	var objects []map[string]interface{}
	if err := json.Unmarshal(out, &objects); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(objects) != 2 {
		t.Fatalf("expected 2 objects (ConfigMap + PVC), got %d: %s", len(objects), string(out))
	}

	var foundPVC bool
	for _, obj := range objects {
		u := unstructured.Unstructured{Object: obj}
		switch u.GetKind() {
		case "VolumeSnapshot", "VolumeSnapshotContent", "VolumeRestoreRequest":
			t.Fatalf("control-plane kind %s must not be emitted", u.GetKind())
		}
		if u.GetNamespace() != "restore-ns" {
			t.Fatalf("%s namespace = %q, want restore-ns", u.GetKind(), u.GetNamespace())
		}
		if u.GetKind() == "PersistentVolumeClaim" {
			foundPVC = true
			name, _, _ := unstructured.NestedString(u.Object, "spec", "dataSourceRef", "name")
			if name != "vs-orphan" {
				t.Fatalf("PVC dataSourceRef.name = %q, want vs-orphan", name)
			}
			if _, found, _ := unstructured.NestedString(u.Object, "spec", "volumeName"); found {
				t.Fatal("PVC spec.volumeName must be stripped")
			}
		}
	}
	if !foundPVC {
		t.Fatal("expected restored PVC in output")
	}
}
