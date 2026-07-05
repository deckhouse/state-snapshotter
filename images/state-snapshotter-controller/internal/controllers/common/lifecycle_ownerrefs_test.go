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

package common //nolint:revive // package name matches internal/controllers/common directory

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
)

func TestRootObjectKeeperNameIsDNS1123Safe(t *testing.T) {
	uids := []types.UID{
		"snap-uid-1",
		"example.deckhouse.io/v1alpha1|WidgetSnapshot|ns1/snap",
	}
	for _, uid := range uids {
		got := RootObjectKeeperName(uid)
		if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 {
			t.Fatalf("RootObjectKeeperName(%q) produced invalid metadata.name %q: %v", uid, got, errs)
		}
	}
	// Deterministic per UID; distinct UIDs give distinct names.
	if RootObjectKeeperName("a") != RootObjectKeeperName("a") {
		t.Fatal("RootObjectKeeperName not deterministic")
	}
	if RootObjectKeeperName("a") == RootObjectKeeperName("b") {
		t.Fatal("RootObjectKeeperName collides for distinct UIDs")
	}
}

func TestRootObjectKeeperTTLUsesDefaultForNilConfig(t *testing.T) {
	if got := RootObjectKeeperTTL(nil); got != config.DefaultSnapshotRootOKTTL {
		t.Fatalf("RootObjectKeeperTTL(nil) = %s, want %s", got, config.DefaultSnapshotRootOKTTL)
	}
}

func TestLifecycleOwnerRefsPreservesUnrelatedRefs(t *testing.T) {
	unrelated := metav1.OwnerReference{APIVersion: "example.io/v1", Kind: "AuditAnchor", Name: "audit"}
	desired := metav1.OwnerReference{APIVersion: DeckhouseAPIVersion, Kind: KindObjectKeeper, Name: "ret-snap", Controller: boolPtr(true)}

	got, changed, err := lifecycleOwnerRefs([]metav1.OwnerReference{unrelated}, desired)
	if err != nil {
		t.Fatalf("lifecycleOwnerRefs failed: %v", err)
	}
	if !changed {
		t.Fatal("expected lifecycleOwnerRefs to report changed")
	}
	assertOwnerRefPresentInLifecycleTest(t, got, unrelated.APIVersion, unrelated.Kind, unrelated.Name)
	assertOwnerRefPresentInLifecycleTest(t, got, desired.APIVersion, desired.Kind, desired.Name)
}

func TestLifecycleOwnerRefsIsIdempotent(t *testing.T) {
	desired := metav1.OwnerReference{APIVersion: DeckhouseAPIVersion, Kind: KindObjectKeeper, Name: "ret-snap", UID: "ok-uid", Controller: boolPtr(true)}

	got, changed, err := lifecycleOwnerRefs([]metav1.OwnerReference{desired}, desired)
	if err != nil {
		t.Fatalf("lifecycleOwnerRefs failed: %v", err)
	}
	if changed {
		t.Fatalf("expected idempotent desired ownerRef, got changed refs %#v", got)
	}
}

func TestLifecycleOwnerRefsRejectsAnySnapshotOwnerForContentHandoff(t *testing.T) {
	existing := []metav1.OwnerReference{{
		APIVersion: "custom.deckhouse.io/v1",
		Kind:       "ArbitrarySnapshot",
		Name:       "child-snapshot",
		Controller: boolPtr(true),
	}}
	desired := metav1.OwnerReference{APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "SnapshotContent", Name: "parent-content", Controller: boolPtr(true)}

	if _, _, err := lifecycleOwnerRefs(existing, desired); err == nil {
		t.Fatal("expected Snapshot -> SnapshotContent lifecycle handoff to fail closed")
	}
}

func TestLifecycleOwnerRefsRejectsConflictingObjectKeeper(t *testing.T) {
	existing := []metav1.OwnerReference{{
		APIVersion: DeckhouseAPIVersion,
		Kind:       KindObjectKeeper,
		Name:       "ret-old",
		UID:        "old-uid",
		Controller: boolPtr(true),
	}}
	desired := metav1.OwnerReference{APIVersion: DeckhouseAPIVersion, Kind: KindObjectKeeper, Name: "ret-new", UID: "new-uid", Controller: boolPtr(true)}

	if _, _, err := lifecycleOwnerRefs(existing, desired); err == nil {
		t.Fatal("expected conflicting ObjectKeeper ownerRef to fail closed")
	}
}

func TestLifecycleOwnerRefsRejectsConflictingParentSnapshotContent(t *testing.T) {
	existing := []metav1.OwnerReference{{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "SnapshotContent",
		Name:       "old-parent",
		UID:        "old-uid",
		Controller: boolPtr(true),
	}}
	desired := metav1.OwnerReference{APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "SnapshotContent", Name: "new-parent", UID: "new-uid", Controller: boolPtr(true)}

	if _, _, err := lifecycleOwnerRefs(existing, desired); err == nil {
		t.Fatal("expected conflicting parent SnapshotContent ownerRef to fail closed")
	}
}

func TestLifecycleOwnerRefsAllowsSameOwnerUIDCompletion(t *testing.T) {
	existing := []metav1.OwnerReference{{
		APIVersion: DeckhouseAPIVersion,
		Kind:       KindObjectKeeper,
		Name:       "ret-snap",
		Controller: boolPtr(true),
	}}
	desired := metav1.OwnerReference{APIVersion: DeckhouseAPIVersion, Kind: KindObjectKeeper, Name: "ret-snap", UID: "ok-uid", Controller: boolPtr(true)}

	got, changed, err := lifecycleOwnerRefs(existing, desired)
	if err != nil {
		t.Fatalf("lifecycleOwnerRefs failed: %v", err)
	}
	if !changed {
		t.Fatal("expected UID completion to report changed")
	}
	if len(got) != 1 || got[0].UID != desired.UID {
		t.Fatalf("expected desired ownerRef with completed UID, got %#v", got)
	}
}

func assertOwnerRefPresentInLifecycleTest(t *testing.T, refs []metav1.OwnerReference, apiVersion, kind, name string) {
	t.Helper()
	for _, ref := range refs {
		if ref.APIVersion == apiVersion && ref.Kind == kind && ref.Name == name {
			return
		}
	}
	t.Fatalf("expected ownerRef %s/%s/%s in %#v", apiVersion, kind, name, refs)
}

func boolPtr(v bool) *bool {
	return &v
}
