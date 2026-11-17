//go:build integration
// +build integration

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

package integration

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// The exclude label keeps an object out of a namespace snapshot, and the snapshot records it: the root's
// manifest leg and residual PVC leg leave labelled objects out and list them in excludedRefs — on the root
// SnapshotContent (the durable aggregate) and on the root Snapshot (its mirror). Objects the built-in
// rules drop anyway are not recorded even when labelled.
var _ = Describe("Integration: Snapshot records label-excluded objects in excludedRefs", func() {
	It("lists the labelled ConfigMap and PVC in excludedRefs and captures neither", func() {
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-excluded-refs-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "snapshot-excluded-refs",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		veto := map[string]string{storagev1alpha1.ExcludeLabelKey: "true"}
		controller := true
		for _, cm := range []*corev1.ConfigMap{
			{ObjectMeta: metav1.ObjectMeta{Name: "cm-plain", Namespace: nsName}},
			{ObjectMeta: metav1.ObjectMeta{Name: "cm-vetoed", Namespace: nsName, Labels: veto}},
			// Controller-owned: dropped by the built-in rules, so its label is not a user exclusion.
			{ObjectMeta: metav1.ObjectMeta{Name: "cm-owned-vetoed", Namespace: nsName, Labels: veto, OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "rs-uid", Controller: &controller,
			}}}},
		} {
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())
		}
		// A PVC without a storage class: were it not excluded, the orphan wave would fail to resolve its
		// VolumeSnapshotClass and the root would not become Ready.
		Expect(k8sClient.Create(ctx, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-vetoed", Namespace: nsName, Labels: veto},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		})).To(Succeed())

		snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: nsName}}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		key := types.NamespacedName{Namespace: nsName, Name: snap.Name}

		want := []storagev1alpha1.ExcludedObjectRef{
			{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-vetoed"},
			{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-vetoed"},
		}
		var root storagev1alpha1.Snapshot
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, key, &root)).To(Succeed())
			ready := meta.FindStatusCondition(root.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal(snapshot.ReasonCompleted))
			g.Expect(root.Status.ChildrenSnapshotRefs).To(BeEmpty(), "a labelled PVC must not get an orphan VolumeSnapshot")
			g.Expect(root.Status.ExcludedRefs).To(Equal(want), "the root mirror lists the label-excluded objects, sorted")
		}).WithTimeout(120 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		Expect(root.Status.CaptureState).NotTo(BeNil())
		Expect(root.Status.CaptureState.DomainSpecificController).NotTo(BeNil())
		Expect(root.Status.CaptureState.DomainSpecificController.ExcludedRefs).To(ConsistOf(want), "the root's own record, frozen at Planned")

		content := &storagev1alpha1.SnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: root.Status.BoundSnapshotContentName}, content)).To(Succeed())
		Expect(content.Status.ExcludedRefs).To(Equal(want), "the durable aggregate on the root content")
		Expect(content.Status.ManifestCheckpointName).NotTo(BeEmpty())

		captured := map[string]struct{}{}
		for _, obj := range integrationArchiveObjectsFromMCP(ctx, content.Status.ManifestCheckpointName) {
			md, _ := obj["metadata"].(map[string]interface{})
			name, _ := md["name"].(string)
			kind, _ := obj["kind"].(string)
			captured[kind+"/"+name] = struct{}{}
		}
		Expect(captured).To(HaveKey("ConfigMap/cm-plain"))
		Expect(captured).NotTo(HaveKey("ConfigMap/cm-vetoed"))
		Expect(captured).NotTo(HaveKey("ConfigMap/cm-owned-vetoed"))
		Expect(captured).NotTo(HaveKey("PersistentVolumeClaim/pvc-vetoed"))
	})
})
