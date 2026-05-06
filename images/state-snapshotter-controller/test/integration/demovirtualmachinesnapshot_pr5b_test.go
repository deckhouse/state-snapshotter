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
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func integrationArchiveObjectsFromMCP(ctx context.Context, mcpName string) []map[string]interface{} {
	log, err := logger.NewLogger("error")
	Expect(err).NotTo(HaveOccurred())
	arch := usecase.NewArchiveService(k8sClient, k8sClient, log)
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: mcpName}, mcp)).To(Succeed())
	raw, _, err := arch.GetArchiveFromCheckpoint(ctx, mcp, &usecase.ArchiveRequest{
		CheckpointName:  mcpName,
		CheckpointUID:   string(mcp.UID),
		SourceNamespace: mcp.Spec.SourceNamespace,
	})
	Expect(err).NotTo(HaveOccurred())
	var objects []map[string]interface{}
	Expect(json.Unmarshal(raw, &objects)).To(Succeed())
	return objects
}

func integrationAggregatedObjects(ctx context.Context, contentGVK schema.GroupVersionKind, contentName string) []map[string]interface{} {
	log, err := logger.NewLogger("error")
	Expect(err).NotTo(HaveOccurred())
	arch := usecase.NewArchiveService(k8sClient, k8sClient, log)
	agg := usecase.NewAggregatedNamespaceManifests(k8sClient, arch, integrationGraphRegProvider)
	raw, err := agg.BuildAggregatedJSONFromContent(ctx, contentGVK, contentName)
	Expect(err).NotTo(HaveOccurred())
	var objects []map[string]interface{}
	Expect(json.Unmarshal(raw, &objects)).To(Succeed())
	return objects
}

func integrationObjectsContainKindName(objects []map[string]interface{}, kind, name string) bool {
	for _, obj := range objects {
		if obj["kind"] != kind {
			continue
		}
		meta, ok := obj["metadata"].(map[string]interface{})
		if ok && meta["name"] == name {
			return true
		}
	}
	return false
}

func integrationObjectsContainKind(objects []map[string]interface{}, kind string) bool {
	for _, obj := range objects {
		if obj["kind"] == kind {
			return true
		}
	}
	return false
}

