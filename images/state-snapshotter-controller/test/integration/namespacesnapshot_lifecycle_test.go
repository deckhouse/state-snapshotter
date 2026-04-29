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
	"strings"
	"time"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
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

var _ = Describe("Integration: NamespaceSnapshot lifecycle", func() {
	It("binds NamespaceSnapshotContent and reaches Ready with manifestCheckpointName on content (N2a)", func() {
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-lifecycle-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "namespacesnapshot-lifecycle",
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

			wantContent := fmt.Sprintf("ns-%s", strings.ReplaceAll(string(fresh.UID), "-", ""))
			g.Expect(fresh.Status.BoundSnapshotContentName).To(Equal(wantContent))
			g.Expect(fresh.Status.ObservedGeneration).To(Equal(fresh.Generation))

			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			// Root NamespaceSnapshot status must not surface internal capture CRs (no MCR/VCR fields in API);
			// binding and manifest artifact identity go through NamespaceSnapshotContent.

			sc := &storagev1alpha1.NamespaceSnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: fresh.Status.BoundSnapshotContentName}, sc)).To(Succeed())
			g.Expect(sc.Spec.DeletionPolicy).To(Equal(storagev1alpha1.SnapshotContentDeletionPolicyRetain))
			g.Expect(sc.Spec.NamespaceSnapshotRef.Kind).To(Equal("NamespaceSnapshot"))
			g.Expect(sc.Spec.NamespaceSnapshotRef.Name).To(Equal(fresh.Name))
			g.Expect(sc.Spec.NamespaceSnapshotRef.Namespace).To(Equal(fresh.Namespace))

			mcrName := namespacemanifest.NamespaceSnapshotMCRName(fresh.UID)
			g.Expect(errors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: mcrName}, &ssv1alpha1.ManifestCaptureRequest{}))).To(BeTrue())
			wantMCP := sc.Status.ManifestCheckpointName
			g.Expect(wantMCP).NotTo(BeEmpty())
			mcp := &ssv1alpha1.ManifestCheckpoint{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: wantMCP}, mcp)).To(Succeed())
			g.Expect(mcpOwnerRefToRootNSC(mcp.OwnerReferences, fresh.Status.BoundSnapshotContentName, sc.UID)).To(BeTrue())
			for _, ref := range mcp.OwnerReferences {
				g.Expect(ref.Kind).NotTo(Equal("ObjectKeeper"), "N2a path must not retain MCP via ret-mcr ObjectKeeper")
			}
			g.Expect(mcp.Spec.ManifestCaptureRequestRef).NotTo(BeNil())
			g.Expect(mcp.Spec.ManifestCaptureRequestRef.Namespace).To(Equal(nsName))
			g.Expect(mcp.Spec.ManifestCaptureRequestRef.Name).To(Equal(mcrName))
			g.Expect(mcp.Spec.ManifestCaptureRequestRef.UID).NotTo(BeEmpty())

			retMCRKeeper := fmt.Sprintf("ret-mcr-%s-%s", nsName, mcrName)
			err := k8sClient.Get(ctx, client.ObjectKey{Name: retMCRKeeper}, &deckhousev1alpha1.ObjectKeeper{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())
	})

	It("reaches Ready with empty MCP when there are no allowlisted namespaced resources", func() {
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-notargets-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "namespacesnapshot-no-targets",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

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
			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal(snapshot.ReasonCompleted))

			sc := &storagev1alpha1.NamespaceSnapshotContent{}
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
