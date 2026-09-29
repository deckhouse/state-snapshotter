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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// The whole recovery chain, driven by live controllers on a snapshot that was really captured: a writer
// replaces the snapshot's status with a model that has no captureState, and what has to happen next is
// nothing — no second capture. The chain has three links and each one belongs to a different layer:
//
//  1. the domain publishes its planning phase again (here the namespace domain, which lives in this
//     repository; for the CSI VolumeSnapshot kind it is the storage-foundation controller and its own
//     guard);
//  2. the core copies the leg latch back FROM the published checkpoint — never re-derived from a live
//     capture request — because the snapshot is still bound to the content that owns the artifact;
//  3. the domain SDK, reading the three-valued latch, creates no request while the leg is undeclared and
//     none once it is restored as captured.
//
// A single assertion pins all three: the pointer still names the checkpoint the data was taken with, and
// no second checkpoint was ever produced. Before this chain existed, a wiped captureState was read as "not
// captured yet", a new ManifestCaptureRequest ran against the live cluster, and the pointer moved onto a
// checkpoint of a namespace that had since changed.
var _ = Describe("Integration: a wiped capture state is restored from the published artifact", Serial, func() {
	It("keeps the checkpoint the snapshot was taken with and produces no second one", func() {
		ctx := context.Background()

		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nss-wipe-recover-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		contentName := ""
		DeferCleanup(func() {
			cctx := context.Background()
			if contentName != "" {
				_ = client.IgnoreNotFound(k8sClient.Delete(cctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: contentName}}))
			}
			_ = client.IgnoreNotFound(k8sClient.Delete(cctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}))
		})

		Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "nss-wipe-cm", Namespace: nsName},
			Data:       map[string]string{"k": "v"},
		})).To(Succeed())

		snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: nsName, Name: "snap"}}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		snapKey := types.NamespacedName{Namespace: nsName, Name: snap.Name}

		// A real capture, all the way to the durable artifact: the content names a checkpoint and the core
		// has latched the manifest leg on the snapshot.
		var capturedMCP string
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, snapKey, fresh)).To(Succeed())
			g.Expect(fresh.Status.BoundSnapshotContentName).NotTo(BeEmpty())
			contentName = fresh.Status.BoundSnapshotContentName
			g.Expect(fresh.Status.CaptureState).NotTo(BeNil())
			g.Expect(fresh.Status.CaptureState.CommonController).NotTo(BeNil())
			g.Expect(fresh.Status.CaptureState.CommonController.ManifestCaptured).NotTo(BeNil())
			g.Expect(*fresh.Status.CaptureState.CommonController.ManifestCaptured).To(BeTrue())

			content := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, content)).To(Succeed())
			capturedMCP = content.Status.ManifestCheckpointName
			g.Expect(capturedMCP).NotTo(BeEmpty())
		}, 180*time.Second, 300*time.Millisecond).Should(Succeed(), "the snapshot must reach a durable checkpoint before its status is damaged")

		// The concrete value, not "the field is non-empty": the assertion after the wipe is that the pointer
		// still names THIS checkpoint, which is the one the captured data belongs with.
		checkpointsOwnedByContent := func(g Gomega) []string {
			list := &ssv1alpha1.ManifestCheckpointList{}
			g.Expect(k8sClient.List(ctx, list)).To(Succeed())
			owned := make([]string, 0, len(list.Items))
			for i := range list.Items {
				for _, ref := range list.Items[i].OwnerReferences {
					if ref.Kind == "SnapshotContent" && ref.Name == contentName {
						owned = append(owned, list.Items[i].Name)
					}
				}
			}
			return owned
		}
		Eventually(func(g Gomega) {
			g.Expect(checkpointsOwnedByContent(g)).To(ConsistOf(capturedMCP))
		}, 60*time.Second, 300*time.Millisecond).Should(Succeed())

		// The damage: a writer whose model has no captureState replaces the status. Everything else it knows
		// about survives, which is what makes the leg latch read as "nothing declared" rather than "false".
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			fresh := &storagev1alpha1.Snapshot{}
			if err := k8sClient.Get(ctx, snapKey, fresh); err != nil {
				return err
			}
			fresh.Status.CaptureState = nil
			return k8sClient.Status().Update(ctx, fresh)
		})).To(Succeed())

		// Link 1 + link 2: the domain re-publishes its phase, and the core copies the latch back from the
		// published checkpoint.
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, snapKey, fresh)).To(Succeed())
			g.Expect(fresh.Status.CaptureState).NotTo(BeNil())
			g.Expect(fresh.Status.CaptureState.CommonController).NotTo(BeNil())
			g.Expect(fresh.Status.CaptureState.CommonController.ManifestCaptured).NotTo(BeNil())
			g.Expect(*fresh.Status.CaptureState.CommonController.ManifestCaptured).To(BeTrue(),
				"the latch must come back as captured, copied from the published checkpoint")
		}, 120*time.Second, 300*time.Millisecond).Should(Succeed())

		// The core announces a restore it has confirmed on the API server.
		Eventually(func(g Gomega) {
			events := &corev1.EventList{}
			g.Expect(k8sClient.List(ctx, events, client.InNamespace(nsName))).To(Succeed())
			reasons := make([]string, 0, len(events.Items))
			for i := range events.Items {
				if events.Items[i].InvolvedObject.Name == snap.Name {
					reasons = append(reasons, events.Items[i].Reason)
				}
			}
			g.Expect(reasons).To(ContainElement(snapshotcontent.EventReasonCaptureStateRestored))
		}, 60*time.Second, 300*time.Millisecond).Should(Succeed())

		// Link 3, and the invariant the whole plan is about: the artifact did not move and no second capture
		// ran. The deterministic request name is checked too — a recapture would have created it again.
		mcrKey := client.ObjectKey{Namespace: nsName, Name: namespacemanifest.SnapshotMCRName(snapshotUIDFor(ctx, snapKey))}
		Consistently(func(g Gomega) {
			content := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, content)).To(Succeed())
			g.Expect(content.Status.ManifestCheckpointName).To(Equal(capturedMCP), "the published artifact must not move")
			g.Expect(checkpointsOwnedByContent(g)).To(ConsistOf(capturedMCP), "no second checkpoint may be produced")
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, mcrKey, &ssv1alpha1.ManifestCaptureRequest{}))).To(BeTrue(),
				"no capture request may be re-created for an already captured snapshot")
		}, 8*time.Second, 500*time.Millisecond).Should(Succeed())

		// And the snapshot is still the Ready snapshot it was: the recovery is not a downgrade.
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, snapKey, fresh)).To(Succeed())
			ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		}, 60*time.Second, 300*time.Millisecond).Should(Succeed())
	})
})

func snapshotUIDFor(ctx context.Context, key types.NamespacedName) types.UID {
	GinkgoHelper()
	fresh := &storagev1alpha1.Snapshot{}
	Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
	return fresh.UID
}
