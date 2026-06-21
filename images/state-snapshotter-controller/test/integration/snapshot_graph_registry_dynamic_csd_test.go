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

// The graph registry is kind-agnostic: it registers any CSD-eligible snapshot kind whose source and
// snapshot GVKs resolve. This spec uses generic test kinds (no domain/demo coupling) — a CSD maps the
// generic source TestSnapshot to the dedicated, non-built-in snapshot kind GraphRegistryTestSnapshot.
// GraphRegistryTestSnapshot is referenced ONLY by this spec, so presence/absence assertions against the
// global graph registry are not polluted by CSDs created by other specs.
const (
	registryTestSourceAPIVersion   = "test.deckhouse.io/v1alpha1"
	registryTestSourceKind         = "TestSnapshot"
	registryTestSnapshotAPIVersion = "test.deckhouse.io/v1alpha1"
	registryTestSnapshotKind       = "GraphRegistryTestSnapshot"
)

func registryTestSnapshotMapping() []ssv1alpha1.SnapshotResourceMappingEntry {
	return []ssv1alpha1.SnapshotResourceMappingEntry{
		{
			Source: ssv1alpha1.SnapshotGVKRef{
				APIVersion: registryTestSourceAPIVersion,
				Kind:       registryTestSourceKind,
			},
			Snapshot: ssv1alpha1.SnapshotGVKRef{
				APIVersion: registryTestSnapshotAPIVersion,
				Kind:       registryTestSnapshotKind,
			},
		},
	}
}

// markCSDRBACReady simulates the Deckhouse hook leg of CSD eligibility (RBACReady=True) for the
// current generation, so CSDWatchEligible becomes true once the reconciler has set Accepted=True.
func markCSDRBACReady(ctx context.Context, name, message string) {
	hook := &ssv1alpha1.CustomSnapshotDefinition{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, hook)).To(Succeed())
	gen := hook.GetGeneration()
	meta.SetStatusCondition(&hook.Status.Conditions, metav1.Condition{
		Type:               controllers.CSDConditionRBACReady,
		Status:             metav1.ConditionTrue,
		Reason:             "IntegrationHook",
		Message:            message,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: gen,
	})
	Expect(k8sClient.Status().Update(ctx, hook)).To(Succeed())
}

func waitCSDAccepted(ctx context.Context, name string) {
	Eventually(func(g Gomega) {
		cur := &ssv1alpha1.CustomSnapshotDefinition{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, cur)).To(Succeed())
		acc := meta.FindStatusCondition(cur.Status.Conditions, controllers.CSDConditionAccepted)
		g.Expect(acc).NotTo(BeNil())
		g.Expect(acc.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}

// Serial: mutates the global integration graph registry through CSD refreshes.
var _ = Describe("Integration: snapshot graph registry (CSD-driven refresh)", Serial, func() {
	const csdName = "integration-dynamic-graph-registry-csd"

	BeforeEach(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.CustomSnapshotDefinition{ObjectMeta: metav1.ObjectMeta{Name: csdName}}))
	})

	AfterEach(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.CustomSnapshotDefinition{ObjectMeta: metav1.ObjectMeta{Name: csdName}}))
		Expect(integrationSnapshotGraphRegistryRefresh(context.Background())).To(Succeed())
	})

	It("adds a CSD-gated snapshot kind after eligible CSD without restarting the process", func() {
		testCtx := context.Background()
		localCfg := *testCfg
		localCfg.UnifiedBootstrapMode = config.UnifiedBootstrapEmpty

		p, err := snapshotgraphregistry.NewProvider(&localCfg, mgr.GetRESTMapper(), k8sClient, ctrl.Log.WithName("dynamic-graph-registry-test"))
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Refresh(testCtx)).To(Succeed())
		Expect(p.Current().RegisteredSnapshotKinds()).NotTo(ContainElement(registryTestSnapshotKind))

		csd := &ssv1alpha1.CustomSnapshotDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: csdName},
			Spec: ssv1alpha1.CustomSnapshotDefinitionSpec{
				SnapshotResourceMapping: registryTestSnapshotMapping(),
			},
		}
		Expect(k8sClient.Create(testCtx, csd)).To(Succeed())

		waitCSDAccepted(testCtx, csdName)
		markCSDRBACReady(testCtx, csdName, "dynamic graph registry")

		Eventually(func(g Gomega) {
			g.Expect(p.Refresh(testCtx)).To(Succeed())
			kinds := p.Current().RegisteredSnapshotKinds()
			g.Expect(kinds).To(ContainElement(registryTestSnapshotKind))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		Expect(k8sClient.Delete(testCtx, &ssv1alpha1.CustomSnapshotDefinition{ObjectMeta: metav1.ObjectMeta{Name: csdName}})).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(p.Refresh(testCtx)).To(Succeed())
			kinds := p.Current().RegisteredSnapshotKinds()
			g.Expect(kinds).NotTo(ContainElement(registryTestSnapshotKind))
		}).WithTimeout(30 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})

	It("global registry refresh is fail-closed for CSD-gated kinds until CSD", func() {
		testCtx := context.Background()
		Expect(integrationSnapshotGraphRegistryRefresh(testCtx)).To(Succeed())
		Expect(integrationGraphRegProvider.Current().RegisteredSnapshotKinds()).NotTo(ContainElement(registryTestSnapshotKind))
	})

	It("global graph registry loses CSD-gated pair after CSD delete without manual Refresh (CSD reconcile only)", func() {
		testCtx := context.Background()
		const globalCSD = "integration-global-graph-csd-delete"
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.CustomSnapshotDefinition{ObjectMeta: metav1.ObjectMeta{Name: globalCSD}}))
			Expect(integrationSnapshotGraphRegistryRefresh(context.Background())).To(Succeed())
		})
		Expect(integrationSnapshotGraphRegistryRefresh(testCtx)).To(Succeed())

		csd := &ssv1alpha1.CustomSnapshotDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: globalCSD},
			Spec: ssv1alpha1.CustomSnapshotDefinitionSpec{
				SnapshotResourceMapping: registryTestSnapshotMapping(),
			},
		}
		Expect(k8sClient.Create(testCtx, csd)).To(Succeed())

		waitCSDAccepted(testCtx, globalCSD)
		markCSDRBACReady(testCtx, globalCSD, "global graph delete test")

		Eventually(func(g Gomega) {
			kinds := integrationGraphRegProvider.Current().RegisteredSnapshotKinds()
			g.Expect(kinds).To(ContainElement(registryTestSnapshotKind))
		}).WithTimeout(60*time.Second).WithPolling(200*time.Millisecond).Should(Succeed(),
			"CSD reconciler must run GraphRegistryRefresh after eligibility; CSD-gated kind should appear")

		Expect(k8sClient.Delete(testCtx, &ssv1alpha1.CustomSnapshotDefinition{ObjectMeta: metav1.ObjectMeta{Name: globalCSD}})).To(Succeed())

		Eventually(func(g Gomega) {
			kinds := integrationGraphRegProvider.Current().RegisteredSnapshotKinds()
			g.Expect(kinds).NotTo(ContainElement(registryTestSnapshotKind))
		}).WithTimeout(60*time.Second).WithPolling(200*time.Millisecond).Should(Succeed(),
			"after CSD delete, next CSD reconcile must refresh graph registry without stale CSD-gated kind")
	})
})
