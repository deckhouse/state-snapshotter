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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
)

// Serial: toggles global integrationGraphAppendDemo used by integrationSnapshotGraphRegistryRefresh.
var _ = Describe("Integration: snapshot graph registry (DSC-driven refresh)", Serial, func() {
	const dscName = "integration-dynamic-graph-registry-dsc"

	BeforeEach(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.DomainSpecificSnapshotController{ObjectMeta: metav1.ObjectMeta{Name: dscName}}))
	})

	AfterEach(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.DomainSpecificSnapshotController{ObjectMeta: metav1.ObjectMeta{Name: dscName}}))
		integrationGraphAppendDemo = true
		Expect(integrationSnapshotGraphRegistryRefresh(context.Background())).To(Succeed())
	})

	It("adds demo disk snapshot kinds after eligible DSC without restarting the process", func() {
		testCtx := context.Background()
		localCfg := *testCfg
		localCfg.UnifiedBootstrapMode = config.UnifiedBootstrapEmpty

		p, err := snapshotgraphregistry.NewProvider(&localCfg, mgr.GetRESTMapper(), k8sClient, ctrl.Log.WithName("dynamic-graph-registry-test"))
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Refresh(testCtx)).To(Succeed())
		Expect(p.Current().RegisteredSnapshotKinds()).NotTo(ContainElement("DemoVirtualDiskSnapshot"))

		dsc := &ssv1alpha1.DomainSpecificSnapshotController{
			ObjectMeta: metav1.ObjectMeta{Name: dscName},
			Spec: ssv1alpha1.DomainSpecificSnapshotControllerSpec{
				OwnerModule: "integration-dynamic-graph",
				SnapshotResourceMapping: []ssv1alpha1.SnapshotResourceMappingEntry{
					{
						ResourceCRDName: "demovirtualdisks.demo.state-snapshotter.deckhouse.io",
						SnapshotCRDName: "demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io",
						ContentCRDName:  "demovirtualdisksnapshotcontents.demo.state-snapshotter.deckhouse.io",
					},
				},
			},
		}
		Expect(k8sClient.Create(testCtx, dsc)).To(Succeed())

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
			Message:            "dynamic graph registry",
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: gen,
		})
		Expect(k8sClient.Status().Update(testCtx, hook)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(p.Refresh(testCtx)).To(Succeed())
			kinds := p.Current().RegisteredSnapshotKinds()
			g.Expect(kinds).To(ContainElement("DemoVirtualDiskSnapshot"))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		Expect(k8sClient.Delete(testCtx, &ssv1alpha1.DomainSpecificSnapshotController{ObjectMeta: metav1.ObjectMeta{Name: dscName}})).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(p.Refresh(testCtx)).To(Succeed())
			kinds := p.Current().RegisteredSnapshotKinds()
			g.Expect(kinds).NotTo(ContainElement("DemoVirtualDiskSnapshot"))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})

	It("global registry refresh respects integrationGraphAppendDemo=false (fail-closed for demo until DSC)", func() {
		testCtx := context.Background()
		integrationGraphAppendDemo = false
		DeferCleanup(func() {
			integrationGraphAppendDemo = true
			Expect(integrationSnapshotGraphRegistryRefresh(context.Background())).To(Succeed())
		})
		Expect(integrationSnapshotGraphRegistryRefresh(testCtx)).To(Succeed())
		Expect(integrationGraphRegProvider.Current().RegisteredSnapshotKinds()).NotTo(ContainElement("DemoVirtualDiskSnapshot"))
	})

	It("global graph registry loses demo disk pair after DSC delete without manual Refresh (DSC reconcile only)", func() {
		testCtx := context.Background()
		const globalDSC = "integration-global-graph-dsc-delete"
		integrationGraphAppendDemo = false
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.DomainSpecificSnapshotController{ObjectMeta: metav1.ObjectMeta{Name: globalDSC}}))
			integrationGraphAppendDemo = true
			Expect(integrationSnapshotGraphRegistryRefresh(context.Background())).To(Succeed())
		})
		Expect(integrationSnapshotGraphRegistryRefresh(testCtx)).To(Succeed())

		dsc := &ssv1alpha1.DomainSpecificSnapshotController{
			ObjectMeta: metav1.ObjectMeta{Name: globalDSC},
			Spec: ssv1alpha1.DomainSpecificSnapshotControllerSpec{
				OwnerModule: "integration-global-graph-delete",
				SnapshotResourceMapping: []ssv1alpha1.SnapshotResourceMappingEntry{
					{
						ResourceCRDName: "demovirtualdisks.demo.state-snapshotter.deckhouse.io",
						SnapshotCRDName: "demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io",
						ContentCRDName:  "demovirtualdisksnapshotcontents.demo.state-snapshotter.deckhouse.io",
					},
				},
			},
		}
		Expect(k8sClient.Create(testCtx, dsc)).To(Succeed())

		Eventually(func(g Gomega) {
			cur := &ssv1alpha1.DomainSpecificSnapshotController{}
			g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: globalDSC}, cur)).To(Succeed())
			acc := meta.FindStatusCondition(cur.Status.Conditions, controllers.DSCConditionAccepted)
			g.Expect(acc).NotTo(BeNil())
			g.Expect(acc.Status).To(Equal(metav1.ConditionTrue))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		hook := &ssv1alpha1.DomainSpecificSnapshotController{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: globalDSC}, hook)).To(Succeed())
		gen := hook.GetGeneration()
		meta.SetStatusCondition(&hook.Status.Conditions, metav1.Condition{
			Type:               controllers.DSCConditionRBACReady,
			Status:             metav1.ConditionTrue,
			Reason:             "IntegrationHook",
			Message:            "global graph delete test",
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: gen,
		})
		Expect(k8sClient.Status().Update(testCtx, hook)).To(Succeed())

		Eventually(func(g Gomega) {
			kinds := integrationGraphRegProvider.Current().RegisteredSnapshotKinds()
			g.Expect(kinds).To(ContainElement("DemoVirtualDiskSnapshot"))
		}).WithTimeout(60*time.Second).WithPolling(200*time.Millisecond).Should(Succeed(),
			"DSC reconciler must run GraphRegistryRefresh after eligibility; demo disk kind should appear")

		Expect(k8sClient.Delete(testCtx, &ssv1alpha1.DomainSpecificSnapshotController{ObjectMeta: metav1.ObjectMeta{Name: globalDSC}})).To(Succeed())

		Eventually(func(g Gomega) {
			kinds := integrationGraphRegProvider.Current().RegisteredSnapshotKinds()
			g.Expect(kinds).NotTo(ContainElement("DemoVirtualDiskSnapshot"))
		}).WithTimeout(60*time.Second).WithPolling(200*time.Millisecond).Should(Succeed(),
			"after DSC delete, next DSC reconcile must refresh graph registry without stale demo kind")
	})
})
