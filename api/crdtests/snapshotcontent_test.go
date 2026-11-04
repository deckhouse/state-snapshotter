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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

const (
	contentSnapshotRefImmutable     = "SnapshotContent spec.snapshotRef is immutable until the bound Snapshot is deleted (recycle-bin restore)"
	contentDeletionPolicyImmutable  = "SnapshotContent spec.deletionPolicy is immutable"
	contentCheckpointReplaced       = "SnapshotContent status.manifestCheckpointName is set-once: the published ManifestCheckpoint name cannot be replaced"
	contentCheckpointRemoved        = "SnapshotContent status.manifestCheckpointName is set-once: a published ManifestCheckpoint name cannot be removed"
	contentDataRemoved              = "SnapshotContent status.data is set-once: a published data binding cannot be removed"
	contentArtifactIdentity         = "SnapshotContent status.data.artifactRef identity (apiVersion, kind, name) is set-once: a published data artifact cannot be replaced"
	contentArtifactUID              = "SnapshotContent status.data.artifactRef.uid is set-once: it may be filled in after the first publish, but not changed or removed"
	contentSourceIdentity           = "SnapshotContent status.data.sourceRef identity (apiVersion, kind, name) is set-once: the captured source of a published data binding cannot be replaced"
	contentSourceNamespace          = "SnapshotContent status.data.sourceRef.namespace is part of the set-once identity of a published data binding: it may not be changed or removed"
	contentSourceUID                = "SnapshotContent status.data.sourceRef.uid is set-once: it may be filled in after the first publish, but not changed or removed"
	contentChildrenRemoved          = "SnapshotContent status.childrenSnapshotContentRefs is frozen once set: a published child set cannot be removed"
	contentChildrenFrozen           = "childrenSnapshotContentRefs is frozen once set: the complete child SnapshotContent set is immutable (no add, remove, reorder, or replace)"
	contentChildrenFrozenOnTheField = `status.childrenSnapshotContentRefs: Invalid value: "array": ` + contentChildrenFrozen
)

func snapshotContent(name string) *storagev1alpha1.SnapshotContent {
	return &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: storagev1alpha1.SnapshotContentSpec{
			DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       storagev1alpha1.SnapshotKind,
				Name:       "snapshot",
				Namespace:  testNamespace,
				UID:        "snapshot-uid",
			},
		},
	}
}

func children(names ...string) []storagev1alpha1.SnapshotContentChildRef {
	refs := make([]storagev1alpha1.SnapshotContentChildRef, 0, len(names))
	for _, name := range names {
		refs = append(refs, storagev1alpha1.SnapshotContentChildRef{Name: name})
	}
	return refs
}

func TestSnapshotContentSpecUpdate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		create  func(*storagev1alpha1.SnapshotContent)
		mutate  func(*storagev1alpha1.SnapshotContent)
		wantErr []string
	}{
		{
			name:    "snapshotRef name",
			mutate:  func(c *storagev1alpha1.SnapshotContent) { c.Spec.SnapshotRef.Name = "other" },
			wantErr: []string{contentSnapshotRefImmutable},
		},
		{
			name:    "snapshotRef uid",
			mutate:  func(c *storagev1alpha1.SnapshotContent) { c.Spec.SnapshotRef.UID = "other-uid" },
			wantErr: []string{contentSnapshotRefImmutable},
		},
		{
			name: "deletionPolicy changed",
			mutate: func(c *storagev1alpha1.SnapshotContent) {
				c.Spec.DeletionPolicy = storagev1alpha1.SnapshotContentDeletionPolicyDelete
			},
			wantErr: []string{contentDeletionPolicyImmutable},
		},
		{
			name:    "deletionPolicy removed",
			mutate:  func(c *storagev1alpha1.SnapshotContent) { c.Spec.DeletionPolicy = "" },
			wantErr: []string{contentDeletionPolicyImmutable},
		},
		{
			name:   "deletionPolicy added",
			create: func(c *storagev1alpha1.SnapshotContent) { c.Spec.DeletionPolicy = "" },
			mutate: func(c *storagev1alpha1.SnapshotContent) {
				c.Spec.DeletionPolicy = storagev1alpha1.SnapshotContentDeletionPolicyRetain
			},
			wantErr: []string{contentDeletionPolicyImmutable},
		},
		{
			name:   "labels are mutable",
			mutate: func(c *storagev1alpha1.SnapshotContent) { c.Labels = map[string]string{"team": "storage"} },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := snapshotContent(objectName("spec", tt.name))
			if tt.create != nil {
				tt.create(c)
			}
			create(t, c)
			change(t, c, writeObject, tt.mutate, tt.wantErr...)
		})
	}
}

