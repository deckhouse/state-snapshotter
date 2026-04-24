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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// childSnapshotRefMatchesUnstructuredChild reports whether a strict NamespaceSnapshotChildRef
// identifies the same object as an unstructured child snapshot from the API.
func childSnapshotRefMatchesUnstructuredChild(ref storagev1alpha1.NamespaceSnapshotChildRef, child *unstructured.Unstructured, childName string) bool {
	if ref.Name != childName || ref.APIVersion == "" || ref.Kind == "" {
		return false
	}
	if ref.APIVersion == child.GetAPIVersion() && ref.Kind == child.GetKind() {
		return true
	}
	refGVK := schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind)
	if refGVK.Kind == "" || refGVK.Version == "" {
		return false
	}
	return refGVK == child.GroupVersionKind()
}

// findParentsReferencingChildSnapshot returns reconcile requests for NamespaceSnapshot parents whose
// status.childrenSnapshotRefs match the child's apiVersion, kind, namespace, and name.
//
// Snapshot-run tree is namespace-local: only NamespaceSnapshot objects in the child's namespace are
// considered (no cluster-wide list). Ref effective namespace must equal the child's namespace.
func findParentsReferencingChildSnapshot(ctx context.Context, c client.Reader, child *unstructured.Unstructured) []reconcile.Request {
	if child == nil {
		return nil
	}
	childName := child.GetName()
	childNS := child.GetNamespace()
	if childNS == "" {
		// Relay is for namespaced child snapshot objects; cluster-scoped snapshot kinds are out of scope here.
		return nil
	}

	list := &storagev1alpha1.NamespaceSnapshotList{}
	if err := c.List(ctx, list, client.InNamespace(childNS)); err != nil {
		log.FromContext(ctx).Error(err, "findParentsReferencingChildSnapshot: list NamespaceSnapshot", "namespace", childNS)
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		p := &list.Items[i]
		for _, ref := range p.Status.ChildrenSnapshotRefs {
			if !childSnapshotRefMatchesUnstructuredChild(ref, child, childName) {
				continue
			}
			effectiveRefNS := ref.Namespace
			if effectiveRefNS == "" {
				effectiveRefNS = p.Namespace
			}
			if effectiveRefNS != childNS {
				continue
			}
			out = append(out, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name},
			})
			break
		}
	}
	return out
}
