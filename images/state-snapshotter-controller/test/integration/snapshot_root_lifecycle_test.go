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
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

var _ = Describe("Integration: Snapshot lifecycle", func() {
	It("binds SnapshotContent and reaches Ready with manifestCheckpointName on content (N2a)", func() {
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-lifecycle-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "snapshot-lifecycle",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-target-cm", Namespace: nsName},
			Data:       map[string]string{"k": "v"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "snap",
				Namespace: nsName,
			},
			Spec: storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		key := types.NamespacedName{Namespace: nsName, Name: snap.Name}
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())

			wantContent := fmt.Sprintf("ns-%s", strings.ReplaceAll(string(fresh.UID), "-", ""))
			g.Expect(fresh.Status.BoundSnapshotContentName).To(Equal(wantContent))

			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			// The root MCR name is a core-internal deterministic handle (no longer a status field); its
			// deletion after handoff is asserted below via SnapshotMCRName(uid).

			sc := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: fresh.Status.BoundSnapshotContentName}, sc)).To(Succeed())
			g.Expect(sc.Spec.DeletionPolicy).To(Equal(storagev1alpha1.SnapshotContentDeletionPolicyRetain))

			mcrName := namespacemanifest.SnapshotMCRName(fresh.UID)
			g.Expect(errors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: mcrName}, &ssv1alpha1.ManifestCaptureRequest{}))).To(BeTrue())
			wantMCP := sc.Status.ManifestCheckpointName
			g.Expect(wantMCP).NotTo(BeEmpty())
			mcp := &ssv1alpha1.ManifestCheckpoint{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: wantMCP}, mcp)).To(Succeed())
			// Unified lifecycle: the MCP is created owned by the execution ObjectKeeper and then handed off
			// to SnapshotContent by SnapshotContentController. After convergence the controller owner is the
			// root SnapshotContent and the MCP no longer has an ObjectKeeper ownerRef (the execution OK object
			// itself may still exist in envtest; only the ownerRef is asserted here).
			g.Expect(mcpOwnerRefToRootContent(mcp.OwnerReferences, fresh.Status.BoundSnapshotContentName, sc.UID)).To(BeTrue())
			for _, ref := range mcp.OwnerReferences {
				g.Expect(ref.Kind).NotTo(Equal("ObjectKeeper"), "after handoff the MCP must no longer have an ObjectKeeper ownerRef")
			}
			g.Expect(mcp.Spec.SourceNamespace).To(Equal(nsName))
			g.Expect(mcp.Labels).To(HaveKeyWithValue("state-snapshotter.deckhouse.io/source-request", mcrName))
			// Durability is guaranteed by the MCP being owned by SnapshotContent (asserted above), not by the
			// transient execution ObjectKeeper. The execution OK itself is garbage-collected by the external
			// Deckhouse ObjectKeeper controller (FollowObject -> MCR) once the MCR is deleted; that controller
			// is not present in envtest, so its eventual removal is not asserted here.
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())
	})

	It("reaches Ready with empty MCP when there are no allowlisted namespaced resources", func() {
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-notargets-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "snapshot-no-targets",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "snap",
				Namespace: nsName,
			},
			Spec: storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		key := types.NamespacedName{Namespace: nsName, Name: snap.Name}
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal(snapshot.ReasonCompleted))

			sc := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: fresh.Status.BoundSnapshotContentName}, sc)).To(Succeed())
			g.Expect(sc.Status.ManifestCheckpointName).NotTo(BeEmpty())
			contentReady := meta.FindStatusCondition(sc.Status.Conditions, snapshot.ConditionReady)
			g.Expect(contentReady).NotTo(BeNil())
			g.Expect(contentReady.Status).To(Equal(metav1.ConditionTrue))
			objects := integrationArchiveObjectsFromMCP(ctx, sc.Status.ManifestCheckpointName)
			g.Expect(objects).To(BeEmpty())
		}).WithTimeout(60 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})
})
