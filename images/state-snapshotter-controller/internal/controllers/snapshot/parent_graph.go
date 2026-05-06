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

package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/csdregistry"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func (r *SnapshotReconciler) reconcileParentOwnedChildGraph(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
) (bool, error) {
	mappings, err := csdregistry.EligibleResourceSnapshotMappings(ctx, r.snapshotReader())
	if err != nil {
		return false, err
	}
	if len(mappings) == 0 {
		changed, err := r.patchSnapshotChildrenRefs(ctx, types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, nil)
		return changed, err
	}

	var desiredRefs []storagev1alpha1.SnapshotChildRef
	for _, mapping := range mappings {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(mapping.ResourceGVK)
		list.SetKind(mapping.ResourceGVK.Kind + "List")
		resources, err := r.Dynamic.Resource(mapping.ResourceGVR).Namespace(nsSnap.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			if errors.IsNotFound(err) || errors.IsForbidden(err) {
				continue
			}
			return false, err
		}
		list.Items = resources.Items
		for i := range list.Items {
			resource := &list.Items[i]
			if len(resource.GetOwnerReferences()) > 0 {
				// Parent-owned graph starts only from top-level domain resources.
				// Owned resources are covered by their owner domain subtree when that owner is registered,
				// and skipped fail-closed when the owner domain is not registered.
				continue
			}
			childName := snapshotChildSnapshotName(nsSnap.Name, mapping.ResourceGVK.String(), mapping.SnapshotGVK.String(), resource.GetName())
			if err := r.ensureParentOwnedChildSnapshot(ctx, nsSnap, childName, mapping.SnapshotGVK, mapping.ResourceGVK, resource.GetName()); err != nil {
				return false, err
			}
			desiredRefs = append(desiredRefs, storagev1alpha1.SnapshotChildRef{
				APIVersion: mapping.SnapshotGVK.GroupVersion().String(),
				Kind:       mapping.SnapshotGVK.Kind,
				Name:       childName,
			})
		}
	}
	sortSnapshotChildRefs(desiredRefs)

	statusChanged, err := r.patchSnapshotChildrenRefs(ctx, types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, desiredRefs)
	if err != nil {
		return false, err
	}

	_ = content
	return statusChanged, nil
}

func snapshotChildSnapshotName(parentName, resourceGVK, snapshotGVK, resourceName string) string {
	sum := sha256.Sum256([]byte(parentName + "|" + resourceGVK + "|" + snapshotGVK + "|" + resourceName))
	return "nss-child-" + hex.EncodeToString(sum[:10])
}

func (r *SnapshotReconciler) ensureParentOwnedChildSnapshot(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	name string,
	gvk schema.GroupVersionKind,
	resourceGVK schema.GroupVersionKind,
	resourceName string,
) error {
	key := client.ObjectKey{Namespace: nsSnap.Namespace, Name: name}
	child := &unstructured.Unstructured{}
	child.SetGroupVersionKind(gvk)
	if err := r.Client.Get(ctx, key, child); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		child = &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": gvk.GroupVersion().String(),
				"kind":       gvk.Kind,
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": nsSnap.Namespace,
				},
				"spec": map[string]interface{}{
					"sourceRef": map[string]interface{}{
						"apiVersion": resourceGVK.GroupVersion().String(),
						"kind":       resourceGVK.Kind,
						"name":       resourceName,
					},
				},
			},
		}
		child.SetGroupVersionKind(gvk)
		child.SetOwnerReferences([]metav1.OwnerReference{controllercommon.SnapshotOwnerReference(storagev1alpha1.SchemeGroupVersion.String(), "Snapshot", nsSnap.Name, nsSnap.UID)})
		return r.Client.Create(ctx, child)
	}
	base := child.DeepCopy()
	changed := false
	if err := controllercommon.EnsureSnapshotOwnerRef(child, controllercommon.SnapshotOwnerReference(storagev1alpha1.SchemeGroupVersion.String(), "Snapshot", nsSnap.Name, nsSnap.UID)); err != nil {
		return err
	}
	if !controllercommon.OwnerReferencesEqual(base.GetOwnerReferences(), child.GetOwnerReferences()) {
		changed = true
	}
	if child.Object["spec"] == nil {
		child.Object["spec"] = map[string]interface{}{}
		changed = true
	}
	spec, _ := child.Object["spec"].(map[string]interface{})
	if ensureSourceRefSpec(spec, resourceGVK, resourceName) {
		changed = true
	}
	if changed {
		return r.Client.Patch(ctx, child, client.MergeFrom(base))
	}
	return nil
}

