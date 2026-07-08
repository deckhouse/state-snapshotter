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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// childGraphSeed is one (child Snapshot, its bound child SnapshotContent) pair for the integration graph seed.
type childGraphSeed struct {
	snapshotName string
	contentName  string
}

// mergeChildGraphIntoRoot wires ONE child into the root graph (single-child convenience wrapper). The root
// content's childrenSnapshotContentRefs goes empty -> [child] in one write, which the Block 4 frozen-set CEL
// (Option A) allows (oldSelf.size()==0).
func mergeChildGraphIntoRoot(ctx context.Context, c client.Client, rootNS, rootName, childNSSName, childSnapshotContentName string) error {
	return mergeChildrenGraphIntoRoot(ctx, c, rootNS, rootName, []childGraphSeed{{snapshotName: childNSSName, contentName: childSnapshotContentName}})
}

// mergeChildrenGraphIntoRoot wires status.childrenSnapshotRefs on the root Snapshot and the matching
// childrenSnapshotContentRefs on the root SnapshotContent (integration seed only), writing the COMPLETE
// child-content set in a SINGLE status update. Block 4 (INV-CONTENT-CHILDREN-2) freezes
// childrenSnapshotContentRefs once non-empty (Option A CEL), so seeding children one-by-one — which grows a
// non-empty set — is rejected; a multi-child tree must be seeded atomically here (this mirrors production,
// where the aggregator publishes the complete frozen set all-or-nothing).
func mergeChildrenGraphIntoRoot(ctx context.Context, c client.Client, rootNS, rootName string, children []childGraphSeed) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		p := &storagev1alpha1.Snapshot{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: rootNS, Name: rootName}, p); err != nil {
			return err
		}
		controller := true
		// Owner-ref every child Snapshot to the root and union its edge into the root's (append-only)
		// childrenSnapshotRefs; write that once.
		refsChanged := false
		for _, seed := range children {
			child := &storagev1alpha1.Snapshot{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: rootNS, Name: seed.snapshotName}, child); err != nil {
				return err
			}
			childBase := child.DeepCopy()
			child.OwnerReferences = replaceControllerOwnerRefForIntegration(child.OwnerReferences, metav1.OwnerReference{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "Snapshot",
				Name:       p.Name,
				UID:        p.UID,
				Controller: &controller,
			})
			if err := c.Patch(ctx, child, client.MergeFrom(childBase)); err != nil {
				return err
			}
			want := storagev1alpha1.SnapshotChildRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "Snapshot",
				Name:       seed.snapshotName,
			}
			found := false
			for _, r := range p.Status.ChildrenSnapshotRefs {
				if r.APIVersion == want.APIVersion && r.Kind == want.Kind && r.Name == want.Name {
					found = true
					break
				}
			}
			if !found {
				p.Status.ChildrenSnapshotRefs = append(p.Status.ChildrenSnapshotRefs, want)
				refsChanged = true
			}
		}
		if refsChanged {
			if err := c.Status().Update(ctx, p); err != nil {
				return err
			}
		}
		if err := c.Get(ctx, types.NamespacedName{Namespace: rootNS, Name: rootName}, p); err != nil {
			return err
		}
		rootContentName := p.Status.BoundSnapshotContentName
		if rootContentName == "" {
			return fmt.Errorf("root has no bound SnapshotContent yet")
		}
		pc := &storagev1alpha1.SnapshotContent{}
		if err := c.Get(ctx, client.ObjectKey{Name: rootContentName}, pc); err != nil {
			return err
		}
		// Owner-ref each child SnapshotContent, then build the COMPLETE content-refs set and write ONCE.
		desired := append([]storagev1alpha1.SnapshotContentChildRef(nil), pc.Status.ChildrenSnapshotContentRefs...)
		for _, seed := range children {
			childContent := &storagev1alpha1.SnapshotContent{}
			if err := c.Get(ctx, client.ObjectKey{Name: seed.contentName}, childContent); err != nil {
				return err
			}
			childContentBase := childContent.DeepCopy()
			childContent.OwnerReferences = replaceControllerOwnerRefForIntegration(childContent.OwnerReferences, metav1.OwnerReference{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "SnapshotContent",
				Name:       pc.Name,
				UID:        pc.UID,
				Controller: &controller,
			})
			if err := c.Patch(ctx, childContent, client.MergeFrom(childContentBase)); err != nil {
				return err
			}
			exists := false
			for _, r := range desired {
				if r.Name == seed.contentName {
					exists = true
					break
				}
			}
			if !exists {
				desired = append(desired, storagev1alpha1.SnapshotContentChildRef{Name: seed.contentName})
			}
		}
		if len(desired) == len(pc.Status.ChildrenSnapshotContentRefs) {
			return nil
		}
		pc.Status.ChildrenSnapshotContentRefs = desired
		return c.Status().Update(ctx, pc)
	})
}

