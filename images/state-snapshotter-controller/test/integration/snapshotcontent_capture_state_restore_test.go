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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// A captured snapshot artifact is set-once, so the published pointer on the SnapshotContent — not a live
// capture request — is the source of truth for the owning snapshot's capture latch. When a typed writer
// with no extension fields replaces the owner's status, status.captureState is gone and a domain that
// reads the missing latch would create a second capture request against the live cluster. Core copies the
// latch back FROM the published pointer, and never publishes an intermediate false on the way: that
// window is exactly what a domain reads as "create a capture request".
//
// The same decisions are asserted against a fake client in the snapshotcontent unit tests. What only a
// real apiserver shows is here: the merge patch onto the status subresource of an unstructured domain
// kind, the uncached confirming read that separates a real restore from a patch the schema prunes, and
// the owner's version stream — evidence that no intermediate false was ever persisted, not merely that
// the end state is true.
//
// The owner's bound + Planned signals are a fixture here, because they are core's INPUT: this repository
// has no VolumeSnapshot controller, and the domain half of the recovery chain (a wiped domain object
// publishing Planned again with no capture request) is proven in the domain module and by the final e2e.
var _ = Describe("Integration: core restores a wiped capture latch from the published pointer", Serial, func() {
	var (
		ctx         context.Context
		ownerGVK    schema.GroupVersionKind
		contentGVK  schema.GroupVersionKind
		contentCtrl *controllers.SnapshotContentController
	)

	BeforeEach(func() {
		ctx = context.Background()
		ownerGVK = schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "TestSnapshot"}
		contentGVK = unifiedbootstrap.CommonSnapshotContentGVK()

		// A spec-local controller instance: the suite's running SnapshotContentController does not treat
		// TestSnapshot as a domain-capture kind, so marking it here keeps the capture-leg lifecycle under
		// this spec's control while every write still goes through the envtest apiserver.
		var err error
		contentCtrl, err = controllers.NewSnapshotContentController(
			k8sClient,
			mgr.GetAPIReader(),
			scheme,
			mgr.GetRESTMapper(),
			testCfg,
			[]schema.GroupVersionKind{contentGVK},
		)
		Expect(err).NotTo(HaveOccurred())
		contentCtrl.MarkDomainCaptureKind(ownerGVK)
		contentCtrl.Recorder = mgr.GetEventRecorderFor("integration-capture-state-restore")
	})

	It("copies manifestCaptured back from the published checkpoint without an intermediate false", func() {
		ns := captureRestoreNamespace(ctx)
		ownerName := "capture-latch-owner"
		owner := captureRestoreOwner(ctx, ownerGVK, ns, ownerName)
		contentName, mcpName := captureRestoreContentWithPointer(ctx, ownerGVK, ns, ownerName)

		// The wiped state a typed status replace leaves behind, plus the two signals core waits for. No
		// commonController: the latch the domain would read as "capture again".
		captureRestorePublishOwnerStatus(ctx, owner, contentName, storagev1alpha1.SnapshotCapturePhasePlanned)

		// Watch from the version the fixture just wrote, so every later version of the owner is observed.
		ownerVersions := captureRestoreWatchOwner(ctx, ownerGVK, ns, owner.GetResourceVersion())
		defer ownerVersions.Stop()

		Eventually(func(g Gomega) {
			_, err := contentCtrl.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: contentName}})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(captureRestoreLatch(ctx, ownerGVK, ns, ownerName)).To(Equal(captureLatchTrue))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed(), "core must copy manifestCaptured from the published checkpoint")

		// Every persisted version between the fixture and the restored latch: a false in that stream is
		// the recapture window, even though the end state is true.
		Expect(captureRestoreDrainUntilLatched(ownerVersions, ownerName)).NotTo(ContainElement(captureLatchFalse),
			"the restore must not publish an intermediate false")

		content := &storagev1alpha1.SnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, content)).To(Succeed())
		Expect(content.Status.ManifestCheckpointName).To(Equal(mcpName), "the published pointer must not move")

		Eventually(func(g Gomega) {
			events := &corev1.EventList{}
			g.Expect(k8sClient.List(ctx, events, client.InNamespace(ns))).To(Succeed())
			reasons := make([]string, 0, len(events.Items))
			for i := range events.Items {
				if events.Items[i].InvolvedObject.Name == ownerName {
					reasons = append(reasons, events.Items[i].Reason)
				}
			}
			g.Expect(reasons).To(ContainElement(snapshotcontent.EventReasonCaptureStateRestored))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed(), "a confirmed restore is announced on the owner")
	})

	It("leaves the latch alone until the owner is bound and past Planned", func() {
		for _, tc := range []struct {
			name  string
			bound func(contentName string) string
			phase storagev1alpha1.SnapshotCapturePhase
		}{
			{
				name:  "bound to another content",
				bound: func(string) string { return "some-other-content" },
				phase: storagev1alpha1.SnapshotCapturePhasePlanned,
			},
			{
				name:  "before Planned",
				bound: func(contentName string) string { return contentName },
				phase: storagev1alpha1.SnapshotCapturePhasePlanning,
			},
		} {
			By(tc.name)
			ns := captureRestoreNamespace(ctx)
			ownerName := "capture-latch-owner"
			owner := captureRestoreOwner(ctx, ownerGVK, ns, ownerName)
			contentName, _ := captureRestoreContentWithPointer(ctx, ownerGVK, ns, ownerName)
			captureRestorePublishOwnerStatus(ctx, owner, tc.bound(contentName), tc.phase)

			Consistently(func(g Gomega) {
				_, err := contentCtrl.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: contentName}})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(captureRestoreLatch(ctx, ownerGVK, ns, ownerName)).To(Equal(captureLatchAbsent))
			}, 3*time.Second, 200*time.Millisecond).Should(Succeed(), "neither the restore nor the eager-init may run before both signals are on the owner")
		}
	})
})