func TestSnapshotContentSnapshotRefAfterSnapshotDeleted(t *testing.T) {
	c := snapshotContent("snapshotref-recycle-bin")
	create(t, c)

	c = change(t, c, writeStatus, func(c *storagev1alpha1.SnapshotContent) { c.Status.BoundSnapshotDeleted = true })
	change(t, c, writeObject, func(c *storagev1alpha1.SnapshotContent) {
		c.Spec.SnapshotRef.Name = "snapshot-recreated"
		c.Spec.SnapshotRef.UID = "snapshot-recreated-uid"
	})
}

// publishedContent creates a SnapshotContent and writes status the way the
// controller publishes it, one admitted step after another, so that every case
// starts from its own object.
func publishedContent(t *testing.T, name string, publish ...func(*storagev1alpha1.SnapshotContent)) *storagev1alpha1.SnapshotContent {
	t.Helper()
	c := snapshotContent(name)
	create(t, c)
	for _, step := range publish {
		c = change(t, c, writeStatus, step)
	}
	return c
}

func TestSnapshotContentManifestCheckpointName(t *testing.T) {
	publish := func(c *storagev1alpha1.SnapshotContent) { c.Status.ManifestCheckpointName = "mcp-a" }
	for _, tt := range []struct {
		name    string
		mutate  func(*storagev1alpha1.SnapshotContent)
		wantErr []string
	}{
		{
			name:    "replaced",
			mutate:  func(c *storagev1alpha1.SnapshotContent) { c.Status.ManifestCheckpointName = "mcp-b" },
			wantErr: []string{contentCheckpointReplaced},
		},
		{
			name:    "removed",
			mutate:  func(c *storagev1alpha1.SnapshotContent) { c.Status.ManifestCheckpointName = "" },
			wantErr: []string{contentCheckpointRemoved},
		},
		{
			name:   "the rest of status stays mutable",
			mutate: func(c *storagev1alpha1.SnapshotContent) { c.Status.SubtreeManifestsPersisted = true },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := publishedContent(t, objectName("checkpoint", tt.name), publish)
			change(t, c, writeStatus, tt.mutate, tt.wantErr...)
		})
	}
}

