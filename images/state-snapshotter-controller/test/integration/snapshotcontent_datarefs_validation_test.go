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
	"k8s.io/apimachinery/pkg/types"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

var _ = Describe("SnapshotContent data CRD validation", func() {
	const sourceUID = "pvc-uid-a"

	binding := func(name string) storagev1alpha1.SnapshotDataBinding {
		return storagev1alpha1.SnapshotDataBinding{
			Source: storagev1alpha1.SnapshotSubjectRef{
				APIVersion: "v1",
				Kind:       "PersistentVolumeClaim",
				Name:       name,
				Namespace:  "default",
				UID:        types.UID(sourceUID),
			},
			Artifact: storagev1alpha1.SnapshotDataArtifactRef{
				APIVersion: "snapshot.storage.k8s.io/v1",
				Kind:       "VolumeSnapshotContent",
				Name:       "vsc-" + name,
			},
		}
	}

	// Variant A (cardinality ≤1): a SnapshotContent carries at most one data binding (a singular object,
	// not a list), so a duplicate-in-a-list validation is structurally impossible. The wave5 hard rename
	// dropped the standalone required targetUID; the volume identity is now data.source.uid (optional at
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
})

func randomSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