// PR5b: DemoVirtualMachineSnapshot under root Snapshot; DemoVirtualDiskSnapshot as child under VM; ref-only walk sees VM content then disk content.
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
					},
					{
						ResourceCRDName: "demovirtualmachines.demo.state-snapshotter.deckhouse.io",
						SnapshotCRDName: "demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io",
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
		integrationWaitGraphRegistryKind("DemoVirtualMachineSnapshot")
		integrationWaitGraphRegistryKind("DemoVirtualDiskSnapshot")

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

		vmResource := &demov1alpha1.DemoVirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-vm-1", Namespace: nsName},
		}
		Expect(k8sClient.Create(testCtx, vmResource)).To(Succeed())
		diskResource := &demov1alpha1.DemoVirtualDisk{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "demo-disk-1",
				Namespace: nsName,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: demov1alpha1.SchemeGroupVersion.String(),
					Kind:       "DemoVirtualMachine",
					Name:       vmResource.Name,
					UID:        vmResource.UID,
				}},
			},
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
			b := meta.FindStatusCondition(r.Status.Conditions, snapshot.ConditionBound)
			g.Expect(b).NotTo(BeNil())
			g.Expect(b.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(r.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			rootContentName = r.Status.BoundSnapshotContentName
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		var vmSnapshotName string
		Eventually(func(g Gomega) {
			r := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: "root"}, r)).To(Succeed())
			vmSnapshotName = ""
			for _, ch := range r.Status.ChildrenSnapshotRefs {
				if ch.APIVersion == demov1alpha1.SchemeGroupVersion.String() && ch.Kind == "DemoVirtualMachineSnapshot" {
					vmSnapshotName = ch.Name
					break
				}
			}
			g.Expect(vmSnapshotName).NotTo(BeEmpty(), "root Snapshot should list VM snapshot")
		}).WithTimeout(45 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		var vmContentName string
		Eventually(func(g Gomega) {
			v := &demov1alpha1.DemoVirtualMachineSnapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: vmSnapshotName}, v)).To(Succeed())
			g.Expect(v.Spec.SourceRef).To(Equal(demov1alpha1.SnapshotSourceRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       "DemoVirtualMachine",
				Name:       "demo-vm-1",
			}))
			g.Expect(v.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			vmContentName = v.Status.BoundSnapshotContentName
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			content := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: rootContentName}, content)).To(Succeed())
			var found bool
			for _, ch := range content.Status.ChildrenSnapshotContentRefs {
				if ch.Name == vmContentName {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "root SnapshotContent should reference VM snapshot content")
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		var diskSnapshotName string
		Eventually(func(g Gomega) {
			v := &demov1alpha1.DemoVirtualMachineSnapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: vmSnapshotName}, v)).To(Succeed())
			diskSnapshotName = ""
			for _, ch := range v.Status.ChildrenSnapshotRefs {
				if ch.APIVersion == demov1alpha1.SchemeGroupVersion.String() && ch.Kind == "DemoVirtualDiskSnapshot" {
					diskSnapshotName = ch.Name
					break
				}
			}
			g.Expect(diskSnapshotName).NotTo(BeEmpty(), "VM snapshot should list disk snapshot as child")
		}).WithTimeout(45 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		var diskContentName string
		Eventually(func(g Gomega) {
			d := &demov1alpha1.DemoVirtualDiskSnapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: diskSnapshotName}, d)).To(Succeed())
			g.Expect(d.Spec.SourceRef).To(Equal(demov1alpha1.SnapshotSourceRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       "DemoVirtualDisk",
				Name:       "demo-disk-1",
			}))
			g.Expect(d.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			diskContentName = d.Status.BoundSnapshotContentName
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			vmc := &storagev1alpha1.SnapshotContent{}
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

		var visited []string
		err := usecase.WalkSnapshotContentSubtree(testCtx, k8sClient, rootContentName,
			func(_ context.Context, content *storagev1alpha1.SnapshotContent) error {
				visited = append(visited, content.Name)
				return nil
			},
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(visited).To(ContainElement(vmContentName))
		Expect(visited).To(ContainElement(diskContentName))

		// E6: after Snapshot publishes childrenSnapshotRefs, root becomes Ready=True only when
		// all referenced child snapshot objects are ready.
		Eventually(func(g Gomega) {
			r := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: nsName, Name: "root"}, r)).To(Succeed())
			rc := meta.FindStatusCondition(r.Status.Conditions, snapshot.ConditionReady)
			g.Expect(rc).NotTo(BeNil(), "root Snapshot has no Ready condition; status=%+v", r.Status)
			g.Expect(rc.Status).To(Equal(metav1.ConditionTrue), "root Ready: reason=%q message=%q", rc.Reason, rc.Message)
			g.Expect(rc.Reason).To(Equal(snapshot.ReasonCompleted), "root Ready: status=%s message=%q", rc.Status, rc.Message)
		}).WithTimeout(120 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		var rootMCPName, vmMCPName, diskMCPName string
		Eventually(func(g Gomega) {
			rootContent := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(testCtx, client.ObjectKey{Name: rootContentName}, rootContent)).To(Succeed())
			g.Expect(rootContent.Status.ManifestCheckpointName).NotTo(BeEmpty())
			rootMCPName = rootContent.Status.ManifestCheckpointName

			vmContent := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(testCtx, client.ObjectKey{Name: vmContentName}, vmContent)).To(Succeed())
			g.Expect(vmContent.Status.ManifestCheckpointName).NotTo(BeEmpty())
			vmMCPName = vmContent.Status.ManifestCheckpointName

			diskContent := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(testCtx, client.ObjectKey{Name: diskContentName}, diskContent)).To(Succeed())
			g.Expect(diskContent.Status.ManifestCheckpointName).NotTo(BeEmpty())
			diskMCPName = diskContent.Status.ManifestCheckpointName
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		rootObjects := integrationArchiveObjectsFromMCP(testCtx, rootMCPName)
		Expect(integrationObjectsContainKindName(rootObjects, "Namespace", nsName)).To(BeFalse(), "root own MCP must not include the Kubernetes Namespace manifest")
		Expect(integrationObjectsContainKindName(rootObjects, "ConfigMap", "pr5b-cm")).To(BeTrue(), "root own MCP should include namespace-scoped allowlist manifests")
		Expect(integrationObjectsContainKind(rootObjects, "DemoVirtualMachine")).To(BeFalse(), "root own MCP must not include VM child domain manifests")
		Expect(integrationObjectsContainKind(rootObjects, "DemoVirtualDisk")).To(BeFalse(), "root own MCP must not include disk child domain manifests")

		vmObjects := integrationArchiveObjectsFromMCP(testCtx, vmMCPName)
		Expect(integrationObjectsContainKindName(vmObjects, "DemoVirtualMachine", "demo-vm-1")).To(BeTrue(), "VM own MCP should include source VM manifest")
		Expect(integrationObjectsContainKind(vmObjects, "DemoVirtualDisk")).To(BeFalse(), "VM own MCP must not include disk child domain manifests")

		diskObjects := integrationArchiveObjectsFromMCP(testCtx, diskMCPName)
		Expect(integrationObjectsContainKindName(diskObjects, "DemoVirtualDisk", "demo-disk-1")).To(BeTrue(), "disk own MCP should include source disk manifest")
		Expect(integrationObjectsContainKind(diskObjects, "DemoVirtualMachine")).To(BeFalse(), "disk own MCP must not include ancestor manifests")

		rootAggregated := integrationAggregatedObjects(testCtx, usecase.SnapshotContentGVK(), rootContentName)
		Expect(integrationObjectsContainKindName(rootAggregated, "Namespace", nsName)).To(BeFalse())
		Expect(integrationObjectsContainKindName(rootAggregated, "DemoVirtualMachine", "demo-vm-1")).To(BeTrue())
		Expect(integrationObjectsContainKindName(rootAggregated, "DemoVirtualDisk", "demo-disk-1")).To(BeTrue())

		vmContentGVK := usecase.SnapshotContentGVK()
		diskContentGVK := usecase.SnapshotContentGVK()
		vmAggregated := integrationAggregatedObjects(testCtx, vmContentGVK, vmContentName)
		Expect(integrationObjectsContainKindName(vmAggregated, "Namespace", nsName)).To(BeFalse(), "VM subtree read must not include ancestor Namespace manifest")
		Expect(integrationObjectsContainKindName(vmAggregated, "DemoVirtualMachine", "demo-vm-1")).To(BeTrue())
		Expect(integrationObjectsContainKindName(vmAggregated, "DemoVirtualDisk", "demo-disk-1")).To(BeTrue())

		diskAggregated := integrationAggregatedObjects(testCtx, diskContentGVK, diskContentName)
		Expect(integrationObjectsContainKindName(diskAggregated, "Namespace", nsName)).To(BeFalse(), "disk leaf read must not include ancestor Namespace manifest")
		Expect(integrationObjectsContainKind(diskAggregated, "DemoVirtualMachine")).To(BeFalse(), "disk leaf read must not include VM ancestor manifest")
		Expect(integrationObjectsContainKindName(diskAggregated, "DemoVirtualDisk", "demo-disk-1")).To(BeTrue())
	})
})
