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

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
)

// This is the regression guard for the unified request lifecycle (INV-EXECUTION-OK): the
// ManifestCheckpointController must create the execution ObjectKeeper first and the freshly created
// ManifestCheckpoint must be owned by that ObjectKeeper - NOT directly by a SnapshotContent. It uses a
// standalone MCR with no SnapshotContent referencing it, so the SnapshotContent-side handoff never runs
// and the intermediate execution ownership stays observable (snapshot_root_lifecycle_test only checks the
// post-handoff state and would not catch a "MCP created under SnapshotContent at birth" regression).
var _ = Describe("Integration: ManifestCaptureRequest execution ObjectKeeper before MCP (regression)", func() {
	It("creates the execution ObjectKeeper first and owns the freshly created MCP by it (no SnapshotContent handoff)", func() {
		ctx := context.Background()

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "mcr-exec-own-cm", Namespace: "default"},
			Data:       map[string]string{"k": "v"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cm) })

		mcr := &storagev1alpha1.ManifestCaptureRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "mcr-exec-own", Namespace: "default"},
			Spec: storagev1alpha1.ManifestCaptureRequestSpec{
				Targets: []storagev1alpha1.ManifestTarget{{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       cm.Name,
				}},
			},
		}
		Expect(k8sClient.Create(ctx, mcr)).To(Succeed())
		mcrKey := types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &storagev1alpha1.ManifestCaptureRequest{ObjectMeta: metav1.ObjectMeta{Name: mcr.Name, Namespace: mcr.Namespace}})
		})

		var mcpName, okName string
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.ManifestCaptureRequest{}
			g.Expect(k8sClient.Get(ctx, mcrKey, fresh)).To(Succeed())
			g.Expect(fresh.Status.CheckpointName).NotTo(BeEmpty())
			mcpName = fresh.Status.CheckpointName

			mcp := &storagev1alpha1.ManifestCheckpoint{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: mcpName}, mcp)).To(Succeed())
			g.Expect(mcp.Spec.ManifestCaptureRequestRef).NotTo(BeNil())
			mcrUID := types.UID(mcp.Spec.ManifestCaptureRequestRef.UID)
			g.Expect(mcrUID).NotTo(BeEmpty())
			okName = namespacemanifest.ManifestCaptureRequestObjectKeeperName(mcrUID)

			// Execution ObjectKeeper must exist (created before the MCP) and follow this MCR.
			ok := &deckhousev1alpha1.ObjectKeeper{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: okName}, ok)).To(Succeed())
			g.Expect(ok.Spec.FollowObjectRef).NotTo(BeNil())
			g.Expect(ok.Spec.FollowObjectRef.Kind).To(Equal("ManifestCaptureRequest"))
			g.Expect(ok.Spec.FollowObjectRef.UID).To(Equal(string(mcrUID)))

			// The freshly created MCP must be controller-owned by the execution ObjectKeeper, never by a
			// SnapshotContent at creation time.
			var ctrlOwner *metav1.OwnerReference
			for i := range mcp.OwnerReferences {
				if mcp.OwnerReferences[i].Controller != nil && *mcp.OwnerReferences[i].Controller {
					ctrlOwner = &mcp.OwnerReferences[i]
				}
				g.Expect(mcp.OwnerReferences[i].Kind).NotTo(Equal("SnapshotContent"),
					"unified lifecycle: MCP must not be created owned by SnapshotContent; handoff is done later by SnapshotContentController")
			}
			g.Expect(ctrlOwner).NotTo(BeNil())
			g.Expect(ctrlOwner.Kind).To(Equal("ObjectKeeper"))
			g.Expect(ctrlOwner.Name).To(Equal(okName))
		}).WithTimeout(120 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		// No SnapshotContent references this MCP, so no handoff can occur: ownership must stay on the
		// execution ObjectKeeper (it must not drift to SnapshotContent on its own).
		Consistently(func(g Gomega) {
			mcp := &storagev1alpha1.ManifestCheckpoint{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: mcpName}, mcp)).To(Succeed())
			for i := range mcp.OwnerReferences {
				g.Expect(mcp.OwnerReferences[i].Kind).NotTo(Equal("SnapshotContent"))
			}
		}).WithTimeout(3 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &storagev1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: mcpName}})
			if okName != "" {
				_ = k8sClient.Delete(ctx, &deckhousev1alpha1.ObjectKeeper{ObjectMeta: metav1.ObjectMeta{Name: okName}})
			}
		})
	})
})
