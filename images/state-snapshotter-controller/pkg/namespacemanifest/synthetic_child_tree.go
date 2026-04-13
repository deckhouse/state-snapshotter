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

package namespacemanifest

// Annotation and label keys for the temporary N2b synthetic one-child tree scaffold (until domain-specific
// child wiring). String values are rollout-stable API surface; do not change without migration.

const (
	// AnnotationSyntheticChildTree on a parent NamespaceSnapshot opts into the temporary synthetic child tree.
	AnnotationSyntheticChildTree = "state-snapshotter.deckhouse.io/n2b-pr2-synthetic-tree"

	// LabelSyntheticChild marks a NamespaceSnapshot created as the synthetic child; it must not create
	// further children (leaf N2a only).
	LabelSyntheticChild = "state-snapshotter.deckhouse.io/n2b-synthetic-child"

	// LabelSyntheticParentName and LabelSyntheticParentUID on the synthetic child link it to the parent for
	// watch mapping and authoritative correlation (reconcile validates UID matches current parent).
	LabelSyntheticParentName = "state-snapshotter.deckhouse.io/n2b-parent-name"
	LabelSyntheticParentUID  = "state-snapshotter.deckhouse.io/n2b-parent-uid"
)

// NamespaceSnapshotSyntheticChildName returns the deterministic child name for the temporary scaffold (<parent>-child).
func NamespaceSnapshotSyntheticChildName(parentName string) string {
	return parentName + "-child"
}

// SyntheticChildTreeAnnotationEnabled reports whether the parent opts into the temporary synthetic child tree via annotation.
func SyntheticChildTreeAnnotationEnabled(annotations map[string]string) bool {
	if annotations == nil {
		return false
	}
	v := annotations[AnnotationSyntheticChildTree]
	return v == "true" || v == "1"
}

// IsSyntheticChildNamespaceSnapshot reports whether labels mark this NamespaceSnapshot as the synthetic child
// in the temporary N2b tree scaffold.
func IsSyntheticChildNamespaceSnapshot(labels map[string]string) bool {
	if labels == nil {
		return false
	}
	return labels[LabelSyntheticChild] == "true"
}