func replaceControllerOwnerRefForIntegration(existing []metav1.OwnerReference, desired metav1.OwnerReference) []metav1.OwnerReference {
	// Test-only graph grafting for synthetic Snapshot trees.
	// Production ownerRef changes must go through lifecycle helpers that fail closed on conflicting owners.
	out := make([]metav1.OwnerReference, 0, len(existing)+1)
	for _, ref := range existing {
		if ref.Controller != nil && *ref.Controller {
			continue
		}
		out = append(out, ref)
	}
	return append(out, desired)
}

var _ = Describe("Integration: E5 subtree root MCR gate (registered child snapshot kind fixture)", func() {
	It("does not create root MCR until child snapshot content subtree MCP is Ready", func() {
		ctx := context.Background()
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-e5-graph-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "snapshot-e5-graph",
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

		childName := "e5-subtree-child"
		child := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: childName, Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, child)).To(Succeed())

		childKey := types.NamespacedName{Namespace: nsName, Name: childName}
		var childSnapshotContent string
		Eventually(func(g Gomega) {
			ch := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, childKey, ch)).To(Succeed())
			g.Expect(ch.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			childSnapshotContent = ch.Status.BoundSnapshotContentName
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			ch := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, childKey, ch)).To(Succeed())
			rc := meta.FindStatusCondition(ch.Status.Conditions, snapshot.ConditionReady)
			g.Expect(rc).NotTo(BeNil())
			g.Expect(rc.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(rc.Reason).To(Equal(snapshot.ReasonCompleted))
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		parentName := "e5-subtree-root"
		parent := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: parentName, Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, parent)).To(Succeed())
		parentKey := types.NamespacedName{Namespace: nsName, Name: parentName}

		Eventually(func(g Gomega) {
			p := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			g.Expect(p.Status.BoundSnapshotContentName).NotTo(BeEmpty())
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		Expect(mergeChildGraphIntoRoot(ctx, k8sClient, nsName, parentName, childName, childSnapshotContent)).To(Succeed())

		Eventually(func(g Gomega) {
			p := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			g.Expect(p.Status.BoundSnapshotContentName).NotTo(BeEmpty())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		// E5: while the child subtree MCP is not Ready, root must not run a completed first MCR plan (gate or absent MCR).
		Eventually(func(g Gomega) {
			p := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			pc := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: p.Status.BoundSnapshotContentName}, pc)).To(Succeed())
			g.Expect(pc.Status.ChildrenSnapshotContentRefs).NotTo(BeEmpty())
			chName := pc.Status.ChildrenSnapshotContentRefs[0].Name
			chSnapshotContent := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: chName}, chSnapshotContent)).To(Succeed())

			mcrName := namespacemanifest.SnapshotMCRName(p.UID)
			if chSnapshotContent.Status.ManifestCheckpointName == "" {
				err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: mcrName}, &ssv1alpha1.ManifestCaptureRequest{})
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "root MCR must not exist before child subtree has manifestCheckpointName")
				g.Expect(false).To(BeTrue(), "waiting for child snapshot content manifestCheckpointName")
			}
			mcp := &ssv1alpha1.ManifestCheckpoint{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: chSnapshotContent.Status.ManifestCheckpointName}, mcp)).To(Succeed())
			rc := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
			if rc == nil || rc.Status != metav1.ConditionTrue {
				err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: mcrName}, &ssv1alpha1.ManifestCaptureRequest{})
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "root MCR must not exist until child subtree MCP is Ready")
				g.Expect(false).To(BeTrue(), "waiting for child subtree ManifestCheckpoint Ready")
			}
			g.Expect(true).To(BeTrue())
		}, 240*time.Second, 150*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			p := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			pr := meta.FindStatusCondition(p.Status.Conditions, snapshot.ConditionReady)
			g.Expect(pr).NotTo(BeNil())
			g.Expect(pr.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(pr.Reason).To(Equal(snapshot.ReasonCompleted))
		}, 180*time.Second, 200*time.Millisecond).Should(Succeed())

		// If root MCR still exists briefly, it must not list the ConfigMap already captured under the child subtree.
		pFinal := &storagev1alpha1.Snapshot{}
		Expect(k8sClient.Get(ctx, parentKey, pFinal)).To(Succeed())
		mcrName := namespacemanifest.SnapshotMCRName(pFinal.UID)
		mcr := &ssv1alpha1.ManifestCaptureRequest{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: mcrName}, mcr); err == nil {
			for _, tgt := range mcr.Spec.Targets {
				isGateCM := tgt.APIVersion == "v1" && tgt.Kind == "ConfigMap" && tgt.Name == "e5-gate-cm"
				Expect(isGateCM).To(BeFalse(), "if root MCR still exists, it must exclude ConfigMap captured under child subtree MCP")
			}
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
	})
})

