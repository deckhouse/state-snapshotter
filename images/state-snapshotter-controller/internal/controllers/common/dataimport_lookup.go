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

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// dataImportListGVK is the SVDM DataImportList resource. State-snapshotter reads DataImport cross-service
// via the dynamic/unstructured client, so it takes no Go-module dependency on SVDM.
var dataImportListGVK = schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "DataImportList"}

// FindDataImportForLeaf reverse-looks-up the DataImport that materializes the data leg for an import-mode
// snapshot leaf. The leaf↔DataImport link is single-directional: only DataImport.spec.targetRef points at
// the leaf (group/resource/name; namespace implicit = leaf namespace), so the binder lists DataImports in
// the leaf namespace and matches targetRef against the leaf identity. It is the single source of the
// list+match+fail-closed semantics shared by the generic binder (domain data leaves) and the VolumeSnapshot
// import binder (F2).
//
// Outcomes:
//   - di != nil: exactly one DataImport targets the leaf;
//   - di == nil, terminalReason == "": no DataImport targets the leaf yet (pending — d8 may not have
//     created it; poll);
//   - terminalReason != "": more than one DataImport targets the same leaf (ambiguous, fail-closed);
//   - err != nil: a transient API/RESTMapping failure.
func FindDataImportForLeaf(ctx context.Context, c client.Client, leaf *unstructured.Unstructured) (di *unstructured.Unstructured, terminalReason, terminalMessage string, err error) {
	gvk := leaf.GetObjectKind().GroupVersionKind()
	mapping, mErr := c.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if mErr != nil {
		return nil, "", "", fmt.Errorf("resolve resource for leaf %s: %w", gvk.String(), mErr)
	}
	leafGroup := gvk.Group
	leafResource := mapping.Resource.Resource
	leafName := leaf.GetName()

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(dataImportListGVK)
	if lErr := c.List(ctx, list, client.InNamespace(leaf.GetNamespace())); lErr != nil {
		return nil, "", "", lErr
	}

	var match *unstructured.Unstructured
	count := 0
	for i := range list.Items {
		item := &list.Items[i]
		g, _, _ := unstructured.NestedString(item.Object, "spec", "targetRef", "group")
		res, _, _ := unstructured.NestedString(item.Object, "spec", "targetRef", "resource")
		n, _, _ := unstructured.NestedString(item.Object, "spec", "targetRef", "name")
		if g == leafGroup && res == leafResource && n == leafName {
			count++
			match = item
		}
	}
	switch count {
	case 0:
		return nil, "", "", nil
	case 1:
		return match, "", "", nil
	default:
		return nil, snapshot.ReasonDataImportAmbiguous, fmt.Sprintf(
			"found %d DataImports targeting %s %s/%s; exactly one is required",
			count, leafResource, leaf.GetNamespace(), leafName), nil
	}
}
