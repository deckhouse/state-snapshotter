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

// TestListResidualRootOwnedPVCTargets_ExcludeVeto drives the residual root PVC leg through its public entry
// point and asserts the exclude veto drops PVCs carrying the label — value ignored — while leaving every
// other PVC in the residual set. This is the same lister that feeds both the volume-data leg and the root
// PVC manifest exclude set, so a regression here would half-capture a vetoed PVC (a volume node with no
// manifest, or a manifest with no data).
func TestListResidualRootOwnedPVCTargets_ExcludeVeto(t *testing.T) {
	t.Parallel()
	ns := "ns"
	pvcPlain := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pvc-plain", Namespace: ns, UID: "uid-plain"}}
	pvcLabeled := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pvc-labeled", Namespace: ns, UID: "uid-labeled", Labels: map[string]string{"group": "keep"}}}
	pvcVetoEmpty := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pvc-veto-empty", Namespace: ns, UID: "uid-veto-empty", Labels: map[string]string{storagev1alpha1.ExcludeLabelKey: ""}}}
	pvcVetoTrue := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pvc-veto-true", Namespace: ns, UID: "uid-veto-true", Labels: map[string]string{storagev1alpha1.ExcludeLabelKey: "true", "group": "keep"}}}

	// Residual root scope requires a root content with at least one child ref and a named root snapshot.
	rootContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "child-without-dataref"}},
		},
	}
	// The child has no dataRef, so it covers no PVC UID and leaves all PVCs in the root residual set.
	childContent := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "child-without-dataref"}}

	snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root-snap", Namespace: ns}}
	cl := fakeclient.NewClientBuilder().WithScheme(testSubtreeScheme(t)).
		WithObjects(pvcPlain, pvcLabeled, pvcVetoEmpty, pvcVetoTrue, rootContent, childContent, snap).Build()

	got, err := ListOwnedPVCTargetsForLogicalContent(context.Background(), cl, snap, rootContent, allKindsDataBearing)
	if err != nil {
		t.Fatalf("ListOwnedPVCTargetsForLogicalContent: %v", err)
	}
	uids := targetUIDs(got)

	for _, uid := range []string{"uid-plain", "uid-labeled"} {
		if _, ok := uids[uid]; !ok {
			t.Errorf("un-vetoed PVC %q must stay in the residual set, got %v", uid, uids)
		}
	}
	for _, uid := range []string{"uid-veto-empty", "uid-veto-true"} {
		if _, ok := uids[uid]; ok {
			t.Errorf("exclude-vetoed PVC %q must be dropped from the residual set, got %v", uid, uids)
		}
	}
	t.Logf("listed 4 namespace PVCs, %d survived the veto", len(uids))
}
