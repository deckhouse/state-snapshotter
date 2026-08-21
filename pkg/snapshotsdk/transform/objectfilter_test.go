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

package transform

import (
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// gadget builds a namespaced custom object of kind "Gadget" in the given API group. The name is fixed to
// "g" on purpose: the ambiguity cases need the SAME kind+name present in two different groups.
func gadget(group string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": group + "/v1", "kind": "Gadget",
		"metadata": map[string]interface{}{"name": "g", "namespace": "restore-ns"},
	}}
}

func configMapObject(name string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": name, "namespace": "restore-ns"},
	}}
}

func TestFilterSingleObject_Hit(t *testing.T) {
	objects := []unstructured.Unstructured{configMapObject("app-config"), configMapObject("other")}

	out, err := FilterSingleObject(objects, Options{FilterKind: "ConfigMap", FilterName: "app-config"}, "source-ns/snap")
	if err != nil {
		t.Fatalf("filter hit: %v", err)
	}
	if len(out) != 1 || out[0].GetName() != "app-config" {
		t.Fatalf("filter returned %#v, want the single app-config ConfigMap", out)
	}
}

// TestFilterSingleObject_MissIsNotFound pins the 404 half of the contract: an object the compiled node
// does not contain is absent, not an error about the node. The message names both the object asked for
// and the node it was looked up in, because the caller sees nothing else of the failed request.
func TestFilterSingleObject_MissIsNotFound(t *testing.T) {
	objects := []unstructured.Unstructured{configMapObject("app-config")}

	_, err := FilterSingleObject(objects, Options{FilterKind: "ConfigMap", FilterName: "does-not-exist"}, "source-ns/snap")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a filter miss, got %v", err)
	}
	if !strings.Contains(err.Error(), "ConfigMap/does-not-exist") || !strings.Contains(err.Error(), "source-ns/snap") {
		t.Fatalf("not-found message must name the requested object and the node, got %q", err.Error())
	}
}

// TestFilterSingleObject_AmbiguousWithoutAPIVersion proves a kind+name matching two objects in different
// API groups is a fail-closed 400: the subresource promises ONE object, and picking either silently would
// restore the wrong one.
func TestFilterSingleObject_AmbiguousWithoutAPIVersion(t *testing.T) {
	objects := []unstructured.Unstructured{gadget("a.example.com"), gadget("b.example.com")}

	_, err := FilterSingleObject(objects, Options{FilterKind: "Gadget", FilterName: "g"}, "source-ns/snap")
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest for an ambiguous filter, got %v", err)
	}
	if !strings.Contains(err.Error(), "a.example.com/v1") || !strings.Contains(err.Error(), "b.example.com/v1") {
		t.Fatalf("ambiguity message must list the competing apiVersions, got %q", err.Error())
	}
}

func TestFilterSingleObject_APIVersionFullDisambiguates(t *testing.T) {
	objects := []unstructured.Unstructured{gadget("a.example.com"), gadget("b.example.com")}

	out, err := FilterSingleObject(objects, Options{FilterKind: "Gadget", FilterName: "g", FilterAPIVersion: "a.example.com/v1"}, "source-ns/snap")
	if err != nil {
		t.Fatalf("full apiVersion filter: %v", err)
	}
	if len(out) != 1 || out[0].GetAPIVersion() != "a.example.com/v1" {
		t.Fatalf("full apiVersion filter returned %#v, want the a.example.com/v1 Gadget", out)
	}
}

func TestFilterSingleObject_APIVersionBareGroupDisambiguates(t *testing.T) {
	objects := []unstructured.Unstructured{gadget("a.example.com"), gadget("b.example.com")}

	out, err := FilterSingleObject(objects, Options{FilterKind: "Gadget", FilterName: "g", FilterAPIVersion: "b.example.com"}, "source-ns/snap")
	if err != nil {
		t.Fatalf("bare group filter: %v", err)
	}
	if len(out) != 1 || out[0].GetAPIVersion() != "b.example.com/v1" {
		t.Fatalf("bare group filter returned %#v, want the b.example.com/v1 Gadget", out)
	}
}

// TestFilterSingleObject_AmbiguousWithAPIVersionStillFailsClosed covers the case an apiVersion cannot
// resolve — two objects of the same kind, name AND apiVersion, which a captured tree can hold because the
// filter runs on the compiled output of a node, not on a namespace index. It must stay a refusal, not a
// silent first match.
func TestFilterSingleObject_AmbiguousWithAPIVersionStillFailsClosed(t *testing.T) {
	objects := []unstructured.Unstructured{gadget("a.example.com"), gadget("a.example.com")}

	_, err := FilterSingleObject(objects, Options{FilterKind: "Gadget", FilterName: "g", FilterAPIVersion: "a.example.com/v1"}, "source-ns/snap")
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest when apiVersion cannot narrow the match, got %v", err)
	}
}
