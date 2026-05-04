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

package snapshotgraphregistry

import (
	"context"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme storage: %v", err)
	}
	if err := ssv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme v1alpha1: %v", err)
	}
	return s
}

func namespaceSnapshotRESTMapper(t *testing.T) meta.RESTMapper {
	t.Helper()
	gv := schema.GroupVersion{Group: storagev1alpha1.APIGroup, Version: storagev1alpha1.APIVersion}
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	snap := schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: "NamespaceSnapshot"}
	content := schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: "SnapshotContent"}
	m.Add(snap, meta.RESTScopeNamespace)
	m.Add(content, meta.RESTScopeRoot)
	return m
}

func TestProvider_CurrentNilBeforeRefresh(t *testing.T) {
	mapper := namespaceSnapshotRESTMapper(t)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	pair := unifiedbootstrap.UnifiedGVKPair{
		Snapshot:        schema.GroupVersionKind{Group: storagev1alpha1.APIGroup, Version: storagev1alpha1.APIVersion, Kind: "NamespaceSnapshot"},
		SnapshotContent: schema.GroupVersionKind{Group: storagev1alpha1.APIGroup, Version: storagev1alpha1.APIVersion, Kind: "SnapshotContent"},
	}
	cfg := &config.Options{
		UnifiedBootstrapMode:        config.UnifiedBootstrapCustom,
		UnifiedBootstrapCustomPairs: []unifiedbootstrap.UnifiedGVKPair{pair},
	}
	p, err := NewProvider(cfg, mapper, cl, logr.Discard())
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if p.Current() != nil {
		t.Fatal("expected nil Current before first Refresh")
	}
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if p.Current() == nil {
		t.Fatal("expected non-nil Current after Refresh")
	}
}

func TestProvider_ReplaceCurrentSwapsAtomically(t *testing.T) {
	mapper := namespaceSnapshotRESTMapper(t)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	cfg := &config.Options{UnifiedBootstrapMode: config.UnifiedBootstrapEmpty}
	p, err := NewProvider(cfg, mapper, cl, logr.Discard())
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	r1 := snapshot.NewGVKRegistry()
	r2 := snapshot.NewGVKRegistry()
	p.ReplaceCurrent(r1)
	if p.Current() != r1 {
		t.Fatal("expected r1")
	}
	p.ReplaceCurrent(r2)
	if p.Current() != r2 {
		t.Fatal("expected r2")
	}
}

func TestProvider_ConcurrentCurrentDuringReplaceCurrent(t *testing.T) {
	mapper := namespaceSnapshotRESTMapper(t)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	cfg := &config.Options{UnifiedBootstrapMode: config.UnifiedBootstrapEmpty}
	p, err := NewProvider(cfg, mapper, cl, logr.Discard())
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	for round := 0; round < 20; round++ {
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 50; j++ {
					_ = p.Current()
				}
			}()
		}
		for i := 0; i < 10; i++ {
			p.ReplaceCurrent(snapshot.NewGVKRegistry())
		}
		wg.Wait()
	}
}

func TestStatic_ConcurrentCurrent(t *testing.T) {
	r := snapshot.NewGVKRegistry()
	s := NewStatic(r)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = s.Current()
			}
		}()
	}
	wg.Wait()
	if s.Current() != r {
		t.Fatal("registry pointer changed unexpectedly")
	}
}
