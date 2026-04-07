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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

var _ = Describe("Integration: SnapshotContentController - Artifact finalizers", func() {
	var (
		ctx        context.Context
		contentGVK schema.GroupVersionKind
		mcpGVK     schema.GroupVersionKind
	)

	BeforeEach(func() {
		ctx = context.Background()
		contentGVK = schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "TestSnapshotContent"}
		mcpGVK = schema.GroupVersionKind{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "ManifestCheckpoint"}
	})

	It("should add and remove artifact finalizer on MCP", func() {
		// Create MCR (required for MCP ref UID)
		mcr := &unstructured.Unstructured{}
		mcr.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "state-snapshotter.deckhouse.io",
			Version: "v1alpha1",
			Kind:    "ManifestCaptureRequest",
		})
		mcr.SetName("mcr-artifact-finalizer")
		mcr.SetNamespace("default")
		mcr.Object["spec"] = map[string]interface{}{
			"targets": []interface{}{},
		}
		Expect(k8sClient.Create(ctx, mcr)).To(Succeed())

		// Create MCP
		mcp := &unstructured.Unstructured{}
		mcp.SetGroupVersionKind(mcpGVK)
		mcp.SetName("mcp-artifact-finalizer")
		mcp.Object["spec"] = map[string]interface{}{
			"sourceNamespace": "default",
			"manifestCaptureRequestRef": map[string]interface{}{
				"name":      mcr.GetName(),
				"namespace": mcr.GetNamespace(),
				"uid":       string(mcr.GetUID()),
			},
		}
		Expect(k8sClient.Create(ctx, mcp)).To(Succeed())

		// Create SnapshotContent with manifestCheckpointName
		content := &unstructured.Unstructured{}
		content.SetGroupVersionKind(contentGVK)
		content.SetName("content-artifact-finalizer")
		Expect(k8sClient.Create(ctx, content)).To(Succeed())
		freshContent := &unstructured.Unstructured{}
		freshContent.SetGroupVersionKind(contentGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: content.GetName()}, freshContent)).To(Succeed())
		freshContent.Object["status"] = map[string]interface{}{
			"manifestCheckpointName": mcp.GetName(),
		}
		Expect(k8sClient.Status().Update(ctx, freshContent)).To(Succeed())

		contentCtrl, err := controllers.NewSnapshotContentController(
			k8sClient,
			mgr.GetAPIReader(),
			scheme,
			mgr.GetRESTMapper(),
			testCfg,
			[]schema.GroupVersionKind{contentGVK},
		)
		Expect(err).NotTo(HaveOccurred())

		// Reconcile to add artifact finalizer (may require requeue)
		Eventually(func() bool {
			_, _ = contentCtrl.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: content.GetName()},
			})

			freshMCP := &unstructured.Unstructured{}
			freshMCP.SetGroupVersionKind(mcpGVK)
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: mcp.GetName()}, freshMCP); err != nil {
				return false
			}
			return contains(freshMCP.GetFinalizers(), snapshot.FinalizerArtifactProtect)
		}, "10s", "100ms").Should(BeTrue())

		// Delete SnapshotContent (it has parent-protect finalizer added by controller)
		err = k8sClient.Delete(ctx, content)
		Expect(err).NotTo(HaveOccurred())

		// Reconcile delete path to remove artifact finalizer
		Eventually(func() bool {
			_, _ = contentCtrl.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: content.GetName()},
			})

			freshMCP2 := &unstructured.Unstructured{}
			freshMCP2.SetGroupVersionKind(mcpGVK)
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: mcp.GetName()}, freshMCP2); err != nil {
				return false
			}
			return !contains(freshMCP2.GetFinalizers(), snapshot.FinalizerArtifactProtect)
		}, "10s", "100ms").Should(BeTrue())
	})
})
