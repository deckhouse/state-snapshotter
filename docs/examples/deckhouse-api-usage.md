# Работа с Deckhouse API по частям

## Текущая ситуация

Сейчас используется весь Deckhouse модуль:
```go
require (
    github.com/deckhouse/deckhouse v1.67.7-0.20251212134859-497a0dab9fc0
)
```

Но реально используется только:
- `deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1` для `ObjectKeeper`

## Варианты работы с Deckhouse API

### 1. Dynamic Client (уже используется для некоторых ресурсов)

**Плюсы:**
- Не требует импорта типов
- Работает с любыми CRD
- Минимальные зависимости

**Пример:**
```go
// Уже используется в namespace_archive.go
namespaced := []schema.GroupVersionResource{
    {Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "virtualdisks"},
    {Group: "virtualization.deckhouse.io", Version: "v1alpha2", Resource: "virtualmachines"},
    {Group: "deckhouse.io", Version: "v1alpha1", Resource: "authorizationrules"},
    {Group: "deckhouse.io", Version: "v1alpha1", Resource: "podloggingconfigs"},
}

for _, gvr := range namespaced {
    lst, err := dyn.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
    // ...
}
```

**Для ObjectKeeper тоже можно использовать dynamic client:**
```go
import (
    "k8s.io/apimachinery/pkg/runtime/schema"
    "k8s.io/client-go/dynamic"
)

func createObjectKeeperDynamic(dyn dynamic.Interface, name string) error {
    gvr := schema.GroupVersionResource{
        Group:    "deckhouse.io",
        Version:  "v1alpha1",
        Resource: "objectkeepers",
    }
    
    obj := &unstructured.Unstructured{
        Object: map[string]interface{}{
            "apiVersion": "deckhouse.io/v1alpha1",
            "kind":       "ObjectKeeper",
            "metadata": map[string]interface{}{
                "name": name,
            },
            "spec": map[string]interface{}{
                "mode": "FollowObject",
                "followObjectRef": map[string]interface{}{
                    "apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
                    "kind":       "ManifestCaptureRequest",
                    "name":       "example",
                    "namespace":  "default",
                },
            },
        },
    }
    
    _, err := dyn.Resource(gvr).Create(ctx, obj, metav1.CreateOptions{})
    return err
}
```

### 2. Минимальные типы (если нужна типизация)

**Создать свои типы только для нужных ресурсов:**

```go
// pkg/deckhouse/types.go
package deckhouse

import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ObjectKeeper - минимальный тип только для ObjectKeeper
type ObjectKeeper struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    
    Spec ObjectKeeperSpec `json:"spec"`
}

type ObjectKeeperSpec struct {
    Mode            string           `json:"mode"`
    FollowObjectRef *FollowObjectRef `json:"followObjectRef,omitempty"`
}

type FollowObjectRef struct {
    APIVersion string `json:"apiVersion"`
    Kind       string `json:"kind"`
    Name       string `json:"name"`
    Namespace  string `json:"namespace"`
}

// ObjectKeeperList для List операций
type ObjectKeeperList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []ObjectKeeper `json:"items"`
}
```

**Использование с unstructured:**
```go
import (
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
    "k8s.io/apimachinery/pkg/runtime"
)

func convertToObjectKeeper(u *unstructured.Unstructured) (*ObjectKeeper, error) {
    ok := &ObjectKeeper{}
    if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, ok); err != nil {
        return nil, err
    }
    return ok, nil
}

func convertFromObjectKeeper(ok *ObjectKeeper) (*unstructured.Unstructured, error) {
    obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ok)
    if err != nil {
        return nil, err
    }
    return &unstructured.Unstructured{Object: obj}, nil
}
```

### 3. Replace директивы для точечного импорта (если пакеты экспортируются)

**Если Deckhouse экспортирует отдельные пакеты:**

```go
// go.mod
require (
    github.com/deckhouse/deckhouse-controller-api v0.0.0
)

replace (
    github.com/deckhouse/deckhouse-controller-api => github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1
)
```

**Но это работает только если:**
- Пакет экспортируется как отдельный модуль
- Или используется через `replace` с конкретным путем

### 4. Генерация типов из OpenAPI схемы

**Если есть OpenAPI схема Deckhouse ресурсов:**

```bash
# Генерация типов из OpenAPI
openapi-generator generate -i deckhouse-openapi.yaml -g go -o pkg/deckhouse/types
```

## Рекомендация для state-snapshotter

### Вариант A: Полный переход на Dynamic Client (рекомендуется)

**Плюсы:**
- Нет зависимости от Deckhouse
- Работает с любыми версиями Deckhouse
- Минимальный размер зависимостей

**Минусы:**
- Нет типизации на этапе компиляции
- Нужно вручную работать с unstructured

**Пример миграции ObjectKeeper на dynamic client:**

