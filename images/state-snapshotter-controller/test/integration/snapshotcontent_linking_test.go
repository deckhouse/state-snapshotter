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
	"time"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

var _ = Describe("Integration: GenericSnapshotBinderController - MCR linking", func() {
	var (
		ctx         context.Context
		snapshotGVK schema.GroupVersionKind
		contentGVK  schema.GroupVersionKind
	)

	BeforeEach(func() {
		ctx = context.Background()
		snapshotGVK = schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "TestSnapshot"}
		contentGVK = unifiedbootstrap.CommonSnapshotContentGVK()
	})

	It("SnapshotContentController should hand off ManifestCheckpoint before MCR becomes Ready", func() {
		// Create Snapshot
		snapshotObj := &unstructured.Unstructured{}
		snapshotObj.SetGroupVersionKind(snapshotGVK)
		snapshotObj.SetName("test-linking-snapshot")
		snapshotObj.SetNamespace("default")
		snapshotObj.Object["spec"] = map[string]interface{}{
			"backupClassName": "test-backup-class",
		}
		Expect(k8sClient.Create(ctx, snapshotObj)).To(Succeed())

		linkCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "mcr-linking-cm", Namespace: "default"},
			Data:       map[string]string{"k": "v"},
		}
		Expect(k8sClient.Create(ctx, linkCM)).To(Succeed())

		mcr := &storagev1alpha1.ManifestCaptureRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mcr-linking-1",
				Namespace: "default",
			},
			Spec: storagev1alpha1.ManifestCaptureRequestSpec{
				Targets: []storagev1alpha1.ManifestTarget{{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       linkCM.Name,
				}},
			},
		}
		Expect(k8sClient.Create(ctx, mcr)).To(Succeed())

		mcrKey := types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}
		var mcpName string
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.ManifestCaptureRequest{}
			g.Expect(k8sClient.Get(ctx, mcrKey, fresh)).To(Succeed())
			g.Expect(fresh.Status.CheckpointName).NotTo(BeEmpty())
			mcpName = fresh.Status.CheckpointName
			mcp := &storagev1alpha1.ManifestCheckpoint{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: mcpName}, mcp)).To(Succeed())
			ready := meta.FindStatusCondition(mcp.Status.Conditions, storagev1alpha1.ManifestCheckpointConditionTypeReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal(storagev1alpha1.ManifestCheckpointConditionReasonCompleted))
		}).WithTimeout(120 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
		Expect(mcpName).NotTo(BeEmpty())

		// Simulate domain controller status on Snapshot
		snapshotLike, err := snapshot.ExtractSnapshotLike(snapshotObj)
		Expect(err).NotTo(HaveOccurred())
		snapshot.SetCondition(
			snapshotLike,
			snapshot.ConditionHandledByDomainSpecificController,
			metav1.ConditionTrue,
			"Processed",
			"Domain controller processed snapshot",
		)
		snapshot.SyncConditionsToUnstructured(snapshotObj, snapshotLike.GetStatusConditions())
		status, _ := snapshotObj.Object["status"].(map[string]interface{})
		if status == nil {
			status = map[string]interface{}{}
		}
		status["manifestCaptureRequestName"] = mcr.GetName()
		snapshotObj.Object["status"] = status
		Expect(k8sClient.Status().Update(ctx, snapshotObj)).To(Succeed())

		snapshotCtrl, err := controllers.NewGenericSnapshotBinderController(
			k8sClient,
			mgr.GetAPIReader(),
			scheme,
			testCfg,
			nil,
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(snapshotCtrl.GVKRegistry.RegisterSnapshotContentMapping(
			snapshotGVK.Kind,
			snapshotGVK.GroupVersion().String(),
			contentGVK.Kind,
			contentGVK.GroupVersion().String(),
		)).To(Succeed())
		snapshotCtrl.SnapshotGVKs = []schema.GroupVersionKind{snapshotGVK}

		contentCtrl, err := controllers.NewSnapshotContentController(
			k8sClient,
			mgr.GetAPIReader(),
			scheme,
			mgr.GetRESTMapper(),
			testCfg,
			[]schema.GroupVersionKind{contentGVK},
		)
		Expect(err).NotTo(HaveOccurred())

		req := ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			},
		}

		var contentName string
		Eventually(func(g Gomega) {
			_, err := snapshotCtrl.Reconcile(ctx, req)
			g.Expect(err).NotTo(HaveOccurred())

			freshSnapshot := &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			}, freshSnapshot)).To(Succeed())

			freshLike, err := snapshot.ExtractSnapshotLike(freshSnapshot)
			g.Expect(err).NotTo(HaveOccurred())
			contentName = freshLike.GetStatusContentName()
			g.Expect(contentName).NotTo(BeEmpty())
		}, "30s", "200ms").Should(Succeed(), "Snapshot should bind common SnapshotContent")

		// The snapshot controller owns publisher fields and records the final MCP ref on content.
		Eventually(func(g Gomega) {
			_, err := snapshotCtrl.Reconcile(ctx, req)
			g.Expect(err).NotTo(HaveOccurred())
			contentObj := &unstructured.Unstructured{}
			contentObj.SetGroupVersionKind(contentGVK)
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: contentName}, contentObj)).To(Succeed())
			gotMCPName, _, err := unstructured.NestedString(contentObj.Object, "status", "manifestCheckpointName")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(gotMCPName).To(Equal(mcpName))
		}, "30s", "200ms").Should(Succeed(), "Snapshot controller should publish MCP ref to SnapshotContent")

		Eventually(func(g Gomega) {
			_, err := contentCtrl.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: contentName}})
			g.Expect(err).NotTo(HaveOccurred())
			contentObj := &unstructured.Unstructured{}
			contentObj.SetGroupVersionKind(contentGVK)
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: contentName}, contentObj)).To(Succeed())
			freshSnapshot := &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: snapshotObj.GetName(), Namespace: snapshotObj.GetNamespace()}, freshSnapshot)).To(Succeed())
			mcrName, _, err := unstructured.NestedString(freshSnapshot.Object, "status", "manifestCaptureRequestName")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(mcrName).To(Equal(mcr.Name))

			contentStatus, _ := contentObj.Object["status"].(map[string]interface{})
			g.Expect(contentStatus).NotTo(BeNil())
			g.Expect(contentStatus["manifestCheckpointName"]).To(Equal(mcpName))
			contentLike, err := snapshot.ExtractSnapshotContentLike(contentObj)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(snapshot.IsReady(contentLike)).To(BeTrue())
		}, "30s", "200ms").Should(Succeed(), "SnapshotContentController should link MCP and set common content Ready")

		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.ManifestCaptureRequest{}
			g.Expect(k8sClient.Get(ctx, mcrKey, fresh)).To(Succeed())
			ready := meta.FindStatusCondition(fresh.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted))
		}, "30s", "200ms").Should(Succeed(), "MCR should complete only after MCP ownerRef is handed off to SnapshotContent")

		_, err = snapshotCtrl.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		freshSnapshot := &unstructured.Unstructured{}
		freshSnapshot.SetGroupVersionKind(snapshotGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: snapshotObj.GetName(), Namespace: snapshotObj.GetNamespace()}, freshSnapshot)).To(Succeed())
		freshLike, err := snapshot.ExtractSnapshotLike(freshSnapshot)
		Expect(err).NotTo(HaveOccurred())
		Expect(snapshot.IsReady(freshLike)).To(BeTrue())
	})
})
