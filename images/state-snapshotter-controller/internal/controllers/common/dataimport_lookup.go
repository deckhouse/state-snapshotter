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

package common

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// dataImportListGVK is the SVDM DataImportList resource. State-snapshotter reads DataImport cross-service
// via the dynamic/unstructured client, so it takes no Go-module dependency on SVDM.
var dataImportListGVK = schema.GroupVersionKind{Group: "storage-foundation.deckhouse.io", Version: "v1alpha1", Kind: "DataImportList"}

// FindDataImportForLeaf reverse-looks-up the DataImport that materializes the data leg for an import-mode
// snapshot leaf. The leaf↔DataImport link is single-directional: only a ProduceArtifact DataImport's
// spec.snapshotRef points at the leaf (apiVersion/kind/name; namespace implicit = leaf namespace), so the
// binder lists DataImports in the leaf namespace and matches snapshotRef against the leaf identity. Matching
// is by GroupKind — the leaf's own GVK carries its group and kind, and snapshotRef.apiVersion carries the
// referenced group/version, so no RESTMapping is needed. PopulateVolume DataImports carry no snapshotRef and
// never match. It is the single source of the list+match+fail-closed semantics shared by the generic binder
// (domain data leaves) and the VolumeSnapshot import binder (F2).
//
// Outcomes:
//   - di != nil: exactly one DataImport targets the leaf;
//   - di == nil, terminalReason == "": no DataImport targets the leaf yet (pending — d8 may not have
//     created it; poll);
//   - terminalReason != "": more than one DataImport targets the same leaf (ambiguous, fail-closed);
//   - err != nil: a transient API (List) failure.
func FindDataImportForLeaf(ctx context.Context, c client.Client, leaf *unstructured.Unstructured) (di *unstructured.Unstructured, terminalReason, terminalMessage string, err error) {
	gvk := leaf.GetObjectKind().GroupVersionKind()
	leafGroup := gvk.Group
	leafKind := gvk.Kind
	leafName := leaf.GetName()

	// Defensive: a leaf with no Kind would match any DataImport whose snapshotRef.kind is absent (all-empty
	// equality, fail-open). Production callers always set the leaf GVK, so this is unreachable, but guard
	// it explicitly so the invariant cannot regress silently.
	if leafKind == "" {
		return nil, "", "", nil
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(dataImportListGVK)
	if lErr := c.List(ctx, list, client.InNamespace(leaf.GetNamespace())); lErr != nil {
		return nil, "", "", lErr
	}

	var match *unstructured.Unstructured
	count := 0
	for i := range list.Items {
		item := &list.Items[i]
		apiVersion, _, _ := unstructured.NestedString(item.Object, "spec", "snapshotRef", "apiVersion")
		k, _, _ := unstructured.NestedString(item.Object, "spec", "snapshotRef", "kind")
		n, _, _ := unstructured.NestedString(item.Object, "spec", "snapshotRef", "name")
		// snapshotRef.apiVersion is "group/version" (or "version" for the core group); the leaf identity is
		// keyed by group only, so parse the group out and ignore the version.
		g := schema.FromAPIVersionAndKind(apiVersion, k).Group
		if g == leafGroup && k == leafKind && n == leafName {
			count++
			match = item
		}
	}
	switch count {
	case 0:
		// Help diagnose a producer/consumer version skew: if there are candidate DataImports in the
		// namespace but none matched by GroupKind, the producer may still be writing the legacy
		// spec.targetRef instead of the ProduceArtifact spec.snapshotRef. A leaf with zero candidates is
		// the normal not-yet-created (pending) case and stays quiet.
		if len(list.Items) > 0 {
			log.FromContext(ctx).V(1).Info("no DataImport matched leaf by GroupKind",
				"leafGroup", leafGroup, "leafKind", leafKind, "leafName", leafName,
				"namespace", leaf.GetNamespace(), "candidates", len(list.Items))
		}
		return nil, "", "", nil
	case 1:
		return match, "", "", nil
	default:
		return nil, snapshot.ReasonDataImportAmbiguous, fmt.Sprintf(
			"found %d DataImports targeting %s %s/%s; exactly one is required",
			count, leafKind, leaf.GetNamespace(), leafName), nil
	}
}
