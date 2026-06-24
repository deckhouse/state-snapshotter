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

package common

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newSecretObject(secretType string, annotations map[string]string) *unstructured.Unstructured {
	metadata := map[string]interface{}{
		"name":      "secret",
		"namespace": "ns",
	}
	if annotations != nil {
		metadata["annotations"] = annotations
	}
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   metadata,
		"type":       secretType,
		"data": map[string]interface{}{
			"password": "cGFzcw==",
		},
		"stringData": map[string]interface{}{
			"token": "plain",
		},
	}}
	u.SetAPIVersion("v1")
	u.SetKind("Secret")
	u.SetName("secret")
	u.SetNamespace("ns")
	u.SetAnnotations(annotations)
	return u
}

func newSecretObjectWithoutType(annotations map[string]string) *unstructured.Unstructured {
	u := newSecretObject("Opaque", annotations)
	unstructured.RemoveNestedField(u.Object, "type")
	return u
}

func newConfigMapObject() *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      "cm",
			"namespace": "ns",
		},
		"data": map[string]interface{}{"key": "value"},
	}}
	u.SetAPIVersion("v1")
	u.SetKind("ConfigMap")
	u.SetName("cm")
	u.SetNamespace("ns")
	return u
}

func assertNestedMapAbsent(t *testing.T, obj map[string]interface{}, fields ...string) {
	t.Helper()
	if _, found, err := unstructured.NestedMap(obj, fields...); err != nil || found {
		t.Fatalf("expected %v to be absent, found=%v err=%v", fields, found, err)
	}
}

func assertNestedMapPresent(t *testing.T, obj map[string]interface{}, fields ...string) map[string]interface{} {
	t.Helper()
	value, found, err := unstructured.NestedMap(obj, fields...)
	if err != nil || !found {
		t.Fatalf("expected %v to be present, found=%v err=%v", fields, found, err)
	}
	return value
}

func assertObjectUnchanged(t *testing.T, got, want *unstructured.Unstructured) {
	t.Helper()
	if !reflect.DeepEqual(got.Object, want.Object) {
		t.Fatalf("expected original object unchanged, got %#v want %#v", got.Object, want.Object)
	}
}

func TestShouldSkipObjectAndShouldSkipSecretObject(t *testing.T) {
	tests := []struct {
		name                 string
		object               *unstructured.Unstructured
		wantShouldSkipObject bool
		wantShouldSkipSecret bool
	}{
		{
			name:                 "tls secret is skipped",
			object:               newSecretObject("kubernetes.io/tls", nil),
			wantShouldSkipObject: true,
			wantShouldSkipSecret: true,
		},
		{
			name:                 "docker config json secret is skipped",
			object:               newSecretObject("kubernetes.io/dockerconfigjson", nil),
			wantShouldSkipObject: true,
			wantShouldSkipSecret: true,
		},
		{
			name:                 "opaque secret without annotations is skipped",
			object:               newSecretObject("Opaque", nil),
			wantShouldSkipObject: true,
			wantShouldSkipSecret: true,
		},
		{
			name:                 "opaque secret with include-secret is included",
			object:               newSecretObject("Opaque", map[string]string{AnnotationIncludeSecret: "true"}),
			wantShouldSkipObject: false,
			wantShouldSkipSecret: false,
		},
		{
			name:                 "opaque secret with include-secret-data standalone is included",
			object:               newSecretObject("Opaque", map[string]string{AnnotationIncludeSecretData: "true"}),
			wantShouldSkipObject: false,
			wantShouldSkipSecret: false,
		},
		{
			name:                 "secret without type is skipped",
			object:               newSecretObjectWithoutType(nil),
			wantShouldSkipObject: true,
			wantShouldSkipSecret: true,
		},
		{
			name:                 "configmap is not skipped by secret rules",
			object:               newConfigMapObject(),
			wantShouldSkipObject: false,
			wantShouldSkipSecret: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldSkipObject(tt.object, nil); got != tt.wantShouldSkipObject {
				t.Fatalf("ShouldSkipObject() = %v, want %v", got, tt.wantShouldSkipObject)
			}
			if got := ShouldSkipSecretObject(tt.object); got != tt.wantShouldSkipSecret {
				t.Fatalf("ShouldSkipSecretObject() = %v, want %v", got, tt.wantShouldSkipSecret)
			}
		})
	}
}