func TestSnapshotContentData(t *testing.T) {
	publish := func(c *storagev1alpha1.SnapshotContent) {
		c.Status.Data = &storagev1alpha1.SnapshotDataBinding{
			SourceRef: storagev1alpha1.SnapshotSubjectRef{
				APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc",
			},
			ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
				APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc",
			},
		}
	}
	enrich := func(c *storagev1alpha1.SnapshotContent) {
		c.Status.Data.SourceRef.Namespace = "ns"
		c.Status.Data.SourceRef.UID = "pvc-uid"
		c.Status.Data.ArtifactRef.UID = "vsc-uid"
		c.Status.Data.Size = "1Gi"
	}

	for _, tt := range []struct {
		name    string
		mutate  func(*storagev1alpha1.SnapshotContent)
		wantErr []string
	}{
		{name: "artifactRef apiVersion", mutate: func(c *storagev1alpha1.SnapshotContent) { c.Status.Data.ArtifactRef.APIVersion = "v2" }, wantErr: []string{contentArtifactIdentity}},
		{name: "artifactRef kind", mutate: func(c *storagev1alpha1.SnapshotContent) { c.Status.Data.ArtifactRef.Kind = "Other" }, wantErr: []string{contentArtifactIdentity}},
		{name: "artifactRef name", mutate: func(c *storagev1alpha1.SnapshotContent) { c.Status.Data.ArtifactRef.Name = "other" }, wantErr: []string{contentArtifactIdentity}},
		{name: "artifactRef uid changed", mutate: func(c *storagev1alpha1.SnapshotContent) { c.Status.Data.ArtifactRef.UID = "other-uid" }, wantErr: []string{contentArtifactUID}},
		{name: "artifactRef uid removed", mutate: func(c *storagev1alpha1.SnapshotContent) { c.Status.Data.ArtifactRef.UID = "" }, wantErr: []string{contentArtifactUID}},
		{name: "sourceRef apiVersion", mutate: func(c *storagev1alpha1.SnapshotContent) { c.Status.Data.SourceRef.APIVersion = "v2" }, wantErr: []string{contentSourceIdentity}},
		{name: "sourceRef kind", mutate: func(c *storagev1alpha1.SnapshotContent) { c.Status.Data.SourceRef.Kind = "Other" }, wantErr: []string{contentSourceIdentity}},
		{name: "sourceRef name", mutate: func(c *storagev1alpha1.SnapshotContent) { c.Status.Data.SourceRef.Name = "other" }, wantErr: []string{contentSourceIdentity}},
		{name: "sourceRef namespace changed", mutate: func(c *storagev1alpha1.SnapshotContent) { c.Status.Data.SourceRef.Namespace = "other" }, wantErr: []string{contentSourceNamespace}},
		{name: "sourceRef namespace removed", mutate: func(c *storagev1alpha1.SnapshotContent) { c.Status.Data.SourceRef.Namespace = "" }, wantErr: []string{contentSourceNamespace}},
		{name: "sourceRef uid changed", mutate: func(c *storagev1alpha1.SnapshotContent) { c.Status.Data.SourceRef.UID = "other-uid" }, wantErr: []string{contentSourceUID}},
		{name: "sourceRef uid removed", mutate: func(c *storagev1alpha1.SnapshotContent) { c.Status.Data.SourceRef.UID = "" }, wantErr: []string{contentSourceUID}},
		{name: "removed", mutate: func(c *storagev1alpha1.SnapshotContent) { c.Status.Data = nil }, wantErr: []string{contentDataRemoved}},
		{name: "volume details are mutable", mutate: func(c *storagev1alpha1.SnapshotContent) {
			c.Status.Data.Size = "2Gi"
			c.Status.Data.VolumeMode = "Filesystem"
			c.Status.Data.FsType = "ext4"
			c.Status.Data.StorageClassName = "sc"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := publishedContent(t, objectName("data", tt.name), publish, enrich)
			change(t, c, writeStatus, tt.mutate, tt.wantErr...)
		})
	}
}

func TestSnapshotContentChildren(t *testing.T) {
	publish := func(c *storagev1alpha1.SnapshotContent) { c.Status.ChildrenSnapshotContentRefs = children("a", "b") }
	for _, tt := range []struct {
		name    string
		refs    []storagev1alpha1.SnapshotContentChildRef
		wantErr []string
	}{
		{name: "unchanged", refs: children("a", "b")},
		{name: "appended", refs: children("a", "b", "c"), wantErr: []string{contentChildrenFrozenOnTheField}},
		{name: "shrunk", refs: children("a"), wantErr: []string{contentChildrenFrozenOnTheField}},
		{name: "reordered", refs: children("b", "a"), wantErr: []string{contentChildrenFrozenOnTheField}},
		{name: "replaced", refs: children("a", "c"), wantErr: []string{contentChildrenFrozenOnTheField}},
		{name: "removed", wantErr: []string{contentChildrenRemoved}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := publishedContent(t, objectName("children", tt.name), publish)
			change(t, c, writeStatus, func(c *storagev1alpha1.SnapshotContent) { c.Status.ChildrenSnapshotContentRefs = tt.refs }, tt.wantErr...)
		})
	}
}

// An empty child list is the one published set that may still be filled. The
// typed client drops an empty list, so it is written as a merge patch.
func TestSnapshotContentEmptyChildrenCanBeFilled(t *testing.T) {
	c := snapshotContent("children-empty")
	create(t, c)

	ctx := context.Background()
	for _, patch := range []string{
		`{"status":{"childrenSnapshotContentRefs":[]}}`,
		`{"status":{"childrenSnapshotContentRefs":[{"name":"a"}]}}`,
	} {
		err := k8sClient.Status().Patch(ctx, c, client.RawPatch(types.MergePatchType, []byte(patch)))
		expectAdmission(t, err)
	}
	if got := c.Status.ChildrenSnapshotContentRefs; len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("want the child set [a], got %v", got)
	}
}
