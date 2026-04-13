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

// N2b PR2: one synthetic NamespaceSnapshot child for graph/reconcile semantics only (temporary scaffold
// until domain-driven child wiring ~PR5). Annotation key, child naming rule (parentName+"-child"), and
// related condition reasons are not the long-term product contract — see design §2.4.2 and
// namespace-snapshot-controller.md §11.1.

const (
	// AnnotationN2bPR2SyntheticTree on a parent NamespaceSnapshot opts into PR2: controller ensures
	// exactly one synthetic child named NamespaceSnapshotSyntheticChildName(parent.Name).
	// Scaffold-only until PR5.
	AnnotationN2bPR2SyntheticTree = "state-snapshotter.deckhouse.io/n2b-pr2-synthetic-tree"

	// LabelN2bSyntheticChild marks a NamespaceSnapshot created as the PR2 synthetic child; it must not
	// create further children (leaf N2a only).
	LabelN2bSyntheticChild = "state-snapshotter.deckhouse.io/n2b-synthetic-child"

	// LabelN2bParentName and LabelN2bParentUID on the synthetic child link it to the parent for watch map
	// and authoritative correlation (reconcile validates UID matches current parent).
	LabelN2bParentName = "state-snapshotter.deckhouse.io/n2b-parent-name"
	LabelN2bParentUID  = "state-snapshotter.deckhouse.io/n2b-parent-uid"
)

// NamespaceSnapshotSyntheticChildName returns the deterministic child name for PR2 (<parent>-child).
func NamespaceSnapshotSyntheticChildName(parentName string) string {
	return parentName + "-child"
}

// N2bPR2SyntheticTreeEnabled returns true if the parent opts into PR2 synthetic one-child tree.
func N2bPR2SyntheticTreeEnabled(annotations map[string]string) bool {
	if annotations == nil {
		return false
	}
	v := annotations[AnnotationN2bPR2SyntheticTree]
	return v == "true" || v == "1"
}

// N2bIsSyntheticChildNamespaceSnapshot returns true if this object is a PR2 synthetic child (must not recurse).
func N2bIsSyntheticChildNamespaceSnapshot(labels map[string]string) bool {
	if labels == nil {
		return false
	}
	return labels[LabelN2bSyntheticChild] == "true"
}
