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

package unifiedruntime

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

func TestLayeredGVKState_ResolvedSnapshotKeySet(t *testing.T) {
	s := LayeredGVKState{
		ResolvedSnapshotGVKs: []schema.GroupVersionKind{
			{Group: "g", Version: "v1", Kind: "A"},
			{Group: "g", Version: "v1", Kind: "B"},
		},
	}
	m := s.ResolvedSnapshotKeySet()
	if len(m) != 2 {
		t.Fatalf("len %d", len(m))
	}
	if _, ok := m["g/v1, Kind=A"]; !ok {
		t.Fatalf("missing key: %#v", m)
	}
}

func TestBuildLayeredGVKState_DSCListErrorFallsBackToBootstrap(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	// No state-snapshotter types: List DSC fails.
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	bootstrap := []unifiedbootstrap.UnifiedGVKPair{
		{
			Snapshot:        schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "Snapshot"},
			SnapshotContent: schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "SnapshotContent"},
		},
	}
	gv := schema.GroupVersion{Group: "storage.deckhouse.io", Version: "v1alpha1"}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	mapper.Add(bootstrap[0].Snapshot, meta.RESTScopeNamespace)
	mapper.Add(bootstrap[0].SnapshotContent, meta.RESTScopeRoot)

	st := BuildLayeredGVKState(ctx, c, mapper, bootstrap, logr.Discard())
	if st.DSCEligibleError == nil {
		t.Fatal("expected DSC list error")
	}
	if len(st.EligibleFromDSC) != 0 {
		t.Fatalf("eligible: %d", len(st.EligibleFromDSC))
	}
	if len(st.DesiredMerged) != 1 || st.DesiredMerged[0].Snapshot != bootstrap[0].Snapshot {
		t.Fatalf("merge: %+v", st.DesiredMerged)
	}
	if len(st.ResolvedSnapshotGVKs) != 1 {
		t.Fatalf("resolved snaps: %d", len(st.ResolvedSnapshotGVKs))
	}
}

func TestBuildLayeredGVKState_EmptyDSCListMergesBootstrap(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = storagev1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	bootstrap := unifiedbootstrap.DefaultDesiredUnifiedSnapshotPairs()
	gv := schema.GroupVersion{Group: "storage.deckhouse.io", Version: "v1alpha1"}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	snap := schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: "Snapshot"}
	content := schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: "SnapshotContent"}
	mapper.Add(snap, meta.RESTScopeNamespace)
	mapper.Add(content, meta.RESTScopeRoot)

	st := BuildLayeredGVKState(ctx, c, mapper, bootstrap, logr.Discard())
	if st.DSCEligibleError != nil {
		t.Fatalf("unexpected err: %v", st.DSCEligibleError)
	}
	if len(st.EligibleFromDSC) != 0 {
		t.Fatalf("eligible dsc: %d", len(st.EligibleFromDSC))
	}
	if len(st.ResolvedSnapshotGVKs) < 1 {
		t.Fatalf("expected at least Snapshot pair resolved, got %d", len(st.ResolvedSnapshotGVKs))
	}
}