// The three states of the owner's manifestCaptured latch as a spec reads them: a missing field is the
// wiped state, and it is a different fact from a published false.
const (
	captureLatchAbsent = "absent"
	captureLatchFalse  = "false"
	captureLatchTrue   = "true"
)

func captureRestoreNamespace(ctx context.Context) string {
	GinkgoHelper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "capture-latch-"}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	DeferCleanup(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns.Name}}))
	})
	return ns.Name
}

// captureRestoreOwner creates the domain snapshot that owns the content, with an empty status: the object
// a typed writer without extension fields leaves behind.
func captureRestoreOwner(ctx context.Context, ownerGVK schema.GroupVersionKind, ns, name string) *unstructured.Unstructured {
	GinkgoHelper()
	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(ownerGVK)
	owner.SetNamespace(ns)
	owner.SetName(name)
	owner.Object["spec"] = map[string]interface{}{}
	Expect(k8sClient.Create(ctx, owner)).To(Succeed())
	return owner
}

// captureRestoreContentWithPointer creates the SnapshotContent owned by ns/ownerName, publishes its
// durable manifestCheckpointName, and backs that name with a Ready ManifestCheckpoint owned by the
// content — the completed handoff whose pointer the latch is copied from. It returns the content name and
// the published checkpoint name.
func captureRestoreContentWithPointer(ctx context.Context, ownerGVK schema.GroupVersionKind, ns, ownerName string) (string, string) {
	GinkgoHelper()
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "capture-latch-content-"},
		Spec: storagev1alpha1.SnapshotContentSpec{
			DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: ownerGVK.GroupVersion().String(),
				Kind:       ownerGVK.Kind,
				Namespace:  ns,
				Name:       ownerName,
			},
		},
	}
	Expect(k8sClient.Create(ctx, content)).To(Succeed())
	contentName := content.Name
	DeferCleanup(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: contentName}}))
	})

	var contentUID string
	Eventually(func(g Gomega) {
		c := &storagev1alpha1.SnapshotContent{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, c)).To(Succeed())
		contentUID = string(c.UID)
		g.Expect(contentUID).NotTo(BeEmpty())
	}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

	mcpName := "capture-latch-mcp-" + contentName
	ensureManifestCheckpointStatus(ctx, mcpName, contentName, contentUID, func(m *ssv1alpha1.ManifestCheckpoint) {
		meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
			Type:    ssv1alpha1.ManifestCheckpointConditionTypeReady,
			Status:  metav1.ConditionTrue,
			Reason:  ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
			Message: "checkpoint ready",
		})
	})
	DeferCleanup(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: mcpName}}))
	})

	Eventually(func() error {
		c := &storagev1alpha1.SnapshotContent{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, c); err != nil {
			return err
		}
		c.Status.ManifestCheckpointName = mcpName
		return k8sClient.Status().Update(ctx, c)
	}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

	return contentName, mcpName
}

