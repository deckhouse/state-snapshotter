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

var _ = Describe("Integration: SnapshotContentController - Ready Contract", func() {
	var (
		ctx        context.Context
		contentGVK schema.GroupVersionKind
		mcpGVK     schema.GroupVersionKind
	)

	BeforeEach(func() {
		ctx = context.Background()
		contentGVK = schema.GroupVersionKind{
			Group:   "test.deckhouse.io",
			Version: "v1alpha1",
			Kind:    "TestSnapshotContent",
		}
		mcpGVK = schema.GroupVersionKind{
			Group:   "state-snapshotter.deckhouse.io",
			Version: "v1alpha1",
			Kind:    "ManifestCheckpoint",
		}
	})

	It("should NOT set Ready=True when manifestCheckpointName is empty", func() {
		contentObj := &unstructured.Unstructured{}
		contentObj.SetGroupVersionKind(contentGVK)
		contentObj.SetName("ready-contract-no-mcp")
		err := k8sClient.Create(ctx, contentObj)
		Expect(err).NotTo(HaveOccurred())

		contentCtrl, err := controllers.NewSnapshotContentController(
			k8sClient,
			mgr.GetAPIReader(),
			scheme,
			mgr.GetRESTMapper(),
			testCfg,
			[]schema.GroupVersionKind{contentGVK},
		)
		Expect(err).NotTo(HaveOccurred())

		_, err = contentCtrl.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: contentObj.GetName()},
		})
		Expect(err).NotTo(HaveOccurred())

		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(contentGVK)
		err = k8sClient.Get(ctx, types.NamespacedName{Name: contentObj.GetName()}, fresh)
		Expect(err).NotTo(HaveOccurred())

		contentLike, err := snapshot.ExtractSnapshotContentLike(fresh)
		Expect(err).NotTo(HaveOccurred())
		Expect(snapshot.IsReady(contentLike)).To(BeFalse(), "Ready must stay False without manifestCheckpointName")
	})

	It("should require children to be Ready before setting Ready=True", func() {
		// Create MCR for parent (required for MCP ref UID)
		parentMCR := &unstructured.Unstructured{}
		parentMCR.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "state-snapshotter.deckhouse.io",
			Version: "v1alpha1",
			Kind:    "ManifestCaptureRequest",
		})
		parentMCR.SetName("mcr-parent")
		parentMCR.SetNamespace("default")
		parentMCR.Object["spec"] = map[string]interface{}{
			"targets": []interface{}{},
		}
		Expect(k8sClient.Create(ctx, parentMCR)).To(Succeed())

		// Create MCP for parent
		parentMCP := &unstructured.Unstructured{}
		parentMCP.SetGroupVersionKind(mcpGVK)
		parentMCP.SetName("mcp-parent-ready-contract")
		parentMCP.Object["spec"] = map[string]interface{}{
			"sourceNamespace": "default",
			"manifestCaptureRequestRef": map[string]interface{}{
				"name":      parentMCR.GetName(),
				"namespace": parentMCR.GetNamespace(),
				"uid":       string(parentMCR.GetUID()),
			},
		}
		Expect(k8sClient.Create(ctx, parentMCP)).To(Succeed())

		// Create child SnapshotContent (not Ready yet)
		child := &unstructured.Unstructured{}
		child.SetGroupVersionKind(contentGVK)
		child.SetName("child-ready-contract")
		Expect(k8sClient.Create(ctx, child)).To(Succeed())
		freshChildForStatus := &unstructured.Unstructured{}
		freshChildForStatus.SetGroupVersionKind(contentGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: child.GetName()}, freshChildForStatus)).To(Succeed())
		freshChildForStatus.Object["status"] = map[string]interface{}{
			"manifestCheckpointName": parentMCP.GetName(),
		}
		Expect(k8sClient.Status().Update(ctx, freshChildForStatus)).To(Succeed())

		// Create parent SnapshotContent with child ref
		parent := &unstructured.Unstructured{}
		parent.SetGroupVersionKind(contentGVK)
		parent.SetName("parent-ready-contract")
		Expect(k8sClient.Create(ctx, parent)).To(Succeed())
		freshParentForStatus := &unstructured.Unstructured{}
		freshParentForStatus.SetGroupVersionKind(contentGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: parent.GetName()}, freshParentForStatus)).To(Succeed())
		freshParentForStatus.Object["status"] = map[string]interface{}{
			"manifestCheckpointName": parentMCP.GetName(),
			"childrenSnapshotContentRefs": []interface{}{
				map[string]interface{}{
					"kind": contentGVK.Kind,
					"name": child.GetName(),
				},
			},
		}
		Expect(k8sClient.Status().Update(ctx, freshParentForStatus)).To(Succeed())

		contentCtrl, err := controllers.NewSnapshotContentController(
			k8sClient,
			mgr.GetAPIReader(),
			scheme,
			mgr.GetRESTMapper(),
			testCfg,
			[]schema.GroupVersionKind{contentGVK},
		)
		Expect(err).NotTo(HaveOccurred())

		// Parent should not become Ready while child is not Ready
		_, err = contentCtrl.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: parent.GetName()},
		})
		Expect(err).NotTo(HaveOccurred())

		freshParent := &unstructured.Unstructured{}
		freshParent.SetGroupVersionKind(contentGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: parent.GetName()}, freshParent)).To(Succeed())
		parentLike, err := snapshot.ExtractSnapshotContentLike(freshParent)
		Expect(err).NotTo(HaveOccurred())
		Expect(snapshot.IsReady(parentLike)).To(BeFalse(), "Parent must stay not Ready while child is not Ready")

		// Mark child Ready=True
		freshChild := &unstructured.Unstructured{}
		freshChild.SetGroupVersionKind(contentGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: child.GetName()}, freshChild)).To(Succeed())
		childLike, err := snapshot.ExtractSnapshotContentLike(freshChild)
		Expect(err).NotTo(HaveOccurred())
		snapshot.SetCondition(childLike, snapshot.ConditionReady, metav1.ConditionTrue, snapshot.ReasonReady, "Child ready")
		snapshot.SyncConditionsToUnstructured(freshChild, childLike.GetStatusConditions())
		Expect(k8sClient.Status().Update(ctx, freshChild)).To(Succeed())

		// Reconcile parent and expect Ready=True (may require requeue)
		Eventually(func() bool {
			_, _ = contentCtrl.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: parent.GetName()},
			})

			freshParent2 := &unstructured.Unstructured{}
			freshParent2.SetGroupVersionKind(contentGVK)
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: parent.GetName()}, freshParent2); err != nil {
				return false
			}
			parentLike2, err := snapshot.ExtractSnapshotContentLike(freshParent2)
			if err != nil {
				return false
			}
			return snapshot.IsReady(parentLike2)
		}, "10s", "100ms").Should(BeTrue(), "Parent must become Ready after child is Ready")
	})
})
