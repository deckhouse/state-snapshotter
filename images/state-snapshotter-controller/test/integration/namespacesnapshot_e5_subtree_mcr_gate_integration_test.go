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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// E5 finish line: delayed first root MCR — no ManifestCaptureRequest until child subtree MCP is Ready
// (exclude-set computable). Reconcile-path assertions (not usecase-only).
var _ = Describe("Integration: E5 subtree root MCR gate (delayed first MCR)", func() {
	It("does not create root MCR until child NamespaceSnapshotContent has a Ready ManifestCheckpoint", func() {
		ctx := context.Background()
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-e5-mcr-gate-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "namespacesnapshot-e5-mcr-gate",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "e5-gate-cm", Namespace: nsName},
			Data:       map[string]string{"k": "v"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		parentName := "e5-gate-parent"
		parent := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      parentName,
				Namespace: nsName,
				Annotations: map[string]string{
					namespacemanifest.AnnotationSyntheticChildTree: "true",
				},
			},
			Spec: storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, parent)).To(Succeed())
		parentKey := types.NamespacedName{Namespace: nsName, Name: parentName}

		Eventually(func(g Gomega) {
			p := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			b := meta.FindStatusCondition(p.Status.Conditions, snapshot.ConditionBound)
			g.Expect(b).NotTo(BeNil())
			g.Expect(b.Status).To(Equal(metav1.ConditionTrue))
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			p := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			g.Expect(p.Status.ChildrenSnapshotRefs).To(HaveLen(1))
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			p := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			g.Expect(p.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			pc := &storagev1alpha1.NamespaceSnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: p.Status.BoundSnapshotContentName}, pc)).To(Succeed())
			g.Expect(pc.Status.ChildrenSnapshotContentRefs).NotTo(BeEmpty())
			chName := pc.Status.ChildrenSnapshotContentRefs[0].Name
			chNSC := &storagev1alpha1.NamespaceSnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: chName}, chNSC)).To(Succeed())

			mcrName := namespacemanifest.NamespaceSnapshotMCRName(p.UID)
			if chNSC.Status.ManifestCheckpointName == "" {
				err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: mcrName}, &ssv1alpha1.ManifestCaptureRequest{})
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "root MCR must not exist before child subtree has manifestCheckpointName")
				g.Expect(false).To(BeTrue(), "waiting for child NamespaceSnapshotContent.manifestCheckpointName")
			}
			mcp := &ssv1alpha1.ManifestCheckpoint{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: chNSC.Status.ManifestCheckpointName}, mcp)).To(Succeed())
			rc := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
			if rc == nil || rc.Status != metav1.ConditionTrue {
				err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: mcrName}, &ssv1alpha1.ManifestCaptureRequest{})
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "root MCR must not exist until child subtree MCP is Ready (exclude set must be complete)")
				g.Expect(false).To(BeTrue(), "waiting for child subtree ManifestCheckpoint Ready")
			}
			mcr := &ssv1alpha1.ManifestCaptureRequest{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: mcrName}, mcr)).To(Succeed(), "root MCR must exist once child subtree MCP is Ready")
			for _, tgt := range mcr.Spec.Targets {
				isGateCM := tgt.APIVersion == "v1" && tgt.Kind == "ConfigMap" && tgt.Name == "e5-gate-cm"
				g.Expect(isGateCM).To(BeFalse(), "root MCR must exclude ConfigMap captured under child subtree MCP")
			}
			g.Expect(true).To(BeTrue())
		}, 240*time.Second, 150*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			p := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			pr := meta.FindStatusCondition(p.Status.Conditions, snapshot.ConditionReady)
			g.Expect(pr).NotTo(BeNil())
			g.Expect(pr.Status).To(Equal(metav1.ConditionTrue))
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})
