// Пример замены типизированного ObjectKeeper на dynamic client
// Это позволяет убрать зависимость от всего Deckhouse модуля

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

// ObjectKeeperGVR - GroupVersionResource для ObjectKeeper
var ObjectKeeperGVR = schema.GroupVersionResource{
	Group:    "deckhouse.io",
	Version:  "v1alpha1",
	Resource: "objectkeepers",
}

// ObjectKeeperHelper - helper функции для работы с ObjectKeeper через dynamic client
type ObjectKeeperHelper struct {
	dyn dynamic.Interface
}

func NewObjectKeeperHelper(dyn dynamic.Interface) *ObjectKeeperHelper {
	return &ObjectKeeperHelper{dyn: dyn}
}

// CreateOrGetObjectKeeper создает ObjectKeeper или возвращает существующий
func (h *ObjectKeeperHelper) CreateOrGetObjectKeeper(
	ctx context.Context,
	name string,
	followObjectRef map[string]interface{},
) (*unstructured.Unstructured, error) {
	// Пытаемся получить существующий
	existing, err := h.dyn.Resource(ObjectKeeperGVR).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return existing, nil
	}
	if !errors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to get ObjectKeeper: %w", err)
	}

	// Создаем новый
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

// NewFollowObjectRef создает map для followObjectRef
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

// GetObjectKeeperMode получает mode из ObjectKeeper
func GetObjectKeeperMode(u *unstructured.Unstructured) string {
	mode, _, _ := unstructured.NestedString(u.Object, "spec", "mode")
	return mode
}

// GetFollowObjectRef получает followObjectRef из ObjectKeeper
func GetFollowObjectRef(u *unstructured.Unstructured) (map[string]interface{}, bool) {
	ref, found, _ := unstructured.NestedMap(u.Object, "spec", "followObjectRef")
	return ref, found
}

// Пример использования в контроллере:
//
// func (r *ManifestCheckpointController) createObjectKeeper(
// 	ctx context.Context,
// 	mcr *storagev1alpha1.ManifestCaptureRequest,
// ) (*unstructured.Unstructured, error) {
// 	retainerName := fmt.Sprintf("ret-mcr-%s-%s", mcr.Namespace, mcr.Name)
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

// Альтернатива: использовать controller-runtime client с unstructured
// (если нужна интеграция с существующим кодом)

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










