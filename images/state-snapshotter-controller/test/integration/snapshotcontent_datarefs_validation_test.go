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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

var _ = Describe("SnapshotContent data CRD validation", func() {
	const sourceUID = "pvc-uid-a"

	binding := func(name string) storagev1alpha1.SnapshotDataBinding {
		return storagev1alpha1.SnapshotDataBinding{
			SourceRef: storagev1alpha1.SnapshotSubjectRef{
				APIVersion: "v1",
				Kind:       "PersistentVolumeClaim",
				Name:       name,
				Namespace:  "default",
				UID:        types.UID(sourceUID),
			},
			ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
				APIVersion: "snapshot.storage.k8s.io/v1",
				Kind:       "VolumeSnapshotContent",
				Name:       "vsc-" + name,
			},
		}
	}

	// Variant A (cardinality ≤1): a SnapshotContent carries at most one data binding (a singular object,
	// not a list), so a duplicate-in-a-list validation is structurally impossible. The hard rename
	// dropped the standalone required targetUID; the volume identity is now data.sourceRef.uid (optional at
	// the CRD level), so there is no longer a CRD-level "empty uid" rejection to assert here.
	It("accepts a single status.data on Status().Update", func() {
		name := "single-data-" + randomSuffix()
		sc := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       retainContentSpec(),
		}
		Expect(k8sClient.Create(ctx, sc)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: name}})
		})

		b := binding("pvc-a")
		sc.Status = storagev1alpha1.SnapshotContentStatus{Data: &b}
		Expect(k8sClient.Status().Update(ctx, sc)).To(Succeed())
	})

	// Dropping a field from the Go type is only half of removing it: while the generated CRD still declares the
	// property, the apiserver keeps accepting and STORING the key, so a stale writer or a hand-written patch
	// goes on publishing it and consumers go on reading it — the field is removed in the code and alive on the
	// wire. accessModes is that field: it was dropped from SnapshotDataBinding before this schema was released
	// (it round-trips through the captured PVC manifest, and the transient export volume defaults its own
	// modes), and only the served schema can prove it is gone rather than orphaned.
	//
	// The write goes through unstructured because the typed client cannot express a key its Go type lacks, and
	// the assertions run on the object the apiserver RETURNS from the status write — the persisted state — so
	// the aggregator, which reconciles every SnapshotContent in this suite, cannot race them by rewriting the
	// status afterwards. It can still invalidate the resourceVersion we hold, hence the re-Get-and-retry loop.
	// The keys that survive are what makes the absence meaningful: they prove the write reached the schema, so
	// accessModes is missing because it was pruned, not because the patch never landed.
	It("prunes accessModes out of status.data: the field is gone from the served schema, not orphaned in it", func() {
		name := "pruned-field-" + randomSuffix()
		typed := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       retainContentSpec(),
		}
		Expect(k8sClient.Create(ctx, typed)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: name}})
		})

		var stored map[string]interface{}
		Eventually(func(g Gomega) {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("SnapshotContent"))
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, live)).To(Succeed())
			g.Expect(unstructured.SetNestedMap(live.Object, map[string]interface{}{
				"sourceRef": map[string]interface{}{
					"apiVersion": "v1", "kind": "PersistentVolumeClaim", "name": "pvc-a",
					"namespace": "default", "uid": sourceUID,
				},
				"artifactRef": map[string]interface{}{
					"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshotContent", "name": "vsc-pvc-a",
				},
				"volumeMode":       "Filesystem",
				"fsType":           "ext4",
				"storageClassName": "sc-a",
				"size":             "10Gi",
				"accessModes":      []interface{}{"ReadWriteOnce"},
			}, "status", "data")).To(Succeed())
			g.Expect(k8sClient.Status().Update(ctx, live)).To(Succeed())

			persisted, found, err := unstructured.NestedMap(live.Object, "status", "data")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.data must be stored")
			stored = persisted
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		for _, key := range []string{"sourceRef", "artifactRef", "volumeMode", "fsType", "storageClassName", "size"} {
			Expect(stored).To(HaveKey(key), "the schema must still serve %s; without the survivors the pruning assertion below proves nothing", key)
		}
		Expect(stored).NotTo(HaveKey("accessModes"),
			"the CRD still declares status.data.accessModes, so the apiserver stored it: the field was removed from the type but left orphaned in the schema")
	})
})

func randomSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
