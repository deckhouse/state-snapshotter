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

package volumecapture

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

func targetUIDs(targets []vcpkg.Target) map[string]struct{} {
	out := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		out[t.UID] = struct{}{}
	}
	return out
}

// TestListResidualRootOwnedPVCTargets_resourceSelector drives the residual root PVC leg through its public
// entry point and asserts spec.resourceSelector narrows the captured PVCs. This is the same lister that
// feeds both the volume-data leg and the root PVC manifest exclude set, so filtering here keeps them aligned.
func TestListResidualRootOwnedPVCTargets_resourceSelector(t *testing.T) {
	t.Parallel()
	ns := "ns"
	pvcKeep := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pvc-keep", Namespace: ns, UID: "uid-keep", Labels: map[string]string{"group": "keep"}}}
	pvcDrop := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pvc-drop", Namespace: ns, UID: "uid-drop", Labels: map[string]string{"group": "drop"}}}
	pvcNoLabel := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pvc-nolabel", Namespace: ns, UID: "uid-nolabel"}}

	// Residual root scope requires a root content with at least one child ref and a named root snapshot.
	rootContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "child-without-dataref"}},
		},
	}
	// The child has no dataRef, so it covers no PVC UID and leaves all PVCs in the root residual set.
	childContent := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "child-without-dataref"}}

	run := func(t *testing.T, selector *metav1.LabelSelector) map[string]struct{} {
		t.Helper()
		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "root-snap", Namespace: ns},
			Spec:       storagev1alpha1.SnapshotSpec{ResourceSelector: selector},
		}
		cl := fakeclient.NewClientBuilder().WithScheme(testSubtreeScheme(t)).
			WithObjects(pvcKeep, pvcDrop, pvcNoLabel, rootContent, childContent, snap).Build()
		got, err := ListOwnedPVCTargetsForLogicalContent(context.Background(), cl, snap, rootContent, allKindsDataBearing)
		if err != nil {
			t.Fatalf("ListOwnedPVCTargetsForLogicalContent: %v", err)
		}
		return targetUIDs(got)
	}

	t.Run("nil selector captures all PVCs", func(t *testing.T) {
		got := run(t, nil)
		for _, uid := range []string{"uid-keep", "uid-drop", "uid-nolabel"} {
			if _, ok := got[uid]; !ok {
				t.Errorf("nil selector must capture %q, got %v", uid, got)
			}
		}
	})

	t.Run("empty selector captures all PVCs", func(t *testing.T) {
		got := run(t, &metav1.LabelSelector{})
		for _, uid := range []string{"uid-keep", "uid-drop", "uid-nolabel"} {
			if _, ok := got[uid]; !ok {
				t.Errorf("empty selector must capture %q, got %v", uid, got)
			}
		}
	})

	t.Run("matchLabels include keeps only matching PVC", func(t *testing.T) {
		got := run(t, &metav1.LabelSelector{MatchLabels: map[string]string{"group": "keep"}})
		if _, ok := got["uid-keep"]; !ok {
			t.Error("include selector must keep uid-keep")
		}
		for _, uid := range []string{"uid-drop", "uid-nolabel"} {
			if _, ok := got[uid]; ok {
				t.Errorf("include selector must drop %q, got %v", uid, got)
			}
		}
	})

	t.Run("NotIn exclude drops only matching PVC", func(t *testing.T) {
		got := run(t, &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "group", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"drop"}},
		}})
		if _, ok := got["uid-drop"]; ok {
			t.Error("NotIn (drop) must exclude uid-drop")
		}
		for _, uid := range []string{"uid-keep", "uid-nolabel"} {
			if _, ok := got[uid]; !ok {
				t.Errorf("NotIn (drop) must keep %q, got %v", uid, got)
			}
		}
	})
}
