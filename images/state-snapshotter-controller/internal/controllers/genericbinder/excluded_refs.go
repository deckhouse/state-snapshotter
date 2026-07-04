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

package genericbinder

import (
	"context"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// mirrorExcludedRefsFromContent mirrors the bound SnapshotContent's durable excludedRefs aggregate onto
// the domain XxxSnapshot's top-level status.excludedRefs (a user-facing audit view). The content is the
// single writer of the durable truth; the domain CR only mirrors it. Read-modify-write only this field
// under an optimistic-lock merge patch so it never clobbers the domain's captureState or conditions
// co-written into the same status. The content aggregate is monotonic, so a verbatim mirror is safe.
func (r *GenericSnapshotBinderController) mirrorExcludedRefsFromContent(
	ctx context.Context,
	obj *unstructured.Unstructured,
	contentObj *unstructured.Unstructured,
) error {
	desired := parseExcludedRefs(contentObj, "status", "excludedRefs")
	sortExcludedObjectRefs(desired)

	if excludedObjectRefsEqualIgnoreOrder(parseExcludedRefs(obj, "status", "excludedRefs"), desired) {
		return nil
	}

	gvk := obj.GetObjectKind().GroupVersionKind()
	key := client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(gvk)
		if err := r.Get(ctx, key, fresh); err != nil {
			return err
		}
		if excludedObjectRefsEqualIgnoreOrder(parseExcludedRefs(fresh, "status", "excludedRefs"), desired) {
			return nil
		}
		base := fresh.DeepCopy()
		status, _ := fresh.Object["status"].(map[string]interface{})
		if status == nil {
			status = map[string]interface{}{}
		}
		if len(desired) == 0 {
			delete(status, "excludedRefs")
		} else {
			status["excludedRefs"] = excludedRefsToUnstructured(desired)
		}
		fresh.Object["status"] = status
		if err := r.Status().Patch(ctx, fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("failed to mirror SnapshotContent excludedRefs: %w", err)
		}
		return nil
	})
}

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
