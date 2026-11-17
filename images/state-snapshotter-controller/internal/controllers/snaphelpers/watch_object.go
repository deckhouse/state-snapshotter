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

package snaphelpers

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func WatchObjectForGVK(scheme *runtime.Scheme, gvk schema.GroupVersionKind) client.Object {
	if scheme != nil && scheme.Recognizes(gvk) {
		if obj, err := scheme.New(gvk); err == nil {
			if typed, ok := obj.(client.Object); ok {
				return typed
			}
		}
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}
