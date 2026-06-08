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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// Phase 2a damaged-artifact wake-up (MCP path): a SnapshotContent that reached Ready=True via a Ready
// ManifestCheckpoint must, when that MCP later flips to Ready=False/Failed, be woken by the MCP watch
// (ownerRef MCP -> SnapshotContent) and recompute RequestsReady=False / Ready=False with
// reason ManifestCheckpointFailed. No content spec/status edit triggers the flip — only the MCP event.
var _ = Describe("Integration: MCP degradation wakes owning SnapshotContent", Serial, func() {
	It("flips content Ready=False/ManifestCheckpointFailed when the bound MCP fails", func() {
		ctx := context.Background()

		content := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "mcp-wake-content-"},
			Spec: storagev1alpha1.SnapshotContentSpec{
				DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
			},
		}
		Expect(k8sClient.Create(ctx, content)).To(Succeed())
		contentName := content.Name
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: contentName}}))
		})

		// Wait for the controller to observe the content (finalizer added) and capture its UID.
		var contentUID string
		Eventually(func(g Gomega) {
			c := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, c)).To(Succeed())
			contentUID = string(c.UID)
			g.Expect(contentUID).NotTo(BeEmpty())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		mcpName := "mcp-wake-" + contentName

		// Publish the truth ref (status.manifestCheckpointName) on the content.
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			c := &storagev1alpha1.SnapshotContent{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, c); err != nil {
				return err
			}
			c.Status.ManifestCheckpointName = mcpName
			return k8sClient.Status().Update(ctx, c)
		})).To(Succeed())

		// Create the MCP owned by the content (ownerRef MCP -> SnapshotContent) and mark it Ready=True.
		ensureManifestCheckpointStatus(ctx, mcpName, contentName, contentUID, func(m *ssv1alpha1.ManifestCheckpoint) {
			meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
				Type:    ssv1alpha1.ManifestCheckpointConditionTypeReady,
				Status:  metav1.ConditionTrue,
				Reason:  ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
				Message: "checkpoint ready",
			})
		})
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: mcpName}}))
		})

		// Content reaches Ready=True (requests ready, no children).
		Eventually(func(g Gomega) {
			c := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, c)).To(Succeed())
			ready := meta.FindStatusCondition(c.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal(snapshot.ReasonCompleted))
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed())

		// Degrade the MCP: Ready=False with terminal Failed reason. Only the MCP event should drive the flip.
		ensureManifestCheckpointStatus(ctx, mcpName, contentName, contentUID, func(m *ssv1alpha1.ManifestCheckpoint) {
			meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
				Type:    ssv1alpha1.ManifestCheckpointConditionTypeReady,
				Status:  metav1.ConditionFalse,
				Reason:  ssv1alpha1.ManifestCheckpointConditionReasonFailed,
				Message: "checkpoint corrupted",
			})
		})

		// MCP watch wakes the owning content; it recomputes RequestsReady=False -> Ready=False/ManifestCheckpointFailed.
		Eventually(func(g Gomega) {
			c := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, c)).To(Succeed())
			ready := meta.FindStatusCondition(c.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal(snapshot.ReasonManifestCheckpointFailed))
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed())

		// P2a-I1R recovery: flip the MCP back to Ready=True; the MCP watch wakes the content again and it
		// returns to Ready=True/Completed (degradation and recovery share the same revalidation pipeline).
		ensureManifestCheckpointStatus(ctx, mcpName, contentName, contentUID, func(m *ssv1alpha1.ManifestCheckpoint) {
			meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
				Type:    ssv1alpha1.ManifestCheckpointConditionTypeReady,
				Status:  metav1.ConditionTrue,
				Reason:  ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
				Message: "checkpoint recovered",
			})
		})
		Eventually(func(g Gomega) {
			c := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, c)).To(Succeed())
			ready := meta.FindStatusCondition(c.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal(snapshot.ReasonCompleted))
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed())
	})

	// Phase 2a chunk integrity by exact GET (no chunk watch): a Ready content whose MCP references a chunk
	// that is later deleted must, on the next reconcile (here triggered by an MCP status bump via the MCP
	// watch), recompute Ready=False/ManifestCheckpointFailed naming the missing chunk. Chunk deletion alone
	// does not self-wake — correctness is produced on reconcile.
	It("flips content Ready=False/ManifestCheckpointFailed when a referenced chunk is deleted", func() {
		ctx := context.Background()

		content := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "chunk-wake-content-"},
			Spec:       storagev1alpha1.SnapshotContentSpec{DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain},
		}
		Expect(k8sClient.Create(ctx, content)).To(Succeed())
		contentName := content.Name
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: contentName}}))
		})

		var contentUID string
		Eventually(func(g Gomega) {
			c := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, c)).To(Succeed())
			contentUID = string(c.UID)
			g.Expect(contentUID).NotTo(BeEmpty())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		mcpName := "chunk-wake-" + contentName
		chunkName := mcpName + "-0"

		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			c := &storagev1alpha1.SnapshotContent{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, c); err != nil {
				return err
			}
			c.Status.ManifestCheckpointName = mcpName
			return k8sClient.Status().Update(ctx, c)
		})).To(Succeed())

		chunk := &ssv1alpha1.ManifestCheckpointContentChunk{
			ObjectMeta: metav1.ObjectMeta{Name: chunkName},
			Spec:       ssv1alpha1.ManifestCheckpointContentChunkSpec{CheckpointName: mcpName, Index: 0, Data: "x", ObjectsCount: 0, Checksum: "x"},
		}
		Expect(k8sClient.Create(ctx, chunk)).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.ManifestCheckpointContentChunk{ObjectMeta: metav1.ObjectMeta{Name: chunkName}}))
		})

		ensureManifestCheckpointStatus(ctx, mcpName, contentName, contentUID, func(m *ssv1alpha1.ManifestCheckpoint) {
			m.Status.Chunks = []ssv1alpha1.ChunkInfo{{Name: chunkName, Index: 0}}
			meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
				Type: ssv1alpha1.ManifestCheckpointConditionTypeReady, Status: metav1.ConditionTrue,
				Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted, Message: "checkpoint ready",
			})
		})
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: mcpName}}))
		})

		// Content reaches Ready=True (MCP Ready + chunk present).
		Eventually(func(g Gomega) {
			c := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, c)).To(Succeed())
			ready := meta.FindStatusCondition(c.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed())

		// Delete the chunk (does not self-wake), then bump the MCP to trigger a reconcile via the MCP watch.
		Expect(k8sClient.Delete(ctx, &ssv1alpha1.ManifestCheckpointContentChunk{ObjectMeta: metav1.ObjectMeta{Name: chunkName}})).To(Succeed())
		ensureManifestCheckpointStatus(ctx, mcpName, contentName, contentUID, func(m *ssv1alpha1.ManifestCheckpoint) {
			meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
				Type: ssv1alpha1.ManifestCheckpointConditionTypeReady, Status: metav1.ConditionTrue,
				Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted, Message: "checkpoint ready (bump to wake content)",
			})
		})

		Eventually(func(g Gomega) {
			c := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, c)).To(Succeed())
			ready := meta.FindStatusCondition(c.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal(snapshot.ReasonManifestCheckpointFailed))
			g.Expect(ready.Message).To(ContainSubstring(chunkName))
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})

