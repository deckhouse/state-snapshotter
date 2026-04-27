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

package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/dscregistry"
)

func (r *NamespaceSnapshotReconciler) reconcileParentOwnedChildGraph(
	ctx context.Context,
	nsSnap *storagev1alpha1.NamespaceSnapshot,
	content *storagev1alpha1.NamespaceSnapshotContent,
) (bool, error) {
	mappings, err := dscregistry.EligibleResourceSnapshotMappings(ctx, r.namespaceSnapshotReader())
	if err != nil {
		return false, err
	}
	if len(mappings) == 0 {
		return false, nil
	}

	var desiredRefs []storagev1alpha1.NamespaceSnapshotChildRef
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
			childName := namespaceSnapshotChildSnapshotName(nsSnap.Name, mapping.ResourceGVK.String(), mapping.SnapshotGVK.String(), resource.GetName())
			if err := r.ensureParentOwnedChildSnapshot(ctx, nsSnap, childName, mapping.SnapshotGVK); err != nil {
				return false, err
			}
			desiredRefs = append(desiredRefs, storagev1alpha1.NamespaceSnapshotChildRef{
				APIVersion: mapping.SnapshotGVK.GroupVersion().String(),
				Kind:       mapping.SnapshotGVK.Kind,
				Name:       childName,
			})
		}
	}
	sortNamespaceSnapshotChildRefs(desiredRefs)

	statusChanged, effectiveRefs, err := r.patchNamespaceSnapshotChildrenRefs(ctx, types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, desiredRefs)
	if err != nil {
		return false, err
	}

	contentChanged, err := r.patchNamespaceSnapshotContentChildrenFromSnapshotRefs(ctx, content.Name, nsSnap.Namespace, effectiveRefs)
	if err != nil {
		return false, err
	}

	return statusChanged || contentChanged, nil
}

func namespaceSnapshotChildSnapshotName(parentName, resourceGVK, snapshotGVK, resourceName string) string {
	sum := sha256.Sum256([]byte(parentName + "|" + resourceGVK + "|" + snapshotGVK + "|" + resourceName))
	return "nss-child-" + hex.EncodeToString(sum[:10])
}

func (r *NamespaceSnapshotReconciler) ensureParentOwnedChildSnapshot(
	ctx context.Context,
	nsSnap *storagev1alpha1.NamespaceSnapshot,
	name string,
	gvk schema.GroupVersionKind,
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
				"metadata":   map[string]interface{}{"name": name, "namespace": nsSnap.Namespace},
				"spec": map[string]interface{}{
					"parentSnapshotRef": map[string]interface{}{
						"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
						"kind":       "NamespaceSnapshot",
						"name":       nsSnap.Name,
					},
				},
			},
		}
		child.SetGroupVersionKind(gvk)
		child.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
			Kind:       "NamespaceSnapshot",
			Name:       nsSnap.Name,
			UID:        nsSnap.UID,
		}})
		return r.Client.Create(ctx, child)
	}
	if child.Object["spec"] == nil {
		base := child.DeepCopy()
		child.Object["spec"] = map[string]interface{}{
			"parentSnapshotRef": map[string]interface{}{
				"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
				"kind":       "NamespaceSnapshot",
				"name":       nsSnap.Name,
			},
		}
		return r.Client.Patch(ctx, child, client.MergeFrom(base))
	}
	return nil
}

func (r *NamespaceSnapshotReconciler) patchNamespaceSnapshotChildrenRefs(
	ctx context.Context,
	parent types.NamespacedName,
	desired []storagev1alpha1.NamespaceSnapshotChildRef,
) (bool, []storagev1alpha1.NamespaceSnapshotChildRef, error) {
	changed := false
	var effective []storagev1alpha1.NamespaceSnapshotChildRef
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.NamespaceSnapshot{}
		if err := r.Client.Get(ctx, parent, cur); err != nil {
			return err
		}
		effective = mergeNamespaceSnapshotManagedChildRefs(cur.Status.ChildrenSnapshotRefs, desired)
		if namespaceSnapshotChildRefsEqualIgnoreOrder(cur.Status.ChildrenSnapshotRefs, effective) {
			return nil
		}
		cur.Status.ChildrenSnapshotRefs = append([]storagev1alpha1.NamespaceSnapshotChildRef(nil), effective...)
		cur.Status.ObservedGeneration = cur.Generation
		changed = true
		return r.Client.Status().Update(ctx, cur)
	})
	return changed, effective, err
}

func mergeNamespaceSnapshotManagedChildRefs(current, desired []storagev1alpha1.NamespaceSnapshotChildRef) []storagev1alpha1.NamespaceSnapshotChildRef {
	merged := make([]storagev1alpha1.NamespaceSnapshotChildRef, 0, len(current)+len(desired))
	for _, ref := range current {
		if namespaceSnapshotOwnsGeneratedChildRef(ref) {
			continue
		}
		merged = append(merged, ref)
	}
	merged = append(merged, desired...)
	sortNamespaceSnapshotChildRefs(merged)
	return merged
}

func namespaceSnapshotOwnsGeneratedChildRef(ref storagev1alpha1.NamespaceSnapshotChildRef) bool {
	return strings.HasPrefix(ref.Name, "nss-child-")
}

func (r *NamespaceSnapshotReconciler) patchNamespaceSnapshotContentChildrenFromSnapshotRefs(
	ctx context.Context,
	contentName string,
	parentNamespace string,
	refs []storagev1alpha1.NamespaceSnapshotChildRef,
) (bool, error) {
	var desired []storagev1alpha1.NamespaceSnapshotContentChildRef
	for _, ref := range refs {
		child := &unstructured.Unstructured{}
		child.SetAPIVersion(ref.APIVersion)
		child.SetKind(ref.Kind)
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: parentNamespace, Name: ref.Name}, child); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		boundName, found, err := unstructured.NestedString(child.Object, "status", "boundSnapshotContentName")
		if err != nil {
			return false, err
		}
		if found && boundName != "" {
			desired = append(desired, storagev1alpha1.NamespaceSnapshotContentChildRef{Name: boundName})
		}
	}
	sortNamespaceSnapshotContentChildRefs(desired)

	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		nsc := &storagev1alpha1.NamespaceSnapshotContent{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, nsc); err != nil {
			return err
		}
		if namespaceSnapshotContentChildRefsEqualIgnoreOrder(nsc.Status.ChildrenSnapshotContentRefs, desired) {
			return nil
		}
		nsc.Status.ChildrenSnapshotContentRefs = append([]storagev1alpha1.NamespaceSnapshotContentChildRef(nil), desired...)
		changed = true
		return r.Client.Status().Update(ctx, nsc)
	})
	return changed, err
}

func sortNamespaceSnapshotChildRefs(refs []storagev1alpha1.NamespaceSnapshotChildRef) {
	sort.Slice(refs, func(i, j int) bool {
		return fmt.Sprintf("%s/%s/%s", refs[i].APIVersion, refs[i].Kind, refs[i].Name) <
			fmt.Sprintf("%s/%s/%s", refs[j].APIVersion, refs[j].Kind, refs[j].Name)
	})
}

func sortNamespaceSnapshotContentChildRefs(refs []storagev1alpha1.NamespaceSnapshotContentChildRef) {
	sort.Slice(refs, func(i, j int) bool {
		return refs[i].Name < refs[j].Name
	})
}
