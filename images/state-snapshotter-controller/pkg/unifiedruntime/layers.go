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

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/dscregistry"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// LayeredGVKState is the explicit desired → eligible → merged desired → resolved split from
// docs/state-snapshotter-rework/design/r2-phase-2b-r3-runtime-registry.md. It does not include
// controller-runtime wiring; "active" watches are tracked separately on the Syncer.
type LayeredGVKState struct {
	// BootstrapDesired is a copy of the static bootstrap list passed into the syncer.
	BootstrapDesired []unifiedbootstrap.UnifiedGVKPair
	// EligibleFromDSC is dscregistry.EligibleUnifiedGVKPairs (Accepted+RBACReady+generation; CRD-valid rows only).
	EligibleFromDSC []unifiedbootstrap.UnifiedGVKPair
	// DSCEligibleError is set when List/parse of DSC failed; merge then uses bootstrap only.
	DSCEligibleError error
	// DesiredMerged is MergeBootstrapAndDSCPairs(bootstrap, EligibleFromDSC or nil on list error).
	DesiredMerged []unifiedbootstrap.UnifiedGVKPair
	// ResolvedSnapshotGVKs and ResolvedContentGVKs are index-aligned; produced by RESTMapper filter.
	ResolvedSnapshotGVKs []schema.GroupVersionKind
	ResolvedContentGVKs  []schema.GroupVersionKind
}

// BuildLayeredGVKState reads DSC (eligible pairs), merges with bootstrap, resolves against the API mapper.
func BuildLayeredGVKState(
	ctx context.Context,
	reader client.Reader,
	mapper meta.RESTMapper,
	bootstrap []unifiedbootstrap.UnifiedGVKPair,
	log logr.Logger,
) LayeredGVKState {
	st := LayeredGVKState{
		BootstrapDesired: append([]unifiedbootstrap.UnifiedGVKPair(nil), bootstrap...),
	}
	eligible, err := dscregistry.EligibleUnifiedGVKPairs(ctx, reader)
	if err != nil {
		st.DSCEligibleError = err
		st.EligibleFromDSC = nil
	} else {
		st.EligibleFromDSC = append([]unifiedbootstrap.UnifiedGVKPair(nil), eligible...)
	}
	dscForMerge := st.EligibleFromDSC
	if st.DSCEligibleError != nil {
		dscForMerge = nil
	}
	st.DesiredMerged = unifiedbootstrap.MergeBootstrapAndDSCPairs(bootstrap, dscForMerge)
	st.ResolvedSnapshotGVKs, st.ResolvedContentGVKs = unifiedbootstrap.ResolveAvailableUnifiedGVKPairs(
		mapper, st.DesiredMerged, log)
	return st
}

// ResolvedSnapshotKeySet returns a set of ResolvedSnapshotGVKs[i].String() for cheap diffing.
func (s LayeredGVKState) ResolvedSnapshotKeySet() map[string]struct{} {
	m := make(map[string]struct{}, len(s.ResolvedSnapshotGVKs))
	for _, gvk := range s.ResolvedSnapshotGVKs {
		m[gvk.String()] = struct{}{}
	}
	return m
}
