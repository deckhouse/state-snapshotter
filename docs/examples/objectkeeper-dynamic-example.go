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

// Example of replacing the typed ObjectKeeper with a dynamic client.
// This removes the dependency on the whole Deckhouse module.

package examples

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ObjectKeeperGVR is the GroupVersionResource for ObjectKeeper.
var ObjectKeeperGVR = schema.GroupVersionResource{
	Group:    "deckhouse.io",
	Version:  "v1alpha1",
	Resource: "objectkeepers",
}

// ObjectKeeperHelper provides helper functions to work with ObjectKeeper via the dynamic client.
type ObjectKeeperHelper struct {
	dyn dynamic.Interface
}

func NewObjectKeeperHelper(dyn dynamic.Interface) *ObjectKeeperHelper {
	return &ObjectKeeperHelper{dyn: dyn}
}

// CreateOrGetObjectKeeper creates an ObjectKeeper or returns the existing one.
func (h *ObjectKeeperHelper) CreateOrGetObjectKeeper(
	ctx context.Context,
	name string,
	followObjectRef map[string]interface{},
) (*unstructured.Unstructured, error) {
	// Try to get the existing one.
	existing, err := h.dyn.Resource(ObjectKeeperGVR).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return existing, nil
	}
	if !errors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to get ObjectKeeper: %w", err)
	}

	// Create a new one.
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "deckhouse.io/v1alpha1",
			"kind":       "ObjectKeeper",
			"metadata": map[string]interface{}{
				"name": name,
			},
			"spec": map[string]interface{}{
				"mode":            "FollowObject",
				"followObjectRef": followObjectRef,
			},
		},
	}

	created, err := h.dyn.Resource(ObjectKeeperGVR).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create ObjectKeeper: %w", err)
	}

	return created, nil
}

// NewFollowObjectRef builds the map for followObjectRef.
func NewFollowObjectRef(apiVersion, kind, name, namespace, uid string) map[string]interface{} {
	ref := map[string]interface{}{
		"apiVersion": apiVersion,
		"kind":       kind,
		"name":       name,
		"namespace":  namespace,
	}
	if uid != "" {
		ref["uid"] = uid
	}
	return ref
}

// GetObjectKeeperMode reads mode from ObjectKeeper.
func GetObjectKeeperMode(u *unstructured.Unstructured) string {
	mode, _, _ := unstructured.NestedString(u.Object, "spec", "mode")
	return mode
}

// GetFollowObjectRef reads followObjectRef from ObjectKeeper.
func GetFollowObjectRef(u *unstructured.Unstructured) (map[string]interface{}, bool) {
	ref, found, _ := unstructured.NestedMap(u.Object, "spec", "followObjectRef")
	return ref, found
}

// Example usage in a controller:
//
// func (r *ManifestCheckpointController) createObjectKeeper(
// 	ctx context.Context,
// 	mcr *storagev1alpha1.ManifestCaptureRequest,
// ) (*unstructured.Unstructured, error) {
// 	retainerName := namespacemanifest.ManifestCaptureRequestObjectKeeperName(mcr.Namespace, mcr.Name, mcr.UID)
//
// 	helper := NewObjectKeeperHelper(r.dynamicClient)
// 	followRef := NewFollowObjectRef(
// 		"state-snapshotter.deckhouse.io/v1alpha1",
// 		"ManifestCaptureRequest",
// 		mcr.Name,
// 		mcr.Namespace,
// 		string(mcr.UID),
// 	)
//
// 	ok, err := helper.CreateOrGetObjectKeeper(ctx, retainerName, followRef)
// 	if err != nil {
// 		return nil, err
// 	}
//
// 	return ok, nil
// }

// Alternative: use the controller-runtime client with unstructured
// (when integration with existing code is needed).

func CreateObjectKeeperWithControllerClient(
	ctx context.Context,
	c client.Client,
	name string,
	followObjectRef map[string]interface{},
) error {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "deckhouse.io/v1alpha1",
			"kind":       "ObjectKeeper",
			"metadata": map[string]interface{}{
				"name": name,
			},
			"spec": map[string]interface{}{
				"mode":            "FollowObject",
				"followObjectRef": followObjectRef,
			},
		},
	}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "deckhouse.io",
		Version: "v1alpha1",
		Kind:    "ObjectKeeper",
	})

	return c.Create(ctx, obj)
}










