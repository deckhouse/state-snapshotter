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
	"k8s.io/apimachinery/pkg/runtime/schema"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// SnapshotContentNode is the internal shape for common SnapshotContent graph traversal.
type SnapshotContentNode struct {
	GVK                    schema.GroupVersionKind
	Name                   string
	ManifestCheckpointName string
	Children               []storagev1alpha1.SnapshotContentChildRef
}

func snapshotContentNodeFromSnapshotContent(sc *storagev1alpha1.SnapshotContent) SnapshotContentNode {
	if sc == nil {
		return SnapshotContentNode{}
	}
	return SnapshotContentNode{
		GVK:                    SnapshotContentGVK(),
		Name:                   sc.Name,
		ManifestCheckpointName: sc.Status.ManifestCheckpointName,
		Children:               append([]storagev1alpha1.SnapshotContentChildRef(nil), sc.Status.ChildrenSnapshotContentRefs...),
	}
}

func snapshotContentChildRefsFromNames(names []string) []storagev1alpha1.SnapshotContentChildRef {
	out := make([]storagev1alpha1.SnapshotContentChildRef, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		out = append(out, storagev1alpha1.SnapshotContentChildRef{Name: name})
	}
	return out
}
