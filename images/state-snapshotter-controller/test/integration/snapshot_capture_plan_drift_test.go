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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// A namespace snapshot is point-in-time. Once the root ManifestCaptureRequest exists its plan is frozen:
// the controller (MCR-gate in reconcileCaptureN2a) must NOT re-list the live namespace, must NOT rewrite
// spec.targets, and must NEVER set Ready=False/CapturePlanDrift even when the namespace changes after the
// MCR is fixed. This replaces the previous continuous drift-detection behavior (now removed).
var _ = Describe("Integration: Snapshot frozen capture plan (N2a, point-in-time)", func() {
	It("keeps a pre-existing root MCR plan frozen and never sets CapturePlanDrift when the namespace changes", func() {
		ctx := context.Background()
		contentName := ""

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-drift-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "snapshot-frozen-capture-plan",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			if contentName != "" {
				_ = k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: contentName}})
			}
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm1 := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-drift-cm1", Namespace: nsName},
			Data:       map[string]string{"k": "v1"},
		}
		Expect(k8sClient.Create(ctx, cm1)).To(Succeed())

		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "snap",
				Namespace: nsName,
			},
			Spec: storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		key := types.NamespacedName{Namespace: nsName, Name: snap.Name}
		contentName = snapshot.GenerateSnapshotContentName(snap.Name, string(snap.UID))

		mcrKey := client.ObjectKey{Namespace: nsName, Name: namespacemanifest.SnapshotMCRName(snap.UID)}
		controller := true
		mcr := &ssv1alpha1.ManifestCaptureRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      mcrKey.Name,
				Namespace: mcrKey.Namespace,
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/snapshot-uid": string(snap.UID),
				},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
					Kind:       "Snapshot",
					Name:       snap.Name,
					UID:        snap.UID,
					Controller: &controller,
				}},
			},
			Spec: ssv1alpha1.ManifestCaptureRequestSpec{
				Targets: []ssv1alpha1.ManifestTarget{{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       cm1.Name,
				}},
			},
		}
		Expect(k8sClient.Create(ctx, mcr)).To(Succeed())

		// Change the namespace AFTER the MCR plan is fixed: with point-in-time semantics this must NOT cause
		// drift, a re-list, or a spec.targets rewrite (cm2 must never enter the frozen plan).
		cm2 := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-drift-cm2", Namespace: nsName},
			Data:       map[string]string{"k": "v2"},
		}
		Expect(k8sClient.Create(ctx, cm2)).To(Succeed())
		// Wait until root binding exists with an already-stale frozen MCR plan.
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			g.Expect(fresh.Status.BoundSnapshotContentName).To(Equal(contentName))
		}).WithTimeout(30 * time.Second).WithPolling(50 * time.Millisecond).Should(Succeed())

		rootForMCR := &storagev1alpha1.Snapshot{}
		Expect(k8sClient.Get(ctx, key, rootForMCR)).To(Succeed())
		Expect(mcrOwnerRefToSnapshot(mcr.OwnerReferences, rootForMCR.Name, rootForMCR.UID)).To(BeTrue(), "MCR must be owned by root Snapshot for in-flight GC")

		// Kick the reconcile so the controller re-evaluates against the changed namespace.
		snapFresh := &storagev1alpha1.Snapshot{}
		Expect(k8sClient.Get(ctx, key, snapFresh)).To(Succeed())
		base := snapFresh.DeepCopy()
		if snapFresh.Annotations == nil {
			snapFresh.Annotations = map[string]string{}
		}
		snapFresh.Annotations["state-snapshotter.deckhouse.io/integration-frozen-plan-kick"] = fmt.Sprintf("%d", time.Now().UnixNano())
		Expect(k8sClient.Patch(ctx, snapFresh, client.MergeFrom(base))).To(Succeed())

		// Invariant: the root never enters CapturePlanDrift, and while the MCR exists its spec.targets stay
		// frozen to the original plan (cm1 only, never cm2). A NotFound MCR is acceptable: once capture
		// completes against the frozen plan the controller cleans it up (and must not recreate it).
		Consistently(func(g Gomega) {
			root := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, key, root)).To(Succeed())
			if ready := meta.FindStatusCondition(root.Status.Conditions, snapshot.ConditionReady); ready != nil {
				g.Expect(ready.Reason).NotTo(Equal("CapturePlanDrift"), "point-in-time snapshot must never drift on live namespace change")
			}

			mcrNow := &ssv1alpha1.ManifestCaptureRequest{}
			err := k8sClient.Get(ctx, mcrKey, mcrNow)
			if apierrors.IsNotFound(err) {
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			names := make([]string, 0, len(mcrNow.Spec.Targets))
			for _, t := range mcrNow.Spec.Targets {
				names = append(names, t.Name)
			}
			g.Expect(names).To(ConsistOf(cm1.Name), "frozen plan must not be rewritten with new namespace objects (cm2)")
		}).WithTimeout(6 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})
})

