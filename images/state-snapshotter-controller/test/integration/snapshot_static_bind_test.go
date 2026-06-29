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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

const (
	exportImportReadyTimeout = 120 * time.Second
	exportImportPoll         = 300 * time.Millisecond
)

// newStaticBindNamespace creates a uniquely-named namespace and schedules its cleanup.
func newStaticBindNamespace(ctx context.Context, prefix string) string {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	name := ns.Name
	DeferCleanup(func() {
		_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
	return name
}

// createStaticBindContent creates a cluster-scoped SnapshotContent whose spec.snapshotRef points back
// at (refName, ns), backed by a Ready, manifest-only ManifestCheckpoint so the SnapshotContent
// controller derives Ready=True (RequestsReady && ChildContentsReady). refName overrides the back-reference
// name to exercise the misbound case.
func createStaticBindContent(ctx context.Context, contentName, ns, refName string) {
	mcp := aggregatedManifestsIntegrationMustInstallReadyMCP(ctx, k8sClient, contentName+"-mcp", ns, []map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "sb-cm", "namespace": ns}},
	})

	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: contentName},
		Spec: storagev1alpha1.SnapshotContentSpec{
			DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "Snapshot",
				Name:       refName,
				Namespace:  ns,
			},
		},
	}
	Expect(k8sClient.Create(ctx, content)).To(Succeed())
	DeferCleanup(func() {
		bg := context.Background()
		// Cluster-scoped objects are not reclaimed by namespace deletion in envtest (no namespace GC
		// controller), so reclaim everything this helper created to avoid cross-spec accumulation.
		_ = k8sClient.Delete(bg, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: contentName}})
		_ = k8sClient.Delete(bg, &ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: mcp.Name}})
		_ = k8sClient.Delete(bg, &ssv1alpha1.ManifestCheckpointContentChunk{ObjectMeta: metav1.ObjectMeta{Name: mcp.Name + "-chunk-0"}})
	})

	// Re-fetch + patch under Eventually: the SnapshotContent controller watches creates and writes
	// status concurrently (a bare Status().Update on the create-time resourceVersion races to a 409),
	// and the cached reader may also lag the just-created object (NotFound). Eventually retries both.
	Eventually(func(g Gomega) {
		cur := &storagev1alpha1.SnapshotContent{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, cur)).To(Succeed())
		base := cur.DeepCopy()
		cur.Status.ManifestCheckpointName = mcp.Name
		g.Expect(k8sClient.Status().Patch(ctx, cur, client.MergeFrom(base))).To(Succeed())
	}).WithTimeout(30 * time.Second).WithPolling(exportImportPoll).Should(Succeed())
}

var _ = Describe("Integration: Snapshot static binding (pre-provisioning)", func() {
	It("binds a Snapshot to a pre-provisioned SnapshotContent and mirrors its Ready", func() {
		ctx := context.Background()
		ns := newStaticBindNamespace(ctx, "ss-staticbind-")
		const snapName = "sb-snap"
		contentName := "sb-content-" + ns

		// Pre-provisioned content exists before the Snapshot (back-ref UID left empty, as on import).
		createStaticBindContent(ctx, contentName, ns, snapName)

		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snapName, Namespace: ns},
			Spec: storagev1alpha1.SnapshotSpec{
				Source: &storagev1alpha1.SnapshotSource{SnapshotContentName: contentName},
			},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		Eventually(func(g Gomega) {
			f := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: snapName}, f)).To(Succeed())
			g.Expect(f.Status.BoundSnapshotContentName).To(Equal(contentName))
			ready := meta.FindStatusCondition(f.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())
	})

	It("fails terminally when the pre-provisioned content does not point back at the Snapshot", func() {
		ctx := context.Background()
		ns := newStaticBindNamespace(ctx, "ss-staticbind-mis-")
		const snapName = "sb-snap-mis"
		contentName := "sb-content-mis-" + ns

		// Content back-references a different snapshot name -> misbound (cross-binding guard).
		createStaticBindContent(ctx, contentName, ns, "some-other-snapshot")

		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snapName, Namespace: ns},
			Spec: storagev1alpha1.SnapshotSpec{
				Source: &storagev1alpha1.SnapshotSource{SnapshotContentName: contentName},
			},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		Eventually(func(g Gomega) {
			f := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: snapName}, f)).To(Succeed())
			ready := meta.FindStatusCondition(f.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal(snapshot.ReasonSnapshotContentMisbound))
			g.Expect(f.Status.BoundSnapshotContentName).To(BeEmpty())
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())
	})

	It("waits (non-terminally) when the referenced content does not exist yet", func() {
		ctx := context.Background()
		ns := newStaticBindNamespace(ctx, "ss-staticbind-missing-")
		const snapName = "sb-snap-missing"

		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: snapName, Namespace: ns},
			Spec: storagev1alpha1.SnapshotSpec{
				Source: &storagev1alpha1.SnapshotSource{SnapshotContentName: "not-created-yet-" + ns},
			},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		Eventually(func(g Gomega) {
			f := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: snapName}, f)).To(Succeed())
			ready := meta.FindStatusCondition(f.Status.Conditions, snapshot.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal(snapshot.ReasonSourceContentNotFound))
		}).WithTimeout(exportImportReadyTimeout).WithPolling(exportImportPoll).Should(Succeed())
	})
})
