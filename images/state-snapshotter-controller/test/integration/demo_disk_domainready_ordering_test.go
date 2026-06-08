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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// Regression for DomainReady publication ordering in demo controllers (INV-DOMAIN-GATE): a leaf demo
// disk snapshot must publish DomainReady=True only after its own domain request (the MCR) has been
// planned. The previous code published DomainReady=True immediately after source validation, before the
// SnapshotContent and MCR existed - so there was a multi-second window where DomainReady=True but no
// request and no checkpoint had been planned yet. The invariant below would fail on that old ordering.
var _ = Describe("Integration: demo disk DomainReady publication ordering", func() {
	It("publishes DomainReady=True only after the MCR request is planned", func() {
		ctx := context.Background()

		disk := &demov1alpha1.DemoVirtualDisk{
			ObjectMeta: metav1.ObjectMeta{Name: "disk-domainready-order", Namespace: "default"},
		}
		Expect(k8sClient.Create(ctx, disk)).To(Succeed())
		DeferCleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(ctx, disk)) })

		snap := &demov1alpha1.DemoVirtualDiskSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "disk-domainready-order-snap", Namespace: "default"},
			Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
				SourceRef: demov1alpha1.SnapshotSourceRef{
					APIVersion: demov1alpha1.SchemeGroupVersion.String(),
					Kind:       "DemoVirtualDisk",
					Name:       disk.Name,
				},
			},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		key := types.NamespacedName{Namespace: snap.Namespace, Name: snap.Name}
		DeferCleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(ctx, snap)) })

		// Ordering invariant, checked continuously from creation: whenever DomainReady=True is observed,
		// the disk's own request must already be planned. That is true either while the MCR ref is still
		// present on the snapshot, or after the checkpoint has been handed off and recorded on the bound
		// SnapshotContent (the MCR ref is cleared only after that handoff).
		invariant := func(g Gomega) {
			d := &demov1alpha1.DemoVirtualDiskSnapshot{}
			if err := k8sClient.Get(ctx, key, d); err != nil {
				return
			}
			dr := meta.FindStatusCondition(d.Status.Conditions, snapshot.ConditionDomainReady)
			if dr == nil || dr.Status != metav1.ConditionTrue {
				return
			}
			if d.Status.ManifestCaptureRequestName != "" {
				return
			}
			contentName := d.Status.BoundSnapshotContentName
			g.Expect(contentName).NotTo(BeEmpty(),
				"DomainReady=True but neither an MCR ref nor a bound SnapshotContent is present (request not planned yet)")
			content := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: contentName}, content)).To(Succeed())
			g.Expect(content.Status.ManifestCheckpointName).NotTo(BeEmpty(),
				"DomainReady=True but the request was never planned (no MCR ref, no handed-off checkpoint)")
		}
		Consistently(invariant).WithTimeout(8 * time.Second).WithPolling(150 * time.Millisecond).Should(Succeed())

		// And the flow does reach DomainReady=True for the current generation with the honest planning
		// message, with the request observably planned (MCR ref still present, or its checkpoint already
		// handed off and recorded on the bound SnapshotContent after cleanup cleared the ref).
		Eventually(func(g Gomega) {
			d := &demov1alpha1.DemoVirtualDiskSnapshot{}
			g.Expect(k8sClient.Get(ctx, key, d)).To(Succeed())
			dr := meta.FindStatusCondition(d.Status.Conditions, snapshot.ConditionDomainReady)
			g.Expect(dr).NotTo(BeNil())
			g.Expect(dr.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(dr.ObservedGeneration).To(Equal(d.Generation))
			g.Expect(dr.Message).To(Equal("manifest capture request planned"))
			if d.Status.ManifestCaptureRequestName == "" {
				g.Expect(d.Status.BoundSnapshotContentName).NotTo(BeEmpty())
				content := &storagev1alpha1.SnapshotContent{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: d.Status.BoundSnapshotContentName}, content)).To(Succeed())
				g.Expect(content.Status.ManifestCheckpointName).NotTo(BeEmpty())
			}
		}).WithTimeout(60 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})
})
