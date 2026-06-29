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

package usecase

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// Shared test fixtures for the manifest read use cases (per_cr_manifests_test.go, import_upload_test.go,
// root_capture_run_exclude_test.go). These build ManifestCheckpoints/chunks, SnapshotContents,
// Snapshots, and the retained-content ObjectKeeper used to resolve a Snapshot to its root content.

const (
	aggManifestTestSnapNamespace = "ns1"
	aggManifestTestSnapName      = "snap"
)

func aggManifestTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := ssv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := storagev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme storage: %v", err)
	}
	if err := deckhousev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme deckhouse: %v", err)
	}
	return s
}

func aggManifestEncodeChunk(objects []map[string]interface{}) (data string, checksum string) {
	jsonData, _ := json.Marshal(objects)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(jsonData)
	_ = gz.Close()
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	hash := sha256.Sum256(buf.Bytes())
	return encoded, hex.EncodeToString(hash[:])
}

func aggManifestCreateChunk(name, cpName string, data, checksum string) *ssv1alpha1.ManifestCheckpointContentChunk {
	return &ssv1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: cpName,
			Index:          0,
			Data:           data,
			Checksum:       checksum,
			ObjectsCount:   1,
		},
	}
}

func aggManifestReadyMCP(name, srcNS string, chunks []ssv1alpha1.ChunkInfo, totalObj int) *ssv1alpha1.ManifestCheckpoint {
	cp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
		Spec: ssv1alpha1.ManifestCheckpointSpec{
			SourceNamespace: srcNS,
		},
		Status: ssv1alpha1.ManifestCheckpointStatus{
			Chunks:       chunks,
			TotalObjects: totalObj,
		},
	}
	meta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:   ssv1alpha1.ManifestCheckpointConditionTypeReady,
		Status: metav1.ConditionTrue,
		Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
	})
	return cp
}

func aggManifestNotReadyMCP(name, srcNS string, chunks []ssv1alpha1.ChunkInfo, totalObj int) *ssv1alpha1.ManifestCheckpoint {
	cp := aggManifestReadyMCP(name, srcNS, chunks, totalObj)
	meta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:   ssv1alpha1.ManifestCheckpointConditionTypeReady,
		Status: metav1.ConditionFalse,
		Reason: "Pending",
	})
	return cp
}

func aggManifestContent(name, mcpName string, children ...string) *storagev1alpha1.SnapshotContent {
	var refs []storagev1alpha1.SnapshotContentChildRef
	for _, c := range children {
		refs = append(refs, storagev1alpha1.SnapshotContentChildRef{Name: c})
	}
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: storagev1alpha1.SnapshotContentSpec{
			DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
		},
		Status: storagev1alpha1.SnapshotContentStatus{
			ManifestCheckpointName:      mcpName,
			ChildrenSnapshotContentRefs: refs,
		},
	}
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{
		Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed",
	})
	return content
}

func aggManifestNS(bound string) *storagev1alpha1.Snapshot {
	return &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: aggManifestTestSnapName, Namespace: aggManifestTestSnapNamespace},
		Status: storagev1alpha1.SnapshotStatus{
			BoundSnapshotContentName: bound,
		},
	}
}

func aggManifestRootOK(namespace, snapshotName string) *deckhousev1alpha1.ObjectKeeper {
	return &deckhousev1alpha1.ObjectKeeper{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ret-snap-" + namespace + "-" + snapshotName,
			UID:  types.UID("ok-uid-" + namespace + "-" + snapshotName),
		},
		Spec: deckhousev1alpha1.ObjectKeeperSpec{
			Mode: "FollowObjectWithTTL",
			FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "Snapshot",
				Namespace:  namespace,
				Name:       snapshotName,
				UID:        "snapshot-uid",
			},
		},
	}
}

func aggManifestOwnContentByOK(content *storagev1alpha1.SnapshotContent, okObj *deckhousev1alpha1.ObjectKeeper) {
	controller := true
	content.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "deckhouse.io/v1alpha1",
		Kind:       "ObjectKeeper",
		Name:       okObj.Name,
		UID:        okObj.UID,
		Controller: &controller,
	}}
}
