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
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// lookupScheme registers the SVDM DataImport / DataImportList as unstructured so the fake client can
// List them cross-service exactly as the controller does at runtime (no Go-module dependency on SVDM).
func lookupScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	diGVK := schema.GroupVersionKind{Group: "storage-foundation.deckhouse.io", Version: "v1alpha1", Kind: "DataImport"}
	scheme.AddKnownTypeWithName(diGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: diGVK.Group, Version: diGVK.Version, Kind: "DataImportList"}, &unstructured.UnstructuredList{})
	return scheme
}

// dataImportTargeting builds a ProduceArtifact DataImport whose spec.snapshotRef points at a leaf by
// GroupKind+name (apiVersion carries the group as "group/version").
func dataImportTargeting(name, namespace, group, kind, targetName string) *unstructured.Unstructured {
	apiVersion := group + "/v1"
	if group == "" {
		apiVersion = "v1"
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "storage-foundation.deckhouse.io/v1alpha1",
		"kind":       "DataImport",
		"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
		"spec": map[string]interface{}{
			"mode": "ProduceArtifact",
			"snapshotRef": map[string]interface{}{
				"apiVersion": apiVersion,
				"kind":       kind,
				"name":       targetName,
			},
		},
	}}
}

// dataImportLegacyTargetRef builds a DataImport in the superseded shape: spec.targetRef is set (the old
// polymorphic target) and spec.snapshotRef is absent. The snapshotRef-based matcher must ignore it.
func dataImportLegacyTargetRef(name, namespace, group, kind, targetName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "storage-foundation.deckhouse.io/v1alpha1",
		"kind":       "DataImport",
		"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
		"spec": map[string]interface{}{
			"targetRef": map[string]interface{}{
				"group": group,
				"kind":  kind,
				"name":  targetName,
			},
		},
	}}
}

// leafObject builds a snapshot leaf with the given GVK / identity.
func leafObject(group, version, kind, name, namespace string) *unstructured.Unstructured {
	leaf := &unstructured.Unstructured{}
	leaf.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
	leaf.SetName(name)
	leaf.SetNamespace(namespace)
	return leaf
}

func TestFindDataImportForLeaf(t *testing.T) {
	const (
		ns          = "team-a"
		leafGroup   = "virtualization.deckhouse.io"
		leafVersion = "v1alpha2"
		leafKind    = "VirtualDiskSnapshot"
		leafName    = "vd-snap-1"
	)
	leaf := leafObject(leafGroup, leafVersion, leafKind, leafName, ns)

	tests := []struct {
		name        string
		dataImports []*unstructured.Unstructured
		wantMatch   string // expected DataImport name, "" when no match expected
		wantReason  string // expected terminal reason, "" when none
	}{
		{
			name: "no DataImport yet -> pending (nil, no reason)",
		},
		{
			name: "exactly one match by group+kind+name",
			dataImports: []*unstructured.Unstructured{
				dataImportTargeting("di-1", ns, leafGroup, leafKind, leafName),
			},
			wantMatch: "di-1",
		},
		{
			name: "wrong kind is ignored",
			dataImports: []*unstructured.Unstructured{
				dataImportTargeting("di-wrong-kind", ns, leafGroup, "VolumeSnapshot", leafName),
			},
		},
		{
			name: "legacy targetRef is ignored (spec-redesign guard)",
			dataImports: []*unstructured.Unstructured{
				// Superseded shape: spec.targetRef set (group+kind+name matching the leaf), no snapshotRef.
				// Under the old matcher this would have matched; the snapshotRef matcher must skip it.
				dataImportLegacyTargetRef("di-legacy", ns, leafGroup, leafKind, leafName),
			},
		},
		{
			name: "group mismatch is ignored",
			dataImports: []*unstructured.Unstructured{
				dataImportTargeting("di-wrong-group", ns, "snapshot.storage.k8s.io", leafKind, leafName),
			},
		},
		{
			name: "name mismatch is ignored",
			dataImports: []*unstructured.Unstructured{
				dataImportTargeting("di-wrong-name", ns, leafGroup, leafKind, "some-other-leaf"),
			},
		},
		{
			name: "two matching DataImports -> ambiguous fail-closed",
			dataImports: []*unstructured.Unstructured{
				dataImportTargeting("di-1", ns, leafGroup, leafKind, leafName),
				dataImportTargeting("di-2", ns, leafGroup, leafKind, leafName),
			},
			wantReason: snapshot.ReasonDataImportAmbiguous,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			objs := make([]client.Object, 0, len(tt.dataImports))
			for _, di := range tt.dataImports {
				objs = append(objs, di)
			}
			cl := fake.NewClientBuilder().WithScheme(lookupScheme()).WithObjects(objs...).Build()

			di, reason, msg, err := FindDataImportForLeaf(context.Background(), cl, leaf)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantReason != "" {
				if reason != tt.wantReason {
					t.Fatalf("want terminal reason %q, got %q (msg=%q)", tt.wantReason, reason, msg)
				}
				if di != nil {
					t.Fatalf("ambiguous match must return nil DataImport, got %q", di.GetName())
				}
				if !strings.Contains(msg, leafKind) {
					t.Fatalf("ambiguous message should name the leaf kind %q, got %q", leafKind, msg)
				}
				return
			}

			if reason != "" {
				t.Fatalf("unexpected terminal reason %q (msg=%q)", reason, msg)
			}
			if tt.wantMatch == "" {
				if di != nil {
					t.Fatalf("expected no match, got %q", di.GetName())
				}
				return
			}
			if di == nil || di.GetName() != tt.wantMatch {
				t.Fatalf("want match %q, got %v", tt.wantMatch, di)
			}
		})
	}
}

// TestFindDataImportForLeaf_NamespaceScoped verifies the reverse-lookup only considers DataImports in the
// leaf's own namespace (snapshotRef namespace is implicit = leaf namespace), so a same-identity DataImport
// in another namespace must not match.
func TestFindDataImportForLeaf_NamespaceScoped(t *testing.T) {
	leaf := leafObject("snapshot.storage.k8s.io", "v1", "VolumeSnapshot", "snap", "ns-a")
	di := dataImportTargeting("di-other-ns", "ns-b", "snapshot.storage.k8s.io", "VolumeSnapshot", "snap")
	cl := fake.NewClientBuilder().WithScheme(lookupScheme()).WithObjects(di).Build()

	got, reason, _, err := FindDataImportForLeaf(context.Background(), cl, leaf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil || reason != "" {
		t.Fatalf("DataImport in another namespace must not match; got di=%v reason=%q", got, reason)
	}
}
