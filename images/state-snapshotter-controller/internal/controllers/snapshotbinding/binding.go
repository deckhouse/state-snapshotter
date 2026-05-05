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

// Package snapshotbinding contains shared helpers for binding a snapshot object
// to the common storage.deckhouse.io SnapshotContent carrier.
package snapshotbinding

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func StableContentName(snapshotName string, snapshotUID types.UID) string {
	return snapshot.GenerateSnapshotContentName(snapshotName, string(snapshotUID))
}

func SnapshotSubjectRef(apiVersion, kind, name, namespace string, uid types.UID) storagev1alpha1.SnapshotSubjectRef {
	return storagev1alpha1.SnapshotSubjectRef{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		Namespace:  namespace,
		UID:        uid,
	}
}

func SnapshotSubjectRefMap(apiVersion, kind, name, namespace string, uid types.UID) map[string]interface{} {
	out := map[string]interface{}{
		"apiVersion": apiVersion,
		"kind":       kind,
		"name":       name,
	}
	if namespace != "" {
		out["namespace"] = namespace
	}
	if uid != "" {
		out["uid"] = string(uid)
	}
	return out
}

func PatchUnstructuredBoundContentName(ctx context.Context, c client.Client, key client.ObjectKey, gvk schema.GroupVersionKind, contentName string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := c.Get(ctx, key, obj); err != nil {
			return err
		}
		current, _, err := unstructured.NestedString(obj.Object, "status", "boundSnapshotContentName")
		if err != nil {
			return err
		}
		if current == contentName {
			return nil
		}
		if err := unstructured.SetNestedField(obj.Object, contentName, "status", "boundSnapshotContentName"); err != nil {
			return fmt.Errorf("set status.boundSnapshotContentName: %w", err)
		}
		return c.Status().Update(ctx, obj)
	})
}
