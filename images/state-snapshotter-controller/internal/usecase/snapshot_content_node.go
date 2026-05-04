/*
Copyright 2025 Flant JSC

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
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// SnapshotContentNode is the internal target shape for manifest/data graph traversal.
// Stage 1 only introduces adapters; existing runtime still reads dedicated content CRDs.
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

func snapshotContentNodeFromNamespaceSnapshotContent(nsc *storagev1alpha1.NamespaceSnapshotContent) SnapshotContentNode {
	if nsc == nil {
		return SnapshotContentNode{}
	}
	return SnapshotContentNode{
		GVK:                    NamespaceSnapshotContentGVK(),
		Name:                   nsc.Name,
		ManifestCheckpointName: nsc.Status.ManifestCheckpointName,
		Children:               snapshotContentChildRefsFromNamespaceRefs(nsc.Status.ChildrenSnapshotContentRefs),
	}
}

func snapshotContentNodeFromUnstructured(u *unstructured.Unstructured) (SnapshotContentNode, error) {
	if u == nil {
		return SnapshotContentNode{}, nil
	}
	mcpName, _, err := unstructured.NestedString(u.Object, "status", "manifestCheckpointName")
	if err != nil {
		return SnapshotContentNode{}, fmt.Errorf("read status.manifestCheckpointName: %w", err)
	}
	childNames, err := unstructuredChildrenSnapshotContentRefNames(u)
	if err != nil {
		return SnapshotContentNode{}, err
	}
	return SnapshotContentNode{
		GVK:                    u.GroupVersionKind(),
		Name:                   u.GetName(),
		ManifestCheckpointName: mcpName,
		Children:               snapshotContentChildRefsFromNames(childNames),
	}, nil
}

func snapshotContentChildRefsFromNamespaceRefs(refs []storagev1alpha1.NamespaceSnapshotContentChildRef) []storagev1alpha1.SnapshotContentChildRef {
	out := make([]storagev1alpha1.SnapshotContentChildRef, 0, len(refs))
	for _, ref := range refs {
		if ref.Name == "" {
			continue
		}
		out = append(out, storagev1alpha1.SnapshotContentChildRef{Name: ref.Name})
	}
	return out
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
