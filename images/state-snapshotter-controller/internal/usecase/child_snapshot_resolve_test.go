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

package usecase

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func TestResolveChildSnapshotToBoundContentName_Ambiguous(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	reg := snapshot.NewGVKRegistry()
	if err := reg.RegisterSnapshotContentMapping(
		"SyntheticDomainSnapshotA", "generic.state-snapshotter.test/v1",
		"SyntheticDomainSnapshotContentA", "generic.state-snapshotter.test/v1",
	); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterSnapshotContentMapping(
		"SyntheticDomainSnapshotB", "generic.state-snapshotter.test/v1",
		"SyntheticDomainSnapshotContentB", "generic.state-snapshotter.test/v1",
	); err != nil {
		t.Fatal(err)
	}

	a := syntheticSnapshotUnstructured("same-name", "content-a")
	a.SetGroupVersionKind(schema.GroupVersionKind{Group: "generic.state-snapshotter.test", Version: "v1", Kind: "SyntheticDomainSnapshotA"})
	b := syntheticSnapshotUnstructured("same-name", "content-b")
	b.SetGroupVersionKind(schema.GroupVersionKind{Group: "generic.state-snapshotter.test", Version: "v1", Kind: "SyntheticDomainSnapshotB"})

	cl := fake.NewClientBuilder().WithRuntimeObjects(a, b).Build()
	_, err := ResolveChildSnapshotToBoundContentName(ctx, cl, reg, "ns1", "same-name")
	if err == nil {
		t.Fatal("expected ambiguous error")
	}
	if !errors.Is(err, ErrRunGraphAmbiguousChildSnapshotRef) {
		t.Fatalf("expected ErrRunGraphAmbiguousChildSnapshotRef, got %v", err)
	}
}
