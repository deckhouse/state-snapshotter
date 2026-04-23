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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// PR5b: DemoVirtualMachineSnapshot under root NamespaceSnapshot; DemoVirtualDiskSnapshot as child under VM; ref-only walk sees VM content then disk content.
var _ = Describe("Integration: PR5b DemoVirtualMachineSnapshot + disk under VM", Serial, func() {
	const dscName = "integration-pr5b-demo-vm-dsc"

	BeforeEach(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.DomainSpecificSnapshotController{ObjectMeta: metav1.ObjectMeta{Name: dscName}}))
	})

	AfterEach(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.DomainSpecificSnapshotController{ObjectMeta: metav1.ObjectMeta{Name: dscName}}))
	})

	It("registers DSC, wires VM snapshot to root, disk under VM, and traverses demo VM content subtree", func() {
		testCtx := context.Background()

		dsc := &ssv1alpha1.DomainSpecificSnapshotController{
			ObjectMeta: metav1.ObjectMeta{Name: dscName},
			Spec: ssv1alpha1.DomainSpecificSnapshotControllerSpec{
				OwnerModule: "integration-pr5b",
				SnapshotResourceMapping: []ssv1alpha1.SnapshotResourceMappingEntry{
					{
						ResourceCRDName: "demovirtualdisks.demo.state-snapshotter.deckhouse.io",
						SnapshotCRDName: "demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io",
						ContentCRDName:  "demovirtualdisksnapshotcontents.demo.state-snapshotter.deckhouse.io",
					},
					{
						ResourceCRDName: "demovirtualmachines.demo.state-snapshotter.deckhouse.io",
						SnapshotCRDName: "demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io",
						ContentCRDName:  "demovirtualmachinesnapshotcontents.demo.state-snapshotter.deckhouse.io",
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
			Message:            "pr5b demo vm",
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: gen,
		})
		Expect(k8sClient.Status().Update(testCtx, hook)).To(Succeed())

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "pr5b-demo-vm-",
				Labels:       map[string]string{"state-snapshotter.deckhouse.io/test": "pr5b-demo-vm"},
			},
		}
		Expect(k8sClient.Create(testCtx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}) })

		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "pr5b-cm", Namespace: nsName}, Data: map[string]string{"k": "v"}}
		Expect(k8sClient.Create(testCtx, cm)).To(Succeed())

		root := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: nsName},
			Spec:       storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(testCtx, root)).To(Succeed())

		rootRef := storagev1alpha1.SnapshotSubjectRef{
			APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
			Kind:       "NamespaceSnapshot",
			Namespace:  nsName,
			Name:       "root",
		}

		var rootNSC string
		Eventually(func(g Gomega) {
			r := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: "root"}, r)).To(Succeed())
			rc := meta.FindStatusCondition(r.Status.Conditions, snapshot.ConditionReady)
			g.Expect(rc).NotTo(BeNil())
			g.Expect(rc.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(r.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			rootNSC = r.Status.BoundSnapshotContentName
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		vmSnap := &demov1alpha1.DemoVirtualMachineSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "vm-run", Namespace: nsName},
			Spec: demov1alpha1.DemoVirtualMachineSnapshotSpec{
				RootNamespaceSnapshotRef: rootRef,
				VirtualMachineName:       "demo-vm-1",
			},
		}
		Expect(k8sClient.Create(testCtx, vmSnap)).To(Succeed())

		wantVMChild := storagev1alpha1.NamespaceSnapshotChildRef{Namespace: nsName, Name: "vm-run"}
		Eventually(func(g Gomega) {
			r := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: "root"}, r)).To(Succeed())
			var found bool
			for _, ch := range r.Status.ChildrenSnapshotRefs {
				if ch.Namespace == wantVMChild.Namespace && ch.Name == wantVMChild.Name {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "root NamespaceSnapshot should list VM snapshot")
		}).WithTimeout(45 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		var vmContentName string
		Eventually(func(g Gomega) {
			v := &demov1alpha1.DemoVirtualMachineSnapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: "vm-run"}, v)).To(Succeed())
			g.Expect(v.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			vmContentName = v.Status.BoundSnapshotContentName
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			nsc := &storagev1alpha1.NamespaceSnapshotContent{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: rootNSC}, nsc)).To(Succeed())
			var found bool
			for _, ch := range nsc.Status.ChildrenSnapshotContentRefs {
				if ch.Name == vmContentName {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "root NamespaceSnapshotContent should reference VM snapshot content")
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		parentVMRef := &storagev1alpha1.SnapshotSubjectRef{
			APIVersion: demov1alpha1.SchemeGroupVersion.String(),
			Kind:       "DemoVirtualMachineSnapshot",
			Namespace:  nsName,
			Name:       "vm-run",
		}
		disk := &demov1alpha1.DemoVirtualDiskSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "disk-under-vm", Namespace: nsName},
			Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
				RootNamespaceSnapshotRef:            rootRef,
				ParentDemoVirtualMachineSnapshotRef: parentVMRef,
				PersistentVolumeClaimName:           "pr5b-vm-disk-pvc",
			},
		}
		Expect(k8sClient.Create(testCtx, disk)).To(Succeed())

		wantDiskChild := storagev1alpha1.NamespaceSnapshotChildRef{Namespace: nsName, Name: "disk-under-vm"}
		Eventually(func(g Gomega) {
			v := &demov1alpha1.DemoVirtualMachineSnapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: "vm-run"}, v)).To(Succeed())
			var found bool
			for _, ch := range v.Status.ChildrenSnapshotRefs {
				if ch.Namespace == wantDiskChild.Namespace && ch.Name == wantDiskChild.Name {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "VM snapshot should list disk snapshot as child")
		}).WithTimeout(45 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		var diskContentName string
		Eventually(func(g Gomega) {
			d := &demov1alpha1.DemoVirtualDiskSnapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: "disk-under-vm"}, d)).To(Succeed())
			g.Expect(d.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			diskContentName = d.Status.BoundSnapshotContentName
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			vmc := &demov1alpha1.DemoVirtualMachineSnapshotContent{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: vmContentName}, vmc)).To(Succeed())
			var found bool
			for _, ch := range vmc.Status.ChildrenSnapshotContentRefs {
				if ch.Name == diskContentName {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "VM snapshot content should reference disk snapshot content")
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		var vmVisited []string
		var diskVisited []string
		hooks := &usecase.DedicatedContentVisitHooks{
			Visit: func(_ context.Context, gvk schema.GroupVersionKind, contentName string, _ *unstructured.Unstructured, _ bool) error {
				switch gvk.Kind {
				case "DemoVirtualMachineSnapshotContent":
					vmVisited = append(vmVisited, contentName)
				case "DemoVirtualDiskSnapshotContent":
					diskVisited = append(diskVisited, contentName)
				}
				return nil
			},
		}
		err := usecase.WalkNamespaceSnapshotContentSubtreeWithRegistry(testCtx, k8sClient, rootNSC,
			func(_ context.Context, _ *storagev1alpha1.NamespaceSnapshotContent) error {
				return nil
			},
			integrationGraphRegProvider.Current(), hooks,
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(vmVisited).To(ContainElement(vmContentName))
		Expect(diskVisited).To(ContainElement(diskContentName))
	})
})