func ensureSourceRefSpec(spec map[string]interface{}, resourceGVK schema.GroupVersionKind, resourceName string) bool {
	want := map[string]interface{}{
		"apiVersion": resourceGVK.GroupVersion().String(),
		"kind":       resourceGVK.Kind,
		"name":       resourceName,
	}
	got, _ := spec["sourceRef"].(map[string]interface{})
	if got != nil && got["apiVersion"] == want["apiVersion"] && got["kind"] == want["kind"] && got["name"] == want["name"] {
		return false
	}
	spec["sourceRef"] = want
	return true
}

func (r *SnapshotReconciler) patchSnapshotChildrenRefs(
	ctx context.Context,
	parent types.NamespacedName,
	desired []storagev1alpha1.SnapshotChildRef,
) (bool, error) {
	changed := false
	var effective []storagev1alpha1.SnapshotChildRef
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, parent, cur); err != nil {
			return err
		}
		effective = mergeSnapshotManagedChildRefs(cur.Status.ChildrenSnapshotRefs, desired)
		graphReady := meta.FindStatusCondition(cur.Status.Conditions, snapshotpkg.ConditionGraphReady)
		graphReadyCurrent := graphReady != nil &&
			graphReady.Status == metav1.ConditionTrue &&
			graphReady.Reason == snapshotpkg.ReasonCompleted &&
			graphReady.ObservedGeneration == cur.Generation
		if snapshotChildRefsEqualIgnoreOrder(cur.Status.ChildrenSnapshotRefs, effective) && graphReadyCurrent {
			return nil
		}
		cur.Status.ChildrenSnapshotRefs = append([]storagev1alpha1.SnapshotChildRef(nil), effective...)
		cur.Status.ObservedGeneration = cur.Generation
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:               snapshotpkg.ConditionGraphReady,
			Status:             metav1.ConditionTrue,
			Reason:             snapshotpkg.ReasonCompleted,
			Message:            "child graph planned",
			ObservedGeneration: cur.Generation,
		})
		changed = true
		return r.Client.Status().Update(ctx, cur)
	})
	return changed, err
}

func (r *SnapshotReconciler) patchSnapshotGraphReady(
	ctx context.Context,
	key types.NamespacedName,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, key, cur); err != nil {
			return err
		}
		existing := meta.FindStatusCondition(cur.Status.Conditions, snapshotpkg.ConditionGraphReady)
		if existing != nil &&
			existing.Status == status &&
			existing.Reason == reason &&
			existing.Message == message &&
			existing.ObservedGeneration == cur.Generation {
			return nil
		}
		base := cur.DeepCopy()
		cur.Status.ObservedGeneration = cur.Generation
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:               snapshotpkg.ConditionGraphReady,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: cur.Generation,
		})
		return r.Client.Status().Patch(ctx, cur, client.MergeFrom(base))
	})
}

func mergeSnapshotManagedChildRefs(current, desired []storagev1alpha1.SnapshotChildRef) []storagev1alpha1.SnapshotChildRef {
	merged := make([]storagev1alpha1.SnapshotChildRef, 0, len(current)+len(desired))
	for _, ref := range current {
		if snapshotOwnsGeneratedChildRef(ref) {
			continue
		}
		merged = append(merged, ref)
	}
	merged = append(merged, desired...)
	sortSnapshotChildRefs(merged)
	return merged
}

func snapshotOwnsGeneratedChildRef(ref storagev1alpha1.SnapshotChildRef) bool {
	return strings.HasPrefix(ref.Name, "nss-child-")
}

func sortSnapshotChildRefs(refs []storagev1alpha1.SnapshotChildRef) {
	sort.Slice(refs, func(i, j int) bool {
		return fmt.Sprintf("%s/%s/%s", refs[i].APIVersion, refs[i].Kind, refs[i].Name) <
			fmt.Sprintf("%s/%s/%s", refs[j].APIVersion, refs[j].Kind, refs[j].Name)
	})
}

func sortSnapshotContentChildRefs(refs []storagev1alpha1.SnapshotContentChildRef) {
	sort.Slice(refs, func(i, j int) bool {
		return refs[i].Name < refs[j].Name
	})
}
