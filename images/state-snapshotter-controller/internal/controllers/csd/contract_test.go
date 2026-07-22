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

package csd

import (
	"strings"
	"testing"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func statusSchema(fields ...string) *extv1.JSONSchemaProps {
	props := map[string]extv1.JSONSchemaProps{}
	for _, f := range fields {
		props[f] = extv1.JSONSchemaProps{Type: "string"}
	}
	return &extv1.JSONSchemaProps{Type: "object", Properties: props}
}

func TestMissingSnapshotStatusContractFields(t *testing.T) {
	t.Run("all present", func(t *testing.T) {
		if got := missingSnapshotStatusContractFields(statusSchema("boundSnapshotContentName", "conditions", "childrenSnapshotRefs")); len(got) != 0 {
			t.Fatalf("expected none missing, got %v", got)
		}
	})
	t.Run("leaf without childrenSnapshotRefs is satisfied", func(t *testing.T) {
		if got := missingSnapshotStatusContractFields(statusSchema("boundSnapshotContentName", "conditions")); len(got) != 0 {
			t.Fatalf("childrenSnapshotRefs must not be required, got missing %v", got)
		}
	})
	t.Run("missing boundSnapshotContentName", func(t *testing.T) {
		got := missingSnapshotStatusContractFields(statusSchema("conditions"))
		if len(got) != 1 || got[0] != "status.boundSnapshotContentName" {
			t.Fatalf("expected status.boundSnapshotContentName missing, got %v", got)
		}
	})
	t.Run("nil schema is lenient", func(t *testing.T) {
		if got := missingSnapshotStatusContractFields(nil); got != nil {
			t.Fatalf("nil schema must be lenient, got %v", got)
		}
	})
	t.Run("empty properties is lenient", func(t *testing.T) {
		if got := missingSnapshotStatusContractFields(&extv1.JSONSchemaProps{Type: "object"}); got != nil {
			t.Fatalf("empty schema must be lenient, got %v", got)
		}
	})
	t.Run("preserve unknown fields is lenient", func(t *testing.T) {
		preserve := true
		s := statusSchema("conditions")
		s.XPreserveUnknownFields = &preserve
		if got := missingSnapshotStatusContractFields(s); got != nil {
			t.Fatalf("preserve-unknown status must be lenient, got %v", got)
		}
	})
}

func crdWithVersionSchema(version string, root *extv1.JSONSchemaProps) *extv1.CustomResourceDefinition { //nolint:unparam // test fixture keeps uniform signature
	var schema *extv1.CustomResourceValidation
	if root != nil {
		schema = &extv1.CustomResourceValidation{OpenAPIV3Schema: root}
	}
	return &extv1.CustomResourceDefinition{
		Spec: extv1.CustomResourceDefinitionSpec{
			Versions: []extv1.CustomResourceDefinitionVersion{
				{Name: version, Schema: schema},
			},
		},
	}
}

func TestCRDStatusSchemaForVersion(t *testing.T) {
	t.Run("structural schema with status", func(t *testing.T) {
		crd := crdWithVersionSchema("v1alpha1", &extv1.JSONSchemaProps{
			Type: "object",
			Properties: map[string]extv1.JSONSchemaProps{
				"status": *statusSchema("boundSnapshotContentName", "conditions"),
			},
		})
		got, inspectable, declared := crdStatusSchemaForVersion(crd, "v1alpha1")
		if !inspectable || !declared || got == nil || len(got.Properties) != 2 {
			t.Fatalf("expected inspectable status schema with 2 props, got inspectable=%v declared=%v schema=%#v", inspectable, declared, got)
		}
	})
	t.Run("unknown version is not inspectable", func(t *testing.T) {
		crd := crdWithVersionSchema("v1alpha1", &extv1.JSONSchemaProps{Type: "object", Properties: map[string]extv1.JSONSchemaProps{"spec": {Type: "object"}}})
		if _, inspectable, _ := crdStatusSchemaForVersion(crd, "v2"); inspectable {
			t.Fatal("unknown version must not be inspectable")
		}
	})
	t.Run("structural schema without status is inspectable but undeclared", func(t *testing.T) {
		crd := crdWithVersionSchema("v1alpha1", &extv1.JSONSchemaProps{
			Type:       "object",
			Properties: map[string]extv1.JSONSchemaProps{"spec": {Type: "object"}},
		})
		schema, inspectable, declared := crdStatusSchemaForVersion(crd, "v1alpha1")
		if !inspectable || declared || schema != nil {
			t.Fatalf("expected inspectable but undeclared status, got inspectable=%v declared=%v schema=%#v", inspectable, declared, schema)
		}
	})
	t.Run("absent schema is not inspectable", func(t *testing.T) {
		crd := crdWithVersionSchema("v1alpha1", nil)
		if _, inspectable, _ := crdStatusSchemaForVersion(crd, "v1alpha1"); inspectable {
			t.Fatal("absent OpenAPIV3Schema must not be inspectable")
		}
	})
	t.Run("preserve-unknown root is not inspectable", func(t *testing.T) {
		preserve := true
		crd := crdWithVersionSchema("v1alpha1", &extv1.JSONSchemaProps{XPreserveUnknownFields: &preserve})
		if _, inspectable, _ := crdStatusSchemaForVersion(crd, "v1alpha1"); inspectable {
			t.Fatal("preserve-unknown root must not be inspectable")
		}
	})
}

func TestEvaluateSnapshotStatusContract(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "sds-unified-snapshots-poc.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualDiskSnapshot"}

	t.Run("structural schema with contract fields is satisfied", func(t *testing.T) {
		crd := crdWithVersionSchema("v1alpha1", &extv1.JSONSchemaProps{
			Type: "object",
			Properties: map[string]extv1.JSONSchemaProps{
				"status": *statusSchema("boundSnapshotContentName", "conditions"),
			},
		})
		if err := evaluateSnapshotStatusContract(crd, gvk); err != nil {
			t.Fatalf("expected satisfied, got %v", err)
		}
	})
	t.Run("structural schema without status block is unsatisfied", func(t *testing.T) {
		crd := crdWithVersionSchema("v1alpha1", &extv1.JSONSchemaProps{
			Type:       "object",
			Properties: map[string]extv1.JSONSchemaProps{"spec": {Type: "object"}},
		})
		err := evaluateSnapshotStatusContract(crd, gvk)
		if err == nil {
			t.Fatal("structural CRD without status must be SnapshotContractUnsatisfied")
		}
		if !strings.Contains(err.Error(), "status.boundSnapshotContentName") || !strings.Contains(err.Error(), "status.conditions") {
			t.Fatalf("expected both canonical fields reported missing, got %v", err)
		}
	})
	t.Run("structural status missing a field is unsatisfied", func(t *testing.T) {
		crd := crdWithVersionSchema("v1alpha1", &extv1.JSONSchemaProps{
			Type: "object",
			Properties: map[string]extv1.JSONSchemaProps{
				"status": *statusSchema("conditions"),
			},
		})
		err := evaluateSnapshotStatusContract(crd, gvk)
		if err == nil || !strings.Contains(err.Error(), "status.boundSnapshotContentName") {
			t.Fatalf("expected boundSnapshotContentName reported missing, got %v", err)
		}
	})
	t.Run("schemaless CRD stays lenient", func(t *testing.T) {
		preserve := true
		crd := crdWithVersionSchema("v1alpha1", &extv1.JSONSchemaProps{XPreserveUnknownFields: &preserve})
		if err := evaluateSnapshotStatusContract(crd, gvk); err != nil {
			t.Fatalf("schemaless CRD must stay lenient, got %v", err)
		}
	})
	t.Run("absent schema stays lenient", func(t *testing.T) {
		crd := crdWithVersionSchema("v1alpha1", nil)
		if err := evaluateSnapshotStatusContract(crd, gvk); err != nil {
			t.Fatalf("absent schema must stay lenient, got %v", err)
		}
	})
}
