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
	"sort"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// computeExcludedRefsAggregate builds this content node's durable excludedRefs aggregate: the union of
//
//   - this node's OWN direct exclusions — the domain's vetoes read from the owning snapshot's
//     status.captureState.domainSpecificController.excludedRefs (for the root Snapshot, the top-level
//     veto drops the core records in the same field), AND
//   - the excludedRefs aggregate of every declared child common SnapshotContent.
//
// The aggregate is MONOTONIC: it unions in the currently-published set so a transient child-content
// NotFound (during degradation) never drops a previously-recorded exclusion. This is safe because the
// snapshot spec is immutable — the exclusion set for a given snapshot converges and never legitimately
// shrinks. The result is sorted for a stable, diff-friendly status.
func (r *SnapshotContentController) computeExcludedRefsAggregate(ctx context.Context, obj *unstructured.Unstructured) ([]storagev1alpha1.ExcludedObjectRef, error) {
	agg := map[storagev1alpha1.ExcludedObjectRef]struct{}{}

	// Monotonic: retain the already-published aggregate.
	for _, e := range parseExcludedRefs(obj, "status", "excludedRefs") {
		agg[e] = struct{}{}
	}

	own, err := r.ownDirectExcludedRefs(ctx, obj)
	if err != nil {
		return nil, err
	}
	for _, e := range own {
		agg[e] = struct{}{}
	}

	rawRefs, _, err := unstructured.NestedSlice(obj.Object, "status", "childrenSnapshotContentRefs")
	if err != nil {
		return nil, err
	}
	for _, raw := range rawRefs {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		childName, _ := m["name"].(string)
		if childName == "" {
			continue
		}
		childContent := &unstructured.Unstructured{}
		childContent.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
		if gerr := r.Client.Get(ctx, client.ObjectKey{Name: childName}, childContent); gerr != nil {
			if errors.IsNotFound(gerr) {
				// Monotonic aggregate: a not-yet-created child cannot subtract from the union.
				continue
			}
			return nil, gerr
		}
		for _, e := range parseExcludedRefs(childContent, "status", "excludedRefs") {
			agg[e] = struct{}{}
		}
	}

	return sortedExcludedObjectRefs(agg), nil
}

// ownDirectExcludedRefs reads the owning snapshot's direct exclusion vetoes
// (status.captureState.domainSpecificController.excludedRefs). For a domain XxxSnapshot this is what the
// domain controller published via the SDK; for the root Snapshot it is the top-level veto drops the core
// records during parent-graph enumeration. Absence (no owning snapshot yet, no domainSpecificController)
// contributes nothing.
func (r *SnapshotContentController) ownDirectExcludedRefs(ctx context.Context, contentObj *unstructured.Unstructured) ([]storagev1alpha1.ExcludedObjectRef, error) {
	apiVersion, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "apiVersion")
	kind, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "kind")
	name, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "name")
	namespace, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "namespace")
	if apiVersion == "" || kind == "" || name == "" {
		return nil, nil
	}
	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(schema.FromAPIVersionAndKind(apiVersion, kind))
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, owner); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseExcludedRefs(owner, "status", "captureState", "domainSpecificController", "excludedRefs"), nil
}

// parseExcludedRefs reads a []ExcludedObjectRef from a nested unstructured slice of {apiVersion,kind,name}
// maps at the given path. Malformed/partial entries are skipped.
func parseExcludedRefs(obj *unstructured.Unstructured, path ...string) []storagev1alpha1.ExcludedObjectRef {
	raw, found, err := unstructured.NestedSlice(obj.Object, path...)
	if err != nil || !found {
		return nil
	}
	out := make([]storagev1alpha1.ExcludedObjectRef, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		ref := storagev1alpha1.ExcludedObjectRef{}
		ref.APIVersion, _ = m["apiVersion"].(string)
		ref.Kind, _ = m["kind"].(string)
		ref.Name, _ = m["name"].(string)
		if ref.APIVersion == "" || ref.Kind == "" || ref.Name == "" {
			continue
		}
		out = append(out, ref)
	}
	return out
}

// excludedRefsToUnstructured converts a []ExcludedObjectRef into the []interface{} of maps shape required
// to write it back into an unstructured status.
func excludedRefsToUnstructured(refs []storagev1alpha1.ExcludedObjectRef) []interface{} {
	out := make([]interface{}, 0, len(refs))
	for _, ref := range refs {
		out = append(out, map[string]interface{}{
			"apiVersion": ref.APIVersion,
			"kind":       ref.Kind,
			"name":       ref.Name,
		})
	}
	return out
}

func sortedExcludedObjectRefs(set map[storagev1alpha1.ExcludedObjectRef]struct{}) []storagev1alpha1.ExcludedObjectRef {
	out := make([]storagev1alpha1.ExcludedObjectRef, 0, len(set))
	for ref := range set {
		out = append(out, ref)
	}
	sortExcludedObjectRefs(out)
	return out
}

func sortExcludedObjectRefs(refs []storagev1alpha1.ExcludedObjectRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].APIVersion != refs[j].APIVersion {
			return refs[i].APIVersion < refs[j].APIVersion
		}
		if refs[i].Kind != refs[j].Kind {
			return refs[i].Kind < refs[j].Kind
		}
		return refs[i].Name < refs[j].Name
	})
}

// excludedObjectRefsEqualIgnoreOrder reports set equality regardless of order.
func excludedObjectRefsEqualIgnoreOrder(a, b []storagev1alpha1.ExcludedObjectRef) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[storagev1alpha1.ExcludedObjectRef]int, len(a))
	for _, ref := range a {
		seen[ref]++
	}
	for _, ref := range b {
		seen[ref]--
		if seen[ref] < 0 {
			return false
		}
	}
	for _, c := range seen {
		if c != 0 {
			return false
		}
	}
	return true
}
