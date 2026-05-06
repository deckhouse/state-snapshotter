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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// PR5a: one demo kind, merge-safe refs on root Snapshot + root SnapshotContent, plus one DSC row for registration smoke.
var _ = Describe("Integration: PR5a DemoVirtualDiskSnapshot graph wiring", Serial, func() {
	const dscName = "integration-pr5a-demo-disk-dsc"

	BeforeEach(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.DomainSpecificSnapshotController{ObjectMeta: metav1.ObjectMeta{Name: dscName}}))
	})

	AfterEach(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.DomainSpecificSnapshotController{ObjectMeta: metav1.ObjectMeta{Name: dscName}}))
	})

	It("registers DSC and merges demo disk snapshot into root children*Refs", func() {
		testCtx := context.Background()

		dsc := &ssv1alpha1.DomainSpecificSnapshotController{
			ObjectMeta: metav1.ObjectMeta{Name: dscName},
			Spec: ssv1alpha1.DomainSpecificSnapshotControllerSpec{
				OwnerModule: "integration-pr5a",
				SnapshotResourceMapping: []ssv1alpha1.SnapshotResourceMappingEntry{
					{
						ResourceCRDName: "demovirtualdisks.demo.state-snapshotter.deckhouse.io",
						SnapshotCRDName: "demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io",
					},
				},
			},
		}
		Expect(k8sClient.Create(testCtx, dsc)).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(testCtx, &ssv1alpha1.DomainSpecificSnapshotController{ObjectMeta: metav1.ObjectMeta{Name: dscName}}))
		})

		Eventually(func(g Gomega) {
			cur := &ssv1alpha1.DomainSpecificSnapshotController{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: dscName}, cur)).To(Succeed())
			acc := meta.FindStatusCondition(cur.Status.Conditions, controllers.DSCConditionAccepted)
			g.Expect(acc).NotTo(BeNil())
			g.Expect(acc.Status).To(Equal(metav1.ConditionTrue))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		hook := &ssv1alpha1.DomainSpecificSnapshotController{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: dscName}, hook)).To(Succeed())
		gen := hook.GetGeneration()
		meta.SetStatusCondition(&hook.Status.Conditions, metav1.Condition{
			Type:               controllers.DSCConditionRBACReady,
			Status:             metav1.ConditionTrue,
			Reason:             "IntegrationHook",
			Message:            "pr5a demo",
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: gen,
		})
		Expect(k8sClient.Status().Update(testCtx, hook)).To(Succeed())
		integrationWaitGraphRegistryKind("DemoVirtualDiskSnapshot")

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "pr5a-demo-disk-",
				Labels:       map[string]string{"state-snapshotter.deckhouse.io/test": "pr5a-demo-disk"},
			},
		}
		Expect(k8sClient.Create(testCtx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}) })

		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "pr5a-cm", Namespace: nsName}, Data: map[string]string{"k": "v"}}
		Expect(k8sClient.Create(testCtx, cm)).To(Succeed())

		diskResource := &demov1alpha1.DemoVirtualDisk{
			ObjectMeta: metav1.ObjectMeta{Name: "disk-a", Namespace: nsName},
		}
		Expect(k8sClient.Create(testCtx, diskResource)).To(Succeed())

		root := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(testCtx, root)).To(Succeed())

		var rootContentName string
		Eventually(func(g Gomega) {
			r := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: "root"}, r)).To(Succeed())
			rc := meta.FindStatusCondition(r.Status.Conditions, snapshot.ConditionReady)
			g.Expect(rc).NotTo(BeNil())
			g.Expect(rc.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(r.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			rootContentName = r.Status.BoundSnapshotContentName
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		var diskSnapshotName string
		Eventually(func(g Gomega) {
			r := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: "root"}, r)).To(Succeed())
			diskSnapshotName = ""
			for _, ch := range r.Status.ChildrenSnapshotRefs {
				if ch.APIVersion == demov1alpha1.SchemeGroupVersion.String() && ch.Kind == "DemoVirtualDiskSnapshot" {
					diskSnapshotName = ch.Name
					break
				}
			}
			g.Expect(diskSnapshotName).NotTo(BeEmpty(), "root Snapshot should list demo disk snapshot in childrenSnapshotRefs")
		}).WithTimeout(45 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		var contentName string
		Eventually(func(g Gomega) {
			d := &demov1alpha1.DemoVirtualDiskSnapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: diskSnapshotName}, d)).To(Succeed())
			g.Expect(d.Spec.SourceRef).To(Equal(demov1alpha1.SnapshotSourceRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       "DemoVirtualDisk",
				Name:       "disk-a",
			}))
			g.Expect(d.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			contentName = d.Status.BoundSnapshotContentName
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			content := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: rootContentName}, content)).To(Succeed())
			var found bool
			for _, ch := range content.Status.ChildrenSnapshotContentRefs {
				if ch.Name == contentName {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "root SnapshotContent should reference demo disk content")
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		var contentVisited []string
		err := usecase.WalkSnapshotContentSubtree(testCtx, k8sClient, rootContentName,
			func(_ context.Context, content *storagev1alpha1.SnapshotContent) error {
				contentVisited = append(contentVisited, content.Name)
				return nil
			},
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(contentVisited).NotTo(BeEmpty())
		Expect(contentVisited).To(ContainElement(contentName), "ref-only walk should visit SnapshotContent leaf via same childrenSnapshotContentRefs graph")

		Eventually(func(g Gomega) {
			content := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: contentName}, content)).To(Succeed())
			g.Expect(content.Status.ManifestCheckpointName).NotTo(BeEmpty())
			objects := integrationArchiveObjectsFromMCP(testCtx, content.Status.ManifestCheckpointName)
			g.Expect(integrationObjectsContainKindName(objects, "DemoVirtualDisk", "disk-a")).To(BeTrue(), "disk own MCP should contain source DemoVirtualDisk")
			g.Expect(integrationObjectsContainKind(objects, "ConfigMap")).To(BeFalse(), "disk own MCP must contain only the source demo disk object")
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})
})
