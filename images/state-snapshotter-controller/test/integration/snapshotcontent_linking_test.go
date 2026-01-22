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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

var _ = Describe("Integration: SnapshotController - MCR linking", func() {
	var (
		ctx         context.Context
		snapshotGVK schema.GroupVersionKind
		contentGVK  schema.GroupVersionKind
		mcrGVK      schema.GroupVersionKind
		mcpGVK      schema.GroupVersionKind
	)

	BeforeEach(func() {
		ctx = context.Background()
		snapshotGVK = schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "TestSnapshot"}
		contentGVK = schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "TestSnapshotContent"}
		mcrGVK = schema.GroupVersionKind{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "ManifestCaptureRequest"}
		mcpGVK = schema.GroupVersionKind{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "ManifestCheckpoint"}
	})

	It("should link ManifestCheckpointName into SnapshotContent when MCR is Ready", func() {
		// Create Snapshot
		snapshotObj := &unstructured.Unstructured{}
		snapshotObj.SetGroupVersionKind(snapshotGVK)
		snapshotObj.SetName("test-linking-snapshot")
		snapshotObj.SetNamespace("default")
		snapshotObj.Object["spec"] = map[string]interface{}{
			"backupClassName": "test-backup-class",
		}
		Expect(k8sClient.Create(ctx, snapshotObj)).To(Succeed())

		// Create MCP (cluster-scoped)
		mcp := &unstructured.Unstructured{}
		mcp.SetGroupVersionKind(mcpGVK)
		mcp.SetName("mcp-linking-1")
		mcp.Object["spec"] = map[string]interface{}{
			"sourceNamespace": "default",
			"manifestCaptureRequestRef": map[string]interface{}{
				"name":      "mcr-linking-1",
				"namespace": "default",
				"kind":      "ManifestCaptureRequest",
			},
		}
		Expect(k8sClient.Create(ctx, mcp)).To(Succeed())

		// Create MCR
		mcr := &unstructured.Unstructured{}
		mcr.SetGroupVersionKind(mcrGVK)
		mcr.SetName("mcr-linking-1")
		mcr.SetNamespace("default")
		Expect(k8sClient.Create(ctx, mcr)).To(Succeed())

		// Mark MCR Ready and set checkpointName
		mcrStatus := map[string]interface{}{
			"checkpointName": mcp.GetName(),
			"conditions": []interface{}{
				map[string]interface{}{
					"type":               "Ready",
					"status":             string(metav1.ConditionTrue),
					"reason":             "Completed",
					"message":            "Completed",
					"lastTransitionTime": metav1.Now().Format(time.RFC3339),
				},
			},
		}
		mcr.Object["status"] = mcrStatus
		Expect(k8sClient.Status().Update(ctx, mcr)).To(Succeed())

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

		snapshotCtrl, err := controllers.NewSnapshotController(
			k8sClient,
			mgr.GetAPIReader(),
			scheme,
			testCfg,
			[]schema.GroupVersionKind{snapshotGVK},
		)
		Expect(err).NotTo(HaveOccurred())

		req := ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			},
		}

		// Reconcile to create SnapshotContent and link MCP
		Eventually(func() bool {
			_, err := snapshotCtrl.Reconcile(ctx, req)
			if err != nil {
				return false
			}

			freshSnapshot := &unstructured.Unstructured{}
			freshSnapshot.SetGroupVersionKind(snapshotGVK)
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      snapshotObj.GetName(),
				Namespace: snapshotObj.GetNamespace(),
			}, freshSnapshot)
			if err != nil {
				return false
			}

			freshLike, err := snapshot.ExtractSnapshotLike(freshSnapshot)
			if err != nil {
				return false
			}
			contentName := freshLike.GetStatusContentName()
			if contentName == "" {
				return false
			}

			contentObj := &unstructured.Unstructured{}
			contentObj.SetGroupVersionKind(contentGVK)
			err = k8sClient.Get(ctx, types.NamespacedName{Name: contentName}, contentObj)
			if err != nil {
				return false
			}

			contentStatus, _ := contentObj.Object["status"].(map[string]interface{})
			if contentStatus == nil {
				return false
			}
			return contentStatus["manifestCheckpointName"] == mcp.GetName()
		}, "10s", "200ms").Should(BeTrue(), "SnapshotContent should be linked to MCP from MCR")
	})
})