var _ = Describe("Integration: terminal child-Snapshot failure bridge sets parent Ready=False", func() {
	It("sets parent Ready=False ChildrenFailed when child hits terminal capture failure", func() {
		// SKIPPED under envtest: this scenario cannot be synthesized deterministically here. A live
		// ManifestCheckpoint controller runs in this suite, so the child's own capture completes and the
		// child Snapshot.Ready mirrors its bound SnapshotContent back on every reconcile (~500ms). An
		// injected terminal child Ready therefore never persists, and there is no way to force a genuine
		// terminal child capture failure (the CSI VolumeSnapshotClass/Content needed for a real volume
		// failure are intentionally not installed here). The child-Snapshot terminal-failure bridge
		// (INV-FAIL-PROP) is covered deterministically by unit tests in
		// internal/usecase/child_snapshot_terminal_failures_test.go (terminal-reason set, classifier, and
		// SummarizeChildSnapshotTerminalFailures) and end-to-end by e2e/tests/child_bridge_failure_test.go.
		Skip("child-Snapshot terminal-failure bridge is not reproducible under envtest; see unit tests + e2e child-bridge spec")
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-e6-child-fail-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "snapshot-e6-child-fail",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-e6-fail-cm", Namespace: nsName},
			Data:       map[string]string{"k": "v"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		parentName := "parent-e6-fail"
		childName := "child-e6-fail"

		parent := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: parentName, Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, parent)).To(Succeed())

		child := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: childName, Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, child)).To(Succeed())

		parentKey := types.NamespacedName{Namespace: nsName, Name: parentName}
		childKey := types.NamespacedName{Namespace: nsName, Name: childName}

		Eventually(func(g Gomega) {
			ch := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, childKey, ch)).To(Succeed())
			g.Expect(ch.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			g.Expect(ch.UID).NotTo(BeEmpty())
		}, 180*time.Second, 200*time.Millisecond).Should(Succeed())

		childSnap := &storagev1alpha1.Snapshot{}
		Expect(k8sClient.Get(ctx, childKey, childSnap)).To(Succeed())

		Expect(mergeChildGraphIntoRoot(ctx, k8sClient, nsName, parentName, childName, childSnap.Status.BoundSnapshotContentName)).To(Succeed())

		Eventually(func(g Gomega) {
			childFresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, childKey, childFresh)).To(Succeed())
			childBase := childFresh.DeepCopy()
			meta.SetStatusCondition(&childFresh.Status.Conditions, metav1.Condition{
				Type:               snapshot.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             "CapturePlanDrift",
				Message:            "integration terminal child capture failure",
				ObservedGeneration: childFresh.Generation,
				LastTransitionTime: metav1.Now(),
			})
			g.Expect(k8sClient.Status().Patch(ctx, childFresh, client.MergeFrom(childBase))).To(Succeed())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			ch := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, childKey, ch)).To(Succeed())
			cr := meta.FindStatusCondition(ch.Status.Conditions, snapshot.ConditionReady)
			g.Expect(cr).NotTo(BeNil())
			g.Expect(cr.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cr.Reason).To(Equal("CapturePlanDrift"))
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			pKick := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, pKick)).To(Succeed())
			pKickBase := pKick.DeepCopy()
			if pKick.Annotations == nil {
				pKick.Annotations = map[string]string{}
			}
			pKick.Annotations["state-snapshotter.deckhouse.io/integration-parent-kick"] = fmt.Sprintf("%d", time.Now().UnixNano())
			g.Expect(k8sClient.Patch(ctx, pKick, client.MergeFrom(pKickBase))).To(Succeed())

			p := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			pr := meta.FindStatusCondition(p.Status.Conditions, snapshot.ConditionReady)
			g.Expect(pr).NotTo(BeNil())
			g.Expect(pr.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(pr.Reason).To(Equal(snapshot.ReasonChildrenFailed))
			g.Expect(pr.Message).To(ContainSubstring(childKey.String()))
			g.Expect(pr.Message).To(ContainSubstring("CapturePlanDrift"))
		}, 180*time.Second, 300*time.Millisecond).Should(Succeed())
	})

	// Regression (INV-FAIL-PROP): a child whose VOLUME capture fails terminally
	// (Ready=False/VolumeCaptureFailed — a domain snapshot path) must propagate to the
	// parent Snapshot.Ready=False/ChildrenFailed. The child's bound SnapshotContent cannot represent this
	// (its data leg reads from an empty dataRefs[] and reports ready), so the child-Snapshot terminal
	// failure bridge is the only path — and VolumeCaptureFailed was previously missing from its terminal
	// reason set, leaving the parent stale Ready=True over lost volume data.
	It("sets parent Ready=False ChildrenFailed when child hits terminal VolumeCaptureFailed", func() {
		// SKIPPED under envtest: same reason as the sibling spec above. The child's live reconcile
		// re-mirrors its bound SnapshotContent.Ready over any injected VolumeCaptureFailed, and a genuine
		// terminal volume-capture failure cannot be provoked here (no CSI VolumeSnapshotClass/Content). The
		// specific VolumeCaptureFailed regression this guards ("VolumeCaptureFailed was previously missing
		// from the terminal reason set") is covered deterministically by the unit tests
		// TestSummarizeChildSnapshotTerminalFailures_VolumeCaptureFailedIsTerminal /
		// TestChildSnapshotTerminalReadyReasons_IncludesVolumeCaptureTerminals, and end-to-end by
		// e2e/tests/child_bridge_failure_test.go (real domain-disk terminal volume capture -> parent ChildrenFailed).
		Skip("child-Snapshot terminal-failure bridge is not reproducible under envtest; see unit tests + e2e child-bridge spec")
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-e6-volfail-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "snapshot-e6-volfail",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-e6-volfail-cm", Namespace: nsName},
			Data:       map[string]string{"k": "v"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		parentName := "parent-e6-volfail"
		childName := "child-e6-volfail"

		parent := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: parentName, Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, parent)).To(Succeed())

		child := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: childName, Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, child)).To(Succeed())

		parentKey := types.NamespacedName{Namespace: nsName, Name: parentName}
		childKey := types.NamespacedName{Namespace: nsName, Name: childName}

		Eventually(func(g Gomega) {
			ch := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, childKey, ch)).To(Succeed())
			g.Expect(ch.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			g.Expect(ch.UID).NotTo(BeEmpty())
		}, 180*time.Second, 200*time.Millisecond).Should(Succeed())

		childSnap := &storagev1alpha1.Snapshot{}
		Expect(k8sClient.Get(ctx, childKey, childSnap)).To(Succeed())

		Expect(mergeChildGraphIntoRoot(ctx, k8sClient, nsName, parentName, childName, childSnap.Status.BoundSnapshotContentName)).To(Succeed())

		Eventually(func(g Gomega) {
			childFresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, childKey, childFresh)).To(Succeed())
			childBase := childFresh.DeepCopy()
			meta.SetStatusCondition(&childFresh.Status.Conditions, metav1.Condition{
				Type:               snapshot.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             snapshot.ReasonVolumeCaptureFailed,
				Message:            "integration terminal volume capture failure",
				ObservedGeneration: childFresh.Generation,
				LastTransitionTime: metav1.Now(),
			})
			g.Expect(k8sClient.Status().Patch(ctx, childFresh, client.MergeFrom(childBase))).To(Succeed())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			ch := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, childKey, ch)).To(Succeed())
			cr := meta.FindStatusCondition(ch.Status.Conditions, snapshot.ConditionReady)
			g.Expect(cr).NotTo(BeNil())
			g.Expect(cr.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cr.Reason).To(Equal(snapshot.ReasonVolumeCaptureFailed))
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			pKick := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, pKick)).To(Succeed())
			pKickBase := pKick.DeepCopy()
			if pKick.Annotations == nil {
				pKick.Annotations = map[string]string{}
			}
			pKick.Annotations["state-snapshotter.deckhouse.io/integration-parent-kick"] = fmt.Sprintf("%d", time.Now().UnixNano())
			g.Expect(k8sClient.Patch(ctx, pKick, client.MergeFrom(pKickBase))).To(Succeed())

			p := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, parentKey, p)).To(Succeed())
			pr := meta.FindStatusCondition(p.Status.Conditions, snapshot.ConditionReady)
			g.Expect(pr).NotTo(BeNil())
			g.Expect(pr.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(pr.Reason).To(Equal(snapshot.ReasonChildrenFailed))
			g.Expect(pr.Message).To(ContainSubstring(childKey.String()))
			g.Expect(pr.Message).To(ContainSubstring(snapshot.ReasonVolumeCaptureFailed))
		}, 180*time.Second, 300*time.Millisecond).Should(Succeed())
	})
})
