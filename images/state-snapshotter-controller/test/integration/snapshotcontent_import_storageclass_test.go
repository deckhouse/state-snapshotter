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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// The aggregator is the single writer of SnapshotContent.status.data, and on the import leg it is the only
// component that knows the volume metadata the imported bytes were staged with: the StorageClass
// (DataImport.spec.storageParams.storageClassName), the volumeMode of the scratch volume
// (status.volumeMode) and the filesystem it was actually formatted with (status.data.fsType). None of the
// three survives the import — the scratch volume is destroyed right after capture, the durable
// VolumeSnapshotContent records neither mode nor filesystem, and the DataImport itself is reaped by its idle
// TTL — so this projection is what makes them durable.
//
// This spec drives that projection against a REAL apiserver: the DataImport is a real served object (so the
// paths are read off a CRD-validated resource, not a hand-built map — a pruned field yields the empty value
// exactly as it would in production) and the SnapshotContent is the production CRD (so a value that does not
// fit its status.data schema is rejected instead of silently accepted, which a fake client cannot catch).
//
// Only the CONTENT half runs here. The leaf mirror is a verbatim copy of content.status.data with no logic
// of its own after this change, and driving the generic binder's import path in envtest would need the whole
// uploaded-payload chain (parent snapshot + parent content + reconstructed ManifestCheckpoint); it stays
// covered by the binder unit tests (genericbinder, volumesnapshotimport) and by the e2e round-trip, which
// asserts the class on the imported leaf itself.
//
// Label("isolated"): this spec installs the cluster-scoped VolumeSnapshotContent CRD (the enricher and the
// Retain/ownerRef handoff read the produced artifact), which the shared !isolated pass deliberately omits.
var _ = Describe("Integration: import data leg publishes the DataImport volume metadata", Serial, Ordered, Label("isolated"), func() {
	const (
		importSnapshotKind = "TestSnapshot"
		importSnapshotAPI  = "test.deckhouse.io/v1alpha1"
		importScratchClass = "sc-import-scratch"
		// The filesystem storage-foundation observed on the scratch PersistentVolume. It is deliberately not
		// the cluster default for anything, so a value that appears here can only have come from the DataImport.
		importObservedFs = "ext4"
	)

	var (
		testSnapshotGVK = schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: importSnapshotKind}
		vscGVK          = schema.GroupVersionKind{Group: snapshot.CSISnapshotGroup, Version: snapshot.CSISnapshotVersion, Kind: snapshot.KindVolumeSnapshotContent}
	)

	BeforeAll(func() {
		ctx := context.Background()
		pr7InstallCSIClassAndContentCRDs(ctx)
		integrationEnsureDataImportCRD(ctx, cfg)
	})

	// newImportContentController builds a SnapshotContentController that treats the test snapshot kind as
	// data-bearing (in production the CSD sets requiresDataArtifact). It is reconciled directly instead of
	// being registered with the manager: the suite manager already runs a content controller for this GVK,
	// and a second registered one would double-reconcile every content in the suite.
	newImportContentController := func() *controllers.SnapshotContentController {
		GinkgoHelper()
		cc, err := controllers.NewSnapshotContentController(
			mgr.GetClient(), mgr.GetAPIReader(), scheme, mgr.GetRESTMapper(), testCfg,
			[]schema.GroupVersionKind{unifiedbootstrap.CommonSnapshotContentGVK()},
		)
		Expect(err).NotTo(HaveOccurred())
		cc.GVKRegistry.MarkRequiresDataArtifact(importSnapshotKind, true)
		return cc
	}

	It("projects the DataImport StorageClass, volumeMode and fsType onto the content, and heals a content published without them", func() {
		ctx := context.Background()

		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ss-import-sc-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns := nsObj.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})

		By("creating the import-mode data leaf")
		leaf := &unstructured.Unstructured{}
		leaf.SetGroupVersionKind(testSnapshotGVK)
		leaf.SetNamespace(ns)
		leaf.SetName("import-leaf")
		Expect(unstructured.SetNestedField(leaf.Object, string(storagev1alpha1.SnapshotModeImport), "spec", "mode")).To(Succeed())
		Expect(k8sClient.Create(ctx, leaf)).To(Succeed())
		// Guard the fixture itself: a pruned spec.mode would silently turn this into a capture leaf and the
		// whole spec would assert nothing. k8sClient is the manager's CACHED client, so read back under
		// Eventually — a just-created object is not in the informer cache yet.
		var leafUID types.UID
		Eventually(func(g Gomega) {
			fresh := &unstructured.Unstructured{}
			fresh.SetGroupVersionKind(testSnapshotGVK)
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: leaf.GetName()}, fresh)).To(Succeed())
			mode, _, _ := unstructured.NestedString(fresh.Object, "spec", "mode")
			g.Expect(mode).To(Equal(string(storagev1alpha1.SnapshotModeImport)), "spec.mode must survive the CRD schema")
			leafUID = fresh.GetUID()
			g.Expect(leafUID).NotTo(BeEmpty())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("creating the VolumeSnapshotContent the import produced")
		vscName := "import-sc-vsc-" + ns
		vsc := &unstructured.Unstructured{}
		vsc.SetGroupVersionKind(vscGVK)
		vsc.SetName(vscName)
		Expect(unstructured.SetNestedMap(vsc.Object, map[string]interface{}{
			"deletionPolicy":    "Delete",
			"driver":            "test.csi.storage.k8s.io",
			"source":            map[string]interface{}{"snapshotHandle": "handle-" + vscName},
			"volumeSnapshotRef": map[string]interface{}{"name": "vs-" + vscName, "namespace": ns},
		}, "spec")).To(Succeed())
		Expect(k8sClient.Create(ctx, vsc)).To(Succeed())
		DeferCleanup(func() {
			gone := &unstructured.Unstructured{}
			gone.SetGroupVersionKind(vscGVK)
			gone.SetName(vscName)
			_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), gone))
		})
		vscHasStatusSubresource := crdServesStatusSubresource(ctx, "volumesnapshotcontents."+snapshot.CSISnapshotGroup, snapshot.CSISnapshotVersion)
		var vscUID types.UID
		Eventually(func() error {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(vscGVK)
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: vscName}, live); err != nil {
				return err
			}
			vscUID = live.GetUID()
			if err := unstructured.SetNestedField(live.Object, true, "status", "readyToUse"); err != nil {
				return err
			}
			if err := unstructured.SetNestedField(live.Object, int64(524288000), "status", "restoreSize"); err != nil {
				return err
			}
			if vscHasStatusSubresource {
				return k8sClient.Status().Update(ctx, live)
			}
			return k8sClient.Update(ctx, live)
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("creating the PopulateData DataImport that staged the bytes")
		di := &unstructured.Unstructured{}
		di.SetGroupVersionKind(dataImportGVKForTest)
		di.SetNamespace(ns)
		di.SetName("import-sc-di")
		Expect(unstructured.SetNestedMap(di.Object, map[string]interface{}{
			"ttl":                  "24h",
			"waitForFirstConsumer": false,
			"mode":                 snapshot.DataImportModePopulateData,
			"snapshotRef": map[string]interface{}{
				"apiVersion": importSnapshotAPI, "kind": importSnapshotKind, "name": leaf.GetName(),
			},
			"storageParams": map[string]interface{}{
				"storageClassName": importScratchClass,
				"size":             "10Gi",
				"volumeMode":       string(corev1.PersistentVolumeFilesystem),
			},
		}, "spec")).To(Succeed())
		Expect(k8sClient.Create(ctx, di)).To(Succeed())
		Eventually(func() error {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(dataImportGVKForTest)
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: di.GetName()}, live); err != nil {
				return err
			}
			// The artifact uid matters: the enricher backfills it from the live VolumeSnapshotContent when
			// the producer left it empty, and the fast-path latch compares the whole artifactRef. Publishing
			// it here (as a real DataImport does) is what lets the latch actually engage, so the self-heal
			// assertion below tests the storageClassName term instead of an unrelated uid mismatch.
			if err := unstructured.SetNestedMap(live.Object, map[string]interface{}{
				"apiVersion": snapshot.CSISnapshotAPIVersion,
				"kind":       snapshot.KindVolumeSnapshotContent,
				"name":       vscName,
				"uid":        string(vscUID),
			}, "status", "data", "artifactRef"); err != nil {
				return err
			}
			if err := unstructured.SetNestedField(live.Object, string(corev1.PersistentVolumeFilesystem), "status", "volumeMode"); err != nil {
				return err
			}
			// The filesystem observed on the scratch volume while it still existed. state-snapshotter cannot
			// observe it at all (it joins the import only once the artifact exists, by which time the volume is
			// gone), so a DataImport that publishes it is the ONLY way it can reach the content.
			if err := unstructured.SetNestedField(live.Object, importObservedFs, "status", "data", "fsType"); err != nil {
				return err
			}
			return k8sClient.Status().Update(ctx, live)
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("creating the leaf's SnapshotContent")
		content := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "import-sc-content-"},
			Spec: storagev1alpha1.SnapshotContentSpec{
				DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyDelete,
				SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
					APIVersion: importSnapshotAPI,
					Kind:       importSnapshotKind,
					Name:       leaf.GetName(),
					Namespace:  ns,
					UID:        leafUID,
				},
			},
		}
		Expect(k8sClient.Create(ctx, content)).To(Succeed())
		contentName := content.Name
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(),
				&storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: contentName}}))
		})

		cc := newImportContentController()
		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: contentName}}
		contentData := func(g Gomega) storagev1alpha1.SnapshotDataBinding {
			live := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, live)).To(Succeed())
			g.Expect(live.Status.Data).NotTo(BeNil(), "the aggregator must publish status.data for an import data leaf")
			return *live.Status.Data
		}
		expectImportMetadata := func(g Gomega) {
			d := contentData(g)
			g.Expect(d.StorageClassName).To(Equal(importScratchClass))
			g.Expect(d.VolumeMode).To(Equal(string(corev1.PersistentVolumeFilesystem)))
			g.Expect(d.FsType).To(Equal(importObservedFs))
		}

		By("reconciling until the aggregator publishes the import StorageClass, volumeMode and fsType")
		Eventually(func(g Gomega) {
			_, err := cc.Reconcile(ctx, req)
			g.Expect(err).NotTo(HaveOccurred())
			expectImportMetadata(g)
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed())

		By("rewinding status.data to its pre-fix shape (no storageClassName, no fsType)")
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			live := &storagev1alpha1.SnapshotContent{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, live); err != nil {
				return err
			}
			live.Status.Data.StorageClassName = ""
			live.Status.Data.FsType = ""
			return k8sClient.Status().Update(ctx, live)
		})).To(Succeed())
		Eventually(func(g Gomega) {
			d := contentData(g)
			g.Expect(d.StorageClassName).To(BeEmpty())
			g.Expect(d.FsType).To(BeEmpty())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		// A content that predates the fix carries a status.data the projection otherwise considers complete
		// (same artifactRef, same volumeMode) — and both the class and the filesystem are unrecoverable once the
		// DataImport is reaped, so it must catch up rather than stay empty. This asserts the OUTCOME end to end;
		// that the fast-path latch is what has to yield for it (its per-field terms) is pinned at unit level,
		// where the latch can be observed directly.
		By("reconciling again: a content published without the class and filesystem must catch up")
		Eventually(func(g Gomega) {
			_, err := cc.Reconcile(ctx, req)
			g.Expect(err).NotTo(HaveOccurred())
			expectImportMetadata(g)
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})