// ensureManifestCheckpointStatus makes the MCP status setup robust under cross-spec envtest churn.
// Cluster-scoped objects can transiently 404 on a read immediately after a successful Create when
// other Serial specs are tearing down cluster-scoped resources; the production controller never
// deletes MCPs, so this only hardens test setup (not the behavior under test). It ensures the MCP
// exists (re-creating it with the SnapshotContent ownerRef if absent), applies the status mutation,
// and retries on conflict/NotFound until the Status().Update succeeds.
func ensureManifestCheckpointStatus(
	ctx context.Context,
	mcpName, contentName, contentUID string,
	mutate func(*ssv1alpha1.ManifestCheckpoint),
) {
	ctrlTrue := true
	EventuallyWithOffset(1, func() error {
		m := &ssv1alpha1.ManifestCheckpoint{}
		err := k8sClient.Get(ctx, client.ObjectKey{Name: mcpName}, m)
		switch {
		case apierrors.IsNotFound(err):
			m = &ssv1alpha1.ManifestCheckpoint{
				ObjectMeta: metav1.ObjectMeta{
					Name: mcpName,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
						Kind:       "SnapshotContent",
						Name:       contentName,
						UID:        types.UID(contentUID),
						Controller: &ctrlTrue,
					}},
				},
				Spec: ssv1alpha1.ManifestCheckpointSpec{
					SourceNamespace:           "default",
					ManifestCaptureRequestRef: &ssv1alpha1.ObjectReference{Name: "mcr-" + contentName, Namespace: "default", UID: "mcr-uid"},
				},
			}
			if cErr := k8sClient.Create(ctx, m); cErr != nil {
				return cErr
			}
			if gErr := k8sClient.Get(ctx, client.ObjectKey{Name: mcpName}, m); gErr != nil {
				return gErr
			}
		case err != nil:
			return err
		}
		mutate(m)
		return k8sClient.Status().Update(ctx, m)
	}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
}