// A namespace snapshot's child set is frozen at the point-in-time barrier (phase>=Planned) — the ADR PIT
// cycle: "если узел уже Planned — план заморожен, состав не пересчитывается". reconcileNamespaceCapture
// therefore skips its ENTIRE plan+enumerate+freeze block (steps 1-5: PublishSnapshotSource,
// planNamespaceChildren, EnsureChildren, the orphan-PVC wave, and MarkPlanned) once the domain is past
// Planned — they all share the single namespaceDomainPrePlanned gate, so a guard on any one of them guards
// the whole block. This is the CHILDREN-axis mirror of the manifest-axis frozen-plan spec above.
//
// The residual-PVC case below is the decisive regression signal: the orphan-PVC wave is the one gated step
// whose post-Planned execution has an observable, crisp side effect in envtest (a bare PVC with no
// storageClassName fails orphan class resolution and would flip the root Ready=False). A CSD-mapped domain
// source (planNamespaceChildren enumeration) has no ready-made auto-enumeration fixture in this harness, but
// it lives inside the same gate, so this spec transitively guards it too.
//
// The complementary "a declared child deleted after Planned is NOT recreated and instead surfaces as
// terminal ChildSnapshotLost" interaction (this skip removes the old accidental re-plan self-heal, Block E
// keeps the failure loud) is covered deterministically at the unit level — the owner-mirror fold in
// snapshotcontent/lost_children_test.go (TestMirrorReadyToOwnerSnapshot_LostChildrenFold) and the pre-Planned
// planner gate in snapshot/namespace_children_plan_test.go — because the exact Lost-vs-Deleted outcome is
// child-content-Ready-timing dependent and not deterministically reproducible under a live MCP controller.
var _ = Describe("Integration: Snapshot frozen child set (point-in-time, ns-replan-skip)", func() {
	It("does not re-plan the child graph after Planned: a residual PVC added post-Planned is never enumerated and the root stays Ready", func() {
		ctx := context.Background()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-replan-skip-",
				Labels: map[string]string{
					"state-snapshotter.deckhouse.io/test": "snapshot-replan-skip",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
		})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "replan-skip-cm", Namespace: nsName},
			Data:       map[string]string{"k": "v"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: nsName},
			Spec:       storagev1alpha1.SnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		key := types.NamespacedName{Namespace: nsName, Name: snap.Name}

		// Reach the point-in-time barrier: the root binds and captures to Ready=True/Completed (so the domain
		// phase is >=Planned and the child set is frozen). No children exist yet — a childless namespace root
		// captures only the ConfigMap manifest.
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal(snapshot.ReasonCompleted))
			g.Expect(fresh.Status.ChildrenSnapshotRefs).To(BeEmpty())
		}).WithTimeout(120 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		// Drift the namespace AFTER Planned on the CHILDREN axis: a loose (residual) PVC is exactly what the
		// pre-barrier orphan-PVC wave (step 4 of reconcileNamespaceCapture) enumerates into a new
		// VolumeSnapshot child. With the PIT re-plan skip the wave never runs post-Planned, so the PVC is
		// ignored. Were the skip removed, the wave would run: this PVC has no storageClassName, so orphan
		// class resolution fails closed and would flip the root Ready=False — a crisp regression signal.
		latePVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "replan-skip-late-pvc", Namespace: nsName},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, latePVC)).To(Succeed())

		// The Snapshot controller does not watch PVCs, so kick a reconcile to force at least one post-Planned
		// re-evaluation against the changed namespace.
		pr7KickSnapshot(ctx, key)

		// Invariant: post-Planned the child graph is frozen — the late PVC is never enumerated
		// (childrenSnapshotRefs stays empty, no VolumeSnapshot child is created) and the root stays
		// Ready=True/Completed.
		Consistently(func(g Gomega) {
			fresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.Status.ChildrenSnapshotRefs).To(BeEmpty(), "point-in-time snapshot must not enumerate a residual PVC added after Planned")
			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue), "root must stay Ready after a post-Planned namespace change")
			g.Expect(ready.Reason).To(Equal(snapshot.ReasonCompleted))

			vsList := &unstructured.UnstructuredList{}
			vsList.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotList"})
			g.Expect(k8sClient.List(ctx, vsList, client.InNamespace(nsName))).To(Succeed())
			g.Expect(vsList.Items).To(BeEmpty(), "orphan wave must not run post-Planned; no VolumeSnapshot child for the late PVC")
		}).WithTimeout(6 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	})
})
