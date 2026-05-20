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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

var _ = Describe("SnapshotContent dataRefs[] CRD validation", func() {
	const duplicateUID = "pvc-uid-dup"

	binding := func(name string) storagev1alpha1.SnapshotDataBinding {
		return storagev1alpha1.SnapshotDataBinding{
			TargetUID: duplicateUID,
			Target: storagev1alpha1.SnapshotSubjectRef{
				APIVersion: "v1",
				Kind:       "PersistentVolumeClaim",
				Name:       name,
				Namespace:  "default",
				UID:        types.UID(duplicateUID),
			},
			Artifact: storagev1alpha1.SnapshotDataArtifactRef{
				APIVersion: "snapshot.storage.k8s.io/v1",
				Kind:       "VolumeSnapshotContent",
				Name:       "vsc-" + name,
			},
		}
	}

	It("rejects status.dataRefs[] with duplicate targetUID on Status().Update", func() {
		name := "dup-datarefs-" + randomSuffix()
		sc := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: storagev1alpha1.SnapshotContentSpec{
				DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
			},
		}
		Expect(k8sClient.Create(ctx, sc)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: name}})
		})

		sc.Status = storagev1alpha1.SnapshotContentStatus{
			DataRefs: []storagev1alpha1.SnapshotDataBinding{
				binding("pvc-a"),
				binding("pvc-b"),
			},
		}
		err := k8sClient.Status().Update(ctx, sc)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got: %v", err)
		Expect(err.Error()).To(Or(
			ContainSubstring("duplicate"),
			ContainSubstring("unique"),
			ContainSubstring("targetUID"),
		))
	})

	It("rejects empty targetUID in dataRefs[]", func() {
		name := "empty-targetuid-" + randomSuffix()
		sc := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: storagev1alpha1.SnapshotContentSpec{
				DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
			},
		}
		Expect(k8sClient.Create(ctx, sc)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: name}})
		})

		b := binding("pvc-only")
		b.TargetUID = ""
		sc.Status = storagev1alpha1.SnapshotContentStatus{DataRefs: []storagev1alpha1.SnapshotDataBinding{b}}
		err := k8sClient.Status().Update(ctx, sc)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got: %v", err)
	})
})

func randomSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
