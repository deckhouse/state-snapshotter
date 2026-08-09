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

// dataImportTargeting builds a PopulateData DataImport whose spec.snapshotRef points at a leaf by
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
			"mode": "PopulateData",
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

// realisticDataImport builds a DataImport with the FULL spec storage-foundation actually serves (ttl,
// waitForFirstConsumer, mode, snapshotRef, storageParams), not just the field under test. The defect this
// helper replaces was reading a path that does not exist on the object at all, so the fixture must be a
// realistic object rather than a map shaped around the assertion — otherwise a wrong path could still pass
// by accident. A nil storageParams omits the whole block; extra sets additional top-level spec fields.
func realisticDataImport(mode string, storageParams, extra map[string]interface{}) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"ttl":                  "24h",
		"waitForFirstConsumer": false,
		"publish":              false,
		"snapshotRef": map[string]interface{}{
			"apiVersion": "virtualization.deckhouse.io/v1alpha2",
			"kind":       "VirtualDiskSnapshot",
			"name":       "vd-snap-1",
		},
	}
	if mode != "" {
		spec["mode"] = mode
	}
	if storageParams != nil {
		spec["storageParams"] = storageParams
	}
	for k, v := range extra {
		spec[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "storage-foundation.deckhouse.io/v1alpha1",
		"kind":       "DataImport",
		"metadata":   map[string]interface{}{"name": "di-1", "namespace": "team-a"},
		"spec":       spec,
	}}
}

// scratchStorageParams is a complete PopulateData spec.storageParams block.
func scratchStorageParams(storageClassName string) map[string]interface{} {
	params := map[string]interface{}{"size": "10Gi", "volumeMode": "Filesystem"}
	if storageClassName != "" {
		params["storageClassName"] = storageClassName
	}
	return params
}

// ImportStorageClassName must read the class from spec.storageParams.storageClassName — and from nowhere
// else. Two regressions are pinned here: (1) the original bug, where the top-level spec.storageClassName was
// read and always yielded "" because the field does not exist on the CRD; (2) the mode gate, which keeps the
// read fail-closed on this CRD's own discriminator instead of resting on a CEL rule owned by another
// repository.
func TestImportStorageClassName(t *testing.T) {
	tests := []struct {
		name string
		di   *unstructured.Unstructured
		want string
	}{
		{
			name: "nil DataImport (not resolved) yields empty",
			di:   nil,
		},
		{
			name: "PopulateData with storageParams.storageClassName",
			di:   realisticDataImport("PopulateData", scratchStorageParams("sc-import"), nil),
			want: "sc-import",
		},
		{
			name: "PopulateData whose storageParams carry no storageClassName",
			di:   realisticDataImport("PopulateData", scratchStorageParams(""), nil),
		},
		{
			name: "PopulateData with no storageParams block at all",
			di:   realisticDataImport("PopulateData", nil, nil),
		},
		{
			// The original defect: spec.storageClassName is not a field of this CRD. Even if some producer
			// wrote it, the helper must not read it — the scratch class is the authoritative one.
			name: "top-level spec.storageClassName is NOT the path",
			di:   realisticDataImport("PopulateData", nil, map[string]interface{}{"storageClassName": "sc-top-level"}),
		},
		{
			// CEL forbids storageParams in CreatePVC, so this object cannot exist in a real cluster; the gate
			// exists precisely so the helper does not depend on that guarantee holding in another repository.
			name: "CreatePVC ignores storageParams (fail-closed mode gate)",
			di:   realisticDataImport("CreatePVC", scratchStorageParams("sc-scratch"), nil),
		},
		{
			name: "empty mode defaults to CreatePVC and is ignored",
			di:   realisticDataImport("", scratchStorageParams("sc-scratch"), nil),
		},
		{
			name: "an unknown future mode is ignored",
			di:   realisticDataImport("SomeFutureMode", scratchStorageParams("sc-scratch"), nil),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ImportStorageClassName(tt.di); got != tt.want {
				t.Fatalf("ImportStorageClassName = %q, want %q", got, tt.want)
			}
		})
	}
}

// A DataImport the reverse-lookup actually returned must feed the class helper end to end: the two are
// always used as a pair (find the leaf's DataImport, then read its scratch class), so the pairing is
// covered rather than only each half in isolation.
func TestFindDataImportForLeaf_FeedsImportStorageClassName(t *testing.T) {
	leaf := leafObject("virtualization.deckhouse.io", "v1alpha2", "VirtualDiskSnapshot", "vd-snap-1", "team-a")
	di := realisticDataImport("PopulateData", scratchStorageParams("sc-import"), nil)
	cl := fake.NewClientBuilder().WithScheme(lookupScheme()).WithObjects(di).Build()

	got, reason, _, err := FindDataImportForLeaf(context.Background(), cl, leaf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason != "" || got == nil {
		t.Fatalf("expected exactly one match, got di=%v reason=%q", got, reason)
	}
	if sc := ImportStorageClassName(got); sc != "sc-import" {
		t.Fatalf("ImportStorageClassName on the looked-up DataImport = %q, want sc-import", sc)
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
