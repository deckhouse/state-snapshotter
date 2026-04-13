//go:build integration
// +build integration

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

package integration

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

var _ = Describe("Integration: NamespaceSnapshot CapturePlanDrift (N2a)", func() {
	It("sets CapturePlanDrift on root and NSC when allowlisted resources change after MCR is fixed (no silent spec.targets update)", func() {
		ctx := context.Background()
		contentName := ""

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-drift-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "namespacesnapshot-capture-plan-drift",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			if contentName != "" {
				_ = k8sClient.Delete(ctx, &storagev1alpha1.NamespaceSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: contentName}})
			}
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm1 := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-drift-cm1", Namespace: nsName},
			Data:       map[string]string{"k": "v1"},
		}
		Expect(k8sClient.Create(ctx, cm1)).To(Succeed())

		snap := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "snap",
				Namespace: nsName,
			},
			Spec: storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		key := types.NamespacedName{Namespace: nsName, Name: snap.Name}

		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			contentName = fresh.Status.BoundSnapshotContentName
			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		}).WithTimeout(90 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		cm2 := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-drift-cm2", Namespace: nsName},
			Data:       map[string]string{"k": "v2"},
		}
		Expect(k8sClient.Create(ctx, cm2)).To(Succeed())

		// NamespaceSnapshot controller does not watch ConfigMaps; bump metadata to enqueue reconcile.
		snapFresh := &storagev1alpha1.NamespaceSnapshot{}
		Expect(k8sClient.Get(ctx, key, snapFresh)).To(Succeed())
		base := snapFresh.DeepCopy()
		if snapFresh.Annotations == nil {
			snapFresh.Annotations = map[string]string{}
		}
		snapFresh.Annotations["state-snapshotter.deckhouse.io/integration-drift-kick"] = fmt.Sprintf("%d", time.Now().UnixNano())
		Expect(k8sClient.Patch(ctx, snapFresh, client.MergeFrom(base))).To(Succeed())

		Eventually(func(g Gomega) {
			root := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, key, root)).To(Succeed())
			ready := meta.FindStatusCondition(root.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal("CapturePlanDrift"))
			g.Expect(ready.Message).To(ContainSubstring("spec.targets differ"))

			sc := &storagev1alpha1.NamespaceSnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, sc)).To(Succeed())
			cReady := meta.FindStatusCondition(sc.Status.Conditions, snapshot.ConditionReady)
			g.Expect(cReady).NotTo(BeNil())
			g.Expect(cReady.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cReady.Reason).To(Equal("CapturePlanDrift"))
			g.Expect(cReady.Message).To(ContainSubstring("spec.targets differ"))
		}).WithTimeout(60 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		// MCR still exists with frozen spec.targets; operator deletes MCR to retry with a fresh plan.
		root := &storagev1alpha1.NamespaceSnapshot{}
		Expect(k8sClient.Get(ctx, key, root)).To(Succeed())
		mcr := &ssv1alpha1.ManifestCaptureRequest{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{
			Namespace: nsName,
			Name:      namespacemanifest.NamespaceSnapshotMCRName(root.UID),
		}, mcr)).To(Succeed())
	})
})
