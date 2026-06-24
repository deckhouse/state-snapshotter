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

// Package children reconciles a snapshot's desired set of child snapshot objects: create-or-adopt each
// desired child, derive its durable SnapshotChildRef, and garbage-collect orphans no longer desired.
package children

import (
	"context"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/ownerref"
)

// Reconcile makes the cluster match desired (the domain-built child snapshot objects), adopting each child
// under owner, and deletes children present in previous but no longer desired (orphan-GC, diffed against
// the durable child refs). It returns the resulting, sorted SnapshotChildRefs to publish into status.
//
// It fails closed on a child already owned by a different parent (no theft), leaving that child untouched.
func Reconcile(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	namespace string,
	owner metav1.OwnerReference,
	desired []client.Object,
	previous []storagev1alpha1.SnapshotChildRef,
) ([]storagev1alpha1.SnapshotChildRef, error) {
	newRefs := make([]storagev1alpha1.SnapshotChildRef, 0, len(desired))
	for _, d := range desired {
		gvk, err := apiutil.GVKForObject(d, scheme)
		if err != nil {
			return nil, err
		}
		if err := ensureChild(ctx, c, d, owner); err != nil {
			return nil, err
		}
		newRefs = append(newRefs, storagev1alpha1.SnapshotChildRef{
			APIVersion: gvk.GroupVersion().String(),
			Kind:       gvk.Kind,
			Name:       d.GetName(),
		})
	}

	if err := collectOrphans(ctx, c, namespace, previous, newRefs); err != nil {
		return nil, err
	}

	SortRefs(newRefs)
	return newRefs, nil
}

func ensureChild(ctx context.Context, c client.Client, desired client.Object, owner metav1.OwnerReference) error {
	key := client.ObjectKeyFromObject(desired)
	existing := desired.DeepCopyObject().(client.Object)
	err := c.Get(ctx, key, existing)
	if apierrors.IsNotFound(err) {
		if oErr := ownerref.Ensure(desired, owner); oErr != nil {
			return oErr
		}
		return c.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	base := existing.DeepCopyObject().(client.Object)
	if oErr := ownerref.Ensure(existing, owner); oErr != nil {
		return oErr
	}
	if ownerRefsEqual(base.GetOwnerReferences(), existing.GetOwnerReferences()) {
		return nil
	}
	return c.Patch(ctx, existing, client.MergeFrom(base))
}

func collectOrphans(ctx context.Context, c client.Client, namespace string, previous, desired []storagev1alpha1.SnapshotChildRef) error {
	desiredSet := make(map[storagev1alpha1.SnapshotChildRef]struct{}, len(desired))
	for _, r := range desired {
		desiredSet[r] = struct{}{}
	}
	for _, p := range previous {
		if _, keep := desiredSet[p]; keep {
			continue
		}
		gv, err := schema.ParseGroupVersion(p.APIVersion)
		if err != nil {
			return err
		}
		orphan := &unstructured.Unstructured{}
		orphan.SetGroupVersionKind(gv.WithKind(p.Kind))
		orphan.SetNamespace(namespace)
		orphan.SetName(p.Name)
		if err := c.Delete(ctx, orphan); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// SortRefs orders child refs deterministically by (apiVersion, kind, name).
func SortRefs(refs []storagev1alpha1.SnapshotChildRef) {
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

// RefsEqualIgnoreOrder reports set equality of two child-ref slices.
func RefsEqualIgnoreOrder(a, b []storagev1alpha1.SnapshotChildRef) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]storagev1alpha1.SnapshotChildRef(nil), a...)
	bb := append([]storagev1alpha1.SnapshotChildRef(nil), b...)
	SortRefs(aa)
	SortRefs(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

func ownerRefsEqual(left, right []metav1.OwnerReference) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].APIVersion != right[i].APIVersion ||
			left[i].Kind != right[i].Kind ||
			left[i].Name != right[i].Name ||
			left[i].UID != right[i].UID {
			return false
		}
		lc := left[i].Controller != nil && *left[i].Controller
		rc := right[i].Controller != nil && *right[i].Controller
		if lc != rc {
			return false
		}
	}
	return true
}
