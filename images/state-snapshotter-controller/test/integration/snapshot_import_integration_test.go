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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
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
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// importGzip returns base64(gzip(jsonMarshal(objs))).
func importGzip(objs []map[string]interface{}) string {
	raw, _ := json.Marshal(objs)
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(raw)
	_ = w.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

var _ = Describe("Integration: SnapshotImportRequest — manifest-only root node reaches Ready", Serial, func() {
	It("single root node (no volumes): Snapshot + SnapshotContent created, MCP installed, Ready=True", func() {
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "nss-import-"},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		// Prepare one manifest chunk: gzip(json array of one ConfigMap stub).
		cmStub := map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "my-cm",
				"namespace": nsName,
			},
			"data": map[string]interface{}{"key": "value"},
		}
		chunkData := importGzip([]map[string]interface{}{cmStub})

		sirName := "import-test-sir"
		rootNodeID := "root"
		rootSnapshotName := "imported-root"

		// Create a SnapshotImportManifestChunk for the root node.
		chunk := &ssv1alpha1.SnapshotImportManifestChunk{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sirName + "-root-0",
				Namespace: nsName,
			},
			Spec: ssv1alpha1.SnapshotImportManifestChunkSpec{
				ImportRequestName: sirName,
				NodeID:            rootNodeID,
				Index:             0,
				Total:             1,
				Data:              chunkData,
				ObjectsCount:      1,
			},
		}
		Expect(k8sClient.Create(ctx, chunk)).To(Succeed())

		// Create the SnapshotImportRequest.
		sir := &ssv1alpha1.SnapshotImportRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sirName,
				Namespace: nsName,
			},
			Spec: ssv1alpha1.SnapshotImportRequestSpec{
				RootSnapshotName: rootSnapshotName,
				Nodes: []ssv1alpha1.ImportNode{
					{
						ID:         rootNodeID,
						APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
						Kind:       "Snapshot",
						Name:       rootSnapshotName,
						ParentID:   "",
						Children:   nil,
						HasData:    false,
					},
				},
				Volumes: nil,
				TTL:     "24h",
			},
		}
		Expect(k8sClient.Create(ctx, sir)).To(Succeed())
		sirKey := types.NamespacedName{Namespace: nsName, Name: sirName}

		// Wait for the root Snapshot to be created by the import controller.
		rootSnapKey := types.NamespacedName{Namespace: nsName, Name: rootSnapshotName}
		var rootSnap storagev1alpha1.Snapshot
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, rootSnapKey, &rootSnap)).To(Succeed())
			g.Expect(rootSnap.GetAnnotations()[ssv1alpha1.AnnotationImported]).To(Equal("true"))
		}, 60*time.Second, 250*time.Millisecond).Should(Succeed(), "root Snapshot with imported annotation should be created")

		// Wait for SnapshotContent to be bound.
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, rootSnapKey, &rootSnap)).To(Succeed())
			g.Expect(rootSnap.Status.BoundSnapshotContentName).NotTo(BeEmpty())
		}, 60*time.Second, 250*time.Millisecond).Should(Succeed(), "root Snapshot should be bound to SnapshotContent")

		// Verify the SnapshotContent exists.
		contentKey := client.ObjectKey{Name: rootSnap.Status.BoundSnapshotContentName}
		var content storagev1alpha1.SnapshotContent
		Expect(k8sClient.Get(ctx, contentKey, &content)).To(Succeed())
		Expect(content.GetAnnotations()[ssv1alpha1.AnnotationImported]).To(Equal("true"))

		// Wait for ManifestCheckpoint name to be published on SnapshotContent.
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, contentKey, &content)).To(Succeed())
			g.Expect(content.Status.ManifestCheckpointName).NotTo(BeEmpty())
		}, 60*time.Second, 250*time.Millisecond).Should(Succeed(), "ManifestCheckpointName should be published")

		// Wait for ManifestCheckpoint to reach Ready=True.
		mcpKey := client.ObjectKey{Name: content.Status.ManifestCheckpointName}
		var mcp ssv1alpha1.ManifestCheckpoint
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, mcpKey, &mcp)).To(Succeed())
			readyCond := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
			g.Expect(readyCond).NotTo(BeNil())
			g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
		}, 60*time.Second, 250*time.Millisecond).Should(Succeed(), "ManifestCheckpoint should be Ready")

		// Wait for root Snapshot to reach Ready=True (mirrored from SnapshotContent).
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, rootSnapKey, &rootSnap)).To(Succeed())
			readyCond := meta.FindStatusCondition(rootSnap.Status.Conditions, snapshotpkg.ConditionReady)
			g.Expect(readyCond).NotTo(BeNil())
			g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
		}, 120*time.Second, 500*time.Millisecond).Should(Succeed(), "root Snapshot should be Ready=True")

		// Verify SnapshotImportRequest status becomes Ready.
		Eventually(func(g Gomega) {
			var freshSIR ssv1alpha1.SnapshotImportRequest
			g.Expect(k8sClient.Get(ctx, sirKey, &freshSIR)).To(Succeed())
			g.Expect(freshSIR.Status.Phase).To(Equal(ssv1alpha1.SnapshotImportPhaseReady))
			g.Expect(freshSIR.Status.CreatedSnapshotName).To(Equal(rootSnapshotName))
		}, 120*time.Second, 500*time.Millisecond).Should(Succeed(), "SnapshotImportRequest should reach Ready phase")

		// Verify manifest chunk was deleted after processing.
		Eventually(func(g Gomega) {
			chunkList := &ssv1alpha1.SnapshotImportManifestChunkList{}
			g.Expect(k8sClient.List(ctx, chunkList, client.InNamespace(nsName))).To(Succeed())
			for _, item := range chunkList.Items {
				g.Expect(item.Spec.ImportRequestName).NotTo(Equal(sirName),
					"chunk for import request %s should be deleted", sirName)
			}
		}, 60*time.Second, 500*time.Millisecond).Should(Succeed(), "manifest chunks should be deleted after import")
	})
})