// captureRestorePublishOwnerStatus writes the owner status core waits for — the bound content name and
// the domain phase — and nothing else. captureState carries no commonController, so the latch stays in
// its wiped state until core restores it. obj is refreshed to the published version.
func captureRestorePublishOwnerStatus(ctx context.Context, obj *unstructured.Unstructured, boundContentName string, phase storagev1alpha1.SnapshotCapturePhase) {
	GinkgoHelper()
	Eventually(func() error {
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), fresh); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(fresh.Object, boundContentName, "status", "boundSnapshotContentName"); err != nil {
			return err
		}
		setDomainPhase(fresh, phase)
		if err := k8sClient.Status().Update(ctx, fresh); err != nil {
			return err
		}
		obj.Object = fresh.Object
		return nil
	}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
}

func captureRestoreLatch(ctx context.Context, ownerGVK schema.GroupVersionKind, ns, name string) string {
	GinkgoHelper()
	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(ownerGVK)
	Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, fresh)).To(Succeed())
	return captureRestoreLatchOf(fresh)
}

func captureRestoreLatchOf(obj *unstructured.Unstructured) string {
	value, found, err := unstructured.NestedBool(obj.Object, "status", "captureState", "commonController", "manifestCaptured")
	if err != nil || !found {
		return captureLatchAbsent
	}
	if value {
		return captureLatchTrue
	}
	return captureLatchFalse
}

// captureRestoreWatchOwner opens a watch on the owner kind starting right after resourceVersion, so the
// spec sees every version the controller persists from that point on.
func captureRestoreWatchOwner(ctx context.Context, ownerGVK schema.GroupVersionKind, ns, resourceVersion string) watch.Interface {
	GinkgoHelper()
	watchClient, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ownerGVK.GroupVersion().WithKind(ownerGVK.Kind + "List"))
	w, err := watchClient.Watch(ctx, list, client.InNamespace(ns), &client.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: resourceVersion}})
	Expect(err).NotTo(HaveOccurred())
	return w
}

// captureRestoreDrainUntilLatched returns the latch value of every observed version of the owner up to
// and including the one that carries the restored true.
func captureRestoreDrainUntilLatched(w watch.Interface, ownerName string) []string {
	GinkgoHelper()
	observed := make([]string, 0, 4)
	deadline := time.After(30 * time.Second)
	for {
		select {
		case event, ok := <-w.ResultChan():
			Expect(ok).To(BeTrue(), "the owner watch closed before the restored latch was observed")
			obj, ok := event.Object.(*unstructured.Unstructured)
			if !ok || obj.GetName() != ownerName {
				continue
			}
			latch := captureRestoreLatchOf(obj)
			if latch == captureLatchAbsent {
				continue
			}
			observed = append(observed, latch)
			if latch == captureLatchTrue {
				return observed
			}
		case <-deadline:
			Fail(fmt.Sprintf("the restored latch never appeared on the owner watch, observed %v", observed))
		}
	}
}
