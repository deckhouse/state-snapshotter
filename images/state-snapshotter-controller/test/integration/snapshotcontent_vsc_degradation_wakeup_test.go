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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// Phase 2a damaged-artifact wake-up (data/VSC path). These specs require the VolumeSnapshotContent CRD
// (snapshot.storage.k8s.io/v1) to be served by envtest, which only happens when the sibling
// storage-foundation/crds are on the CRD path (see integrationResolveFoundationCRDDir). When the VSC
// API is unavailable the specs Skip with a clear reason; the same behavior is covered at unit level
// (controller_data_readiness_test.go) and is the live-e2e gate (P2a-E1).
var _ = Describe("Integration: VSC degradation wakes owning SnapshotContent", Serial, func() {
	var vscGVK = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"}

	skipUnlessVSCAvailable := func() {
		if _, err := mgr.GetRESTMapper().RESTMapping(vscGVK.GroupKind(), vscGVK.Version); err != nil {
			Skip("VolumeSnapshotContent CRD not served by envtest (storage-foundation/crds not on CRD path); covered by unit tests and live e2e")
		}
	}

	newVSC := func(name string, ownerRefs []metav1.OwnerReference) *unstructured.Unstructured {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(vscGVK)
		o.SetName(name)
		if ownerRefs != nil {
			o.SetOwnerReferences(ownerRefs)
		}
		// Minimal CRD-valid spec for a pre-provisioned VolumeSnapshotContent.
		Expect(unstructured.SetNestedMap(o.Object, map[string]interface{}{
			"deletionPolicy":    "Retain",
			"driver":            "test.csi.storage.k8s.io",
			"source":            map[string]interface{}{"snapshotHandle": "snap-handle-" + name},
			"volumeSnapshotRef": map[string]interface{}{"name": "vs-" + name, "namespace": "default"},
		}, "spec")).To(Succeed())
		return o
	}

	setVSCReadyToUse := func(ctx context.Context, name string, ready bool) {
		EventuallyWithOffset(1, func() error {
			o := &unstructured.Unstructured{}
			o.SetGroupVersionKind(vscGVK)
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, o); err != nil {
				return err
			}
			if err := unstructured.SetNestedField(o.Object, ready, "status", "readyToUse"); err != nil {
				return err
			}
			return k8sClient.Status().Update(ctx, o)
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
	}

	getVSCOwnerRefs := func(ctx context.Context, name string) []metav1.OwnerReference {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(vscGVK)
		ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKey{Name: name}, o)).To(Succeed())
		return o.GetOwnerReferences()
	}

	contentReadyCond := func(ctx context.Context, contentName string) *metav1.Condition {
		c := &storagev1alpha1.SnapshotContent{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, c); err != nil {
			return nil
		}
		return meta.FindStatusCondition(c.Status.Conditions, snapshot.ConditionReady)
	}

	// setupReadyDataLeaf creates a SnapshotContent leaf whose requests leg = Ready MCP + ready VSC,
	// waits for Ready=True, and returns (contentName, contentUID, mcpName, vscName).
	setupReadyDataLeaf := func(ctx context.Context, prefix string) (string, string, string, string) {
		content := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{GenerateName: prefix + "-content-"},
			Spec:       retainContentSpec(),
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

		mcpName := prefix + "-mcp-" + contentName
		vscName := prefix + "-vsc-" + contentName

		// Pre-provision a ready VSC, then publish truth refs (manifestCheckpointName + dataRefs) on the content.
		Expect(k8sClient.Create(ctx, newVSC(vscName, nil))).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, newVSC(vscName, nil)))
		})
		setVSCReadyToUse(ctx, vscName, true)

		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			c := &storagev1alpha1.SnapshotContent{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, c); err != nil {
				return err
			}
			c.Status.ManifestCheckpointName = mcpName
			c.Status.Data = &storagev1alpha1.SnapshotDataBinding{
				Source: storagev1alpha1.SnapshotSubjectRef{
					APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-1", Namespace: "default",
					UID: types.UID("pvc-uid-" + contentName),
				},
				Artifact: storagev1alpha1.SnapshotDataArtifactRef{
					APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: vscName,
				},
			}
			return k8sClient.Status().Update(ctx, c)
		})).To(Succeed())

		// MCP Ready=True (no chunks) so the requests leg proceeds to the data leg.
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

		Eventually(func(g Gomega) {
			ready := contentReadyCond(ctx, contentName)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal(snapshot.ReasonCompleted))
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed())

		return contentName, contentUID, mcpName, vscName
	}

	// P2a-I3: VSC readyToUse=false -> VSC watch wakes content -> DataCapturePending (non-terminal);
	// restoring readyToUse=true recovers the leaf to Ready=True.
	It("flips content Ready=False/DataCapturePending when the VSC is not readyToUse, then recovers", func() {
		skipUnlessVSCAvailable()
		ctx := context.Background()
		contentName, _, _, vscName := setupReadyDataLeaf(ctx, "vsc-pending")

		// Self-heal must have stamped the SnapshotContent ownerRef so the VSC watch can route events.
		Eventually(func(g Gomega) {
			var owned bool
			for _, ref := range getVSCOwnerRefs(ctx, vscName) {
				if ref.Kind == "SnapshotContent" && ref.Name == contentName {
					owned = true
				}
			}
			g.Expect(owned).To(BeTrue(), "self-heal should stamp SnapshotContent ownerRef on the VSC")
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		setVSCReadyToUse(ctx, vscName, false)
		Eventually(func(g Gomega) {
			ready := contentReadyCond(ctx, contentName)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal(snapshot.ReasonDataCapturePending))
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed())

		setVSCReadyToUse(ctx, vscName, true)
		Eventually(func(g Gomega) {
			ready := contentReadyCond(ctx, contentName)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal(snapshot.ReasonCompleted))
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed())
	})

	// P2a-I4: VSC deleted -> VSC watch wakes content -> ArtifactMissing (terminal requests failure).
	It("flips content Ready=False/ArtifactMissing when the referenced VSC is deleted", func() {
		skipUnlessVSCAvailable()
		ctx := context.Background()
		contentName, _, _, vscName := setupReadyDataLeaf(ctx, "vsc-deleted")

		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, newVSC(vscName, nil)))).To(Succeed())
		Eventually(func(g Gomega) {
			ready := contentReadyCond(ctx, contentName)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal(snapshot.ReasonArtifactMissing))
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed())
	})

	// P2a-I5: VSC ownerRef self-healing on reconcile. Strip the SnapshotContent ownerRef (keeping a
	// foreign non-controller ownerRef), trigger a content reconcile, and assert the controller ownerRef
	// is restored while the foreign ownerRef is preserved.
	It("restores the SnapshotContent ownerRef on the VSC and preserves foreign ownerRefs", func() {
		skipUnlessVSCAvailable()
		ctx := context.Background()
		contentName, _, _, vscName := setupReadyDataLeaf(ctx, "vsc-selfheal")

		foreign := metav1.OwnerReference{APIVersion: "example.com/v1", Kind: "Foo", Name: "foo-keeper", UID: types.UID("foo-uid")}

		// Replace the VSC ownerRefs with only the foreign one (drop the self-healed SnapshotContent ref).
		Eventually(func() error {
			o := &unstructured.Unstructured{}
			o.SetGroupVersionKind(vscGVK)
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: vscName}, o); err != nil {
				return err
			}
			o.SetOwnerReferences([]metav1.OwnerReference{foreign})
			return k8sClient.Update(ctx, o)
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		// Trigger a content reconcile (annotation bump) so self-heal re-asserts the ownerRef.
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			c := &storagev1alpha1.SnapshotContent{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, c); err != nil {
				return err
			}
			if c.Annotations == nil {
				c.Annotations = map[string]string{}
			}
			c.Annotations["test.state-snapshotter.deckhouse.io/bump"] = time.Now().Format(time.RFC3339Nano)
			return k8sClient.Update(ctx, c)
		})).To(Succeed())

		Eventually(func(g Gomega) {
			var foundContent, foundForeign bool
			for _, ref := range getVSCOwnerRefs(ctx, vscName) {
				if ref.Kind == "SnapshotContent" && ref.Name == contentName {
					foundContent = true
				}
				if ref.Kind == "Foo" && ref.Name == "foo-keeper" {
					foundForeign = true
				}
			}
			g.Expect(foundContent).To(BeTrue(), "self-heal must restore the SnapshotContent ownerRef")
			g.Expect(foundForeign).To(BeTrue(), "self-heal must preserve foreign non-controller ownerRefs")
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})