func TestSanitizeObjectForManifestCheckpoint(t *testing.T) {
	tests := []struct {
		name  string
		input *unstructured.Unstructured
		check func(t *testing.T, original, sanitized *unstructured.Unstructured)
	}{
		{
			name:  "nil returns nil",
			input: nil,
			check: func(t *testing.T, _, sanitized *unstructured.Unstructured) {
				t.Helper()
				if sanitized != nil {
					t.Fatal("expected nil input to return nil")
				}
			},
		},
		{
			name:  "non-secret configmap returns unchanged copy",
			input: newConfigMapObject(),
			check: func(t *testing.T, original, sanitized *unstructured.Unstructured) {
				t.Helper()
				if sanitized == nil {
					t.Fatal("expected sanitizer result")
				}
				if sanitized == original {
					t.Fatal("expected sanitizer to return a DeepCopy")
				}
				if !reflect.DeepEqual(sanitized.Object, original.Object) {
					t.Fatalf("expected non-Secret object unchanged, got %#v want %#v", sanitized.Object, original.Object)
				}
			},
		},
		{
			name:  "non-opaque secret with include-secret-data returns nil",
			input: newSecretObject("kubernetes.io/tls", map[string]string{AnnotationIncludeSecretData: "true"}),
			check: func(t *testing.T, _, sanitized *unstructured.Unstructured) {
				t.Helper()
				if sanitized != nil {
					t.Fatalf("expected non-Opaque Secret to sanitize to nil, got %#v", sanitized.Object)
				}
			},
		},
		{
			name:  "opaque include-secret removes data and stringData",
			input: newSecretObject("Opaque", map[string]string{AnnotationIncludeSecret: "true"}),
			check: func(t *testing.T, original, sanitized *unstructured.Unstructured) {
				t.Helper()
				if sanitized == nil {
					t.Fatal("expected sanitizer result")
				}
				assertNestedMapAbsent(t, sanitized.Object, "data")
				assertNestedMapAbsent(t, sanitized.Object, "stringData")
				if got, _, _ := unstructured.NestedString(sanitized.Object, "type"); got != "Opaque" {
					t.Fatalf("expected type to be preserved, got %q", got)
				}
				if sanitized.GetName() != "secret" || sanitized.GetNamespace() != "ns" {
					t.Fatalf("expected metadata to be preserved, got %s/%s", sanitized.GetNamespace(), sanitized.GetName())
				}
				if sanitized.GetAnnotations()[AnnotationIncludeSecret] != "true" {
					t.Fatal("expected include-secret annotation to be preserved")
				}
				assertNestedMapPresent(t, original.Object, "data")
				assertNestedMapPresent(t, original.Object, "stringData")
			},
		},
		{
			name:  "opaque include-secret-data preserves data and stringData",
			input: newSecretObject("Opaque", map[string]string{AnnotationIncludeSecretData: "true"}),
			check: func(t *testing.T, _, sanitized *unstructured.Unstructured) {
				t.Helper()
				if sanitized == nil {
					t.Fatal("expected sanitizer result")
				}
				if data := assertNestedMapPresent(t, sanitized.Object, "data"); data["password"] != "cGFzcw==" {
					t.Fatalf("expected data to be preserved, got %v", data)
				}
				if stringData := assertNestedMapPresent(t, sanitized.Object, "stringData"); stringData["token"] != "plain" {
					t.Fatalf("expected stringData to be preserved, got %v", stringData)
				}
			},
		},
		{
			name: "opaque without annotations removes data and stringData",
			// The real pipeline must filter such object before sanitize step.
			input: newSecretObject("Opaque", nil),
			check: func(t *testing.T, original, sanitized *unstructured.Unstructured) {
				t.Helper()
				if sanitized == nil {
					t.Fatal("expected sanitizer result")
				}
				assertNestedMapAbsent(t, sanitized.Object, "data")
				assertNestedMapAbsent(t, sanitized.Object, "stringData")
				assertNestedMapPresent(t, original.Object, "data")
				assertNestedMapPresent(t, original.Object, "stringData")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var original *unstructured.Unstructured
			if tt.input != nil {
				original = tt.input.DeepCopy()
			}
			sanitized := SanitizeObjectForManifestCheckpoint(tt.input)
			tt.check(t, tt.input, sanitized)
			if tt.input != nil {
				assertObjectUnchanged(t, tt.input, original)
			}
		})
	}
}