```go
// internal/controllers/manifestcheckpoint_controller.go

import (
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
    "k8s.io/apimachinery/pkg/runtime/schema"
    "k8s.io/client-go/dynamic"
)

const (
    ObjectKeeperGVR = schema.GroupVersionResource{
        Group:    "deckhouse.io",
        Version:  "v1alpha1",
        Resource: "objectkeepers",
    }
)

func (r *ManifestCheckpointController) createObjectKeeperDynamic(
    ctx context.Context,
    dyn dynamic.Interface,
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
    
    _, err := dyn.Resource(ObjectKeeperGVR).Create(ctx, obj, metav1.CreateOptions{})
    return err
}
```

### Вариант B: Минимальные типы + Dynamic Client

**Создать свои типы только для ObjectKeeper:**

```go
// pkg/deckhouse/objectkeeper.go
package deckhouse

import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/runtime"
    "k8s.io/apimachinery/pkg/runtime/schema"
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var ObjectKeeperGVK = schema.GroupVersionKind{
    Group:   "deckhouse.io",
    Version: "v1alpha1",
    Kind:    "ObjectKeeper",
}

type ObjectKeeper struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec              ObjectKeeperSpec `json:"spec"`
}

type ObjectKeeperSpec struct {
    Mode            string           `json:"mode"`
    FollowObjectRef *FollowObjectRef `json:"followObjectRef,omitempty"`
}

type FollowObjectRef struct {
    APIVersion string `json:"apiVersion"`
    Kind       string `json:"kind"`
    Name       string `json:"name"`
    Namespace  string `json:"namespace"`
}

// ToUnstructured конвертирует ObjectKeeper в unstructured
func (ok *ObjectKeeper) ToUnstructured() (*unstructured.Unstructured, error) {
    obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ok)
    if err != nil {
        return nil, err
    }
    u := &unstructured.Unstructured{Object: obj}
    u.SetGroupVersionKind(ObjectKeeperGVK)
    return u, nil
}

// FromUnstructured создает ObjectKeeper из unstructured
func FromUnstructured(u *unstructured.Unstructured) (*ObjectKeeper, error) {
    ok := &ObjectKeeper{}
    if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, ok); err != nil {
        return nil, err
    }
    return ok, nil
}
```

**Использование:**

```go
import (
    "github.com/deckhouse/state-snapshotter/pkg/deckhouse"
    "k8s.io/client-go/dynamic"
)

func createObjectKeeper(ctx context.Context, dyn dynamic.Interface, name string) error {
    ok := &deckhouse.ObjectKeeper{
        ObjectMeta: metav1.ObjectMeta{Name: name},
        Spec: deckhouse.ObjectKeeperSpec{
            Mode: "FollowObject",
            FollowObjectRef: &deckhouse.FollowObjectRef{
                APIVersion: "state-snapshotter.deckhouse.io/v1alpha1",
                Kind:       "ManifestCaptureRequest",
                Name:       "example",
                Namespace:  "default",
            },
        },
    }
    
    u, err := ok.ToUnstructured()
    if err != nil {
        return err
    }
    
    _, err = dyn.Resource(deckhouse.ObjectKeeperGVR).Create(ctx, u, metav1.CreateOptions{})
    return err
}
```

## Итоговая рекомендация

**Для state-snapshotter лучше всего:**

1. **Убрать зависимость от всего Deckhouse**
2. **Использовать Dynamic Client для всех Deckhouse ресурсов** (включая ObjectKeeper)
3. **Создать минимальные helper-функции** для работы с ObjectKeeper через unstructured

**Преимущества:**
- ✅ Нет зависимости от Deckhouse
- ✅ Работает с любой версией Deckhouse
- ✅ Минимальный размер зависимостей
- ✅ Проще обновлять и поддерживать

**Пример helper-функций:**

```go
// pkg/deckhouse/helpers.go
package deckhouse

import (
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
    "k8s.io/apimachinery/pkg/runtime/schema"
)

var ObjectKeeperGVR = schema.GroupVersionResource{
    Group:    "deckhouse.io",
    Version:  "v1alpha1",
    Resource: "objectkeepers",
}

func NewObjectKeeper(name string, mode string, followRef map[string]interface{}) *unstructured.Unstructured {
    return &unstructured.Unstructured{
        Object: map[string]interface{}{
            "apiVersion": "deckhouse.io/v1alpha1",
            "kind":       "ObjectKeeper",
            "metadata": map[string]interface{}{
                "name": name,
            },
            "spec": map[string]interface{}{
                "mode":            mode,
                "followObjectRef": followRef,
            },
        },
    }
}

func GetObjectKeeperMode(u *unstructured.Unstructured) string {
    mode, _, _ := unstructured.NestedString(u.Object, "spec", "mode")
    return mode
}
```










