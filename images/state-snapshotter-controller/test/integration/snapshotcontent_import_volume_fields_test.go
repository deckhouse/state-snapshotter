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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// A NATIVE-CSI import leaf is a kind VolumeSnapshot, so its data leg is projected by the branch that capture
// and import SHARE (the owner is bound to its VolumeSnapshotContent, not to a VolumeCaptureRequest) — it does
// not take the DataImport branch at all. That is why this spec exists next to the generic-import one: the
// shared branch builds its binding locally from {sourceRef, artifactRef} and has to pull the import-only
// volume metadata in by hand, and getting only half of that in (a latch term without the publish) leaves the
// leg re-publishing on every pass with the field still missing.
//
// Against a real apiserver it also covers what a fake client cannot: the imported leaf's status.sourceRef
// names a PVC that exists only in the checkpoint, so this spec puts a REAL, live PVC of that very name in the
// namespace. The apiserver assigns it its own UID — necessarily different from the captured one — which is
// exactly the production hazard, because the volume-metadata enricher looks a source PVC up BY NAME. A
// stranger's volumeMode published as the imported volume's restores a Block source as a filesystem and serves
// garbage.
//
// The flow is driven by the suite manager's own controllers (no hand-built content, no hand-driven Reconcile):
// the runtime creates and binds the leaf's SnapshotContent itself, and a second content for the same owner
// would be a shape production never produces.
//
// Label("isolated"): it installs the cluster-scoped VolumeSnapshotContent CRD, which the shared !isolated pass
// deliberately omits.
var _ = Describe("Integration: native-CSI import data leg publishes the DataImport volume metadata", Serial, Ordered, Label("isolated"), func() {
	const (
		// The captured source PVC as the checkpoint describes it. The live namesake created below is a
		// DIFFERENT volume that merely answers to this name.
		capturedPVCName = "import-src-pvc"
		// The identity of the captured PVC, as the import binder republishes it onto the leaf's
		// status.sourceRef (importSnapshotSourceRef takes it from the recovered manifest). It cannot collide
		// with any UID this apiserver assigns.
		capturedPVCUID     = "00000000-0000-4000-8000-000000000001"
		importScratchClass = "sc-native-import-scratch"
		strangerClass      = "sc-live-namesake"
		strangerFsType     = "xfs"
		// volumeMode of the IMPORTED volume, published by storage-foundation onto the DataImport. Block is
		// chosen deliberately: nothing else in this cluster could produce it (the live namesake is Filesystem,
		// and an absent value is treated as Filesystem downstream), and it is the value whose loss corrupts a
		// restore rather than merely degrading it.
		importedVolumeMode = string(corev1.PersistentVolumeBlock)
	)

	var (
		vsGVK  = schema.GroupVersionKind{Group: snapshot.CSISnapshotGroup, Version: snapshot.CSISnapshotVersion, Kind: snapshot.KindVolumeSnapshot}
		vscGVK = schema.GroupVersionKind{Group: snapshot.CSISnapshotGroup, Version: snapshot.CSISnapshotVersion, Kind: snapshot.KindVolumeSnapshotContent}
	)

	BeforeAll(func() {
		ctx := context.Background()
		pr7InstallCSIClassAndContentCRDs(ctx)
		integrationEnsureDataImportCRD(ctx, cfg)
	})

	It("publishes volumeMode from the DataImport, invents no filesystem for a Block import, ignores a live PVC of the captured name, and heals a content published with the stranger's metadata", func() {
		ctx := context.Background()

		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ss-import-vol-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns := nsObj.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})

		By("creating a live PVC that carries the captured source name but is a different volume")
		fsMode := corev1.PersistentVolumeFilesystem
		strangerSC := strangerClass
		stranger := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: capturedPVCName},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				VolumeMode:       &fsMode,
				StorageClassName: &strangerSC,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, stranger)).To(Succeed())
		Expect(string(stranger.UID)).NotTo(Equal(capturedPVCUID), "the live namesake must not be the captured volume")

		By("creating the VolumeSnapshotContent the import produced")
		vscName := "native-import-vsc-" + ns
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
			// restoreSize gates the shared branch's latch, so without it this spec could not tell a converged
			// leg from one still waiting for the driver to report a size.
			if err := unstructured.SetNestedField(live.Object, int64(524288000), "status", "restoreSize"); err != nil {
				return err
			}
			if crdServesStatusSubresource(ctx, "volumesnapshotcontents."+snapshot.CSISnapshotGroup, snapshot.CSISnapshotVersion) {
				return k8sClient.Status().Update(ctx, live)
			}
			return k8sClient.Update(ctx, live)
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("creating the import-mode VolumeSnapshot leaf and publishing what the import binder publishes")
		leafName := "native-import-vs"
		leaf := &unstructured.Unstructured{}
		leaf.SetGroupVersionKind(vsGVK)
		leaf.SetNamespace(ns)
		leaf.SetName(leafName)
		// spec.source stays EMPTY: the module's admission webhook rejects an import-mode VolumeSnapshot that
		// carries a CSI source, because import intent is expressed by spec.mode alone and the controller fills
		// spec.source one-shot after the restore (restore rollout guard). The produced artifact is reached
		// through status.boundVolumeSnapshotContentName below, which is what the projection reads anyway.
		Expect(unstructured.SetNestedMap(leaf.Object, map[string]interface{}{
			"mode": string(storagev1alpha1.SnapshotModeImport),
		}, "spec")).To(Succeed())
		Expect(k8sClient.Create(ctx, leaf)).To(Succeed())
		Eventually(func() error {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(vsGVK)
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: leafName}, live); err != nil {
				return err
			}
			// The recovered source PVC (from the checkpoint manifest, hence the captured UID) plus the produced
			// content it is bound to — exactly the pair publishVolumeSnapshotSource and the fork write publish
			// in production.
			if err := unstructured.SetNestedMap(live.Object, map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "PersistentVolumeClaim",
				"name":       capturedPVCName,
				"namespace":  ns,
				"uid":        capturedPVCUID,
			}, "status", "sourceRef"); err != nil {
				return err
			}
			if err := unstructured.SetNestedField(live.Object, vscName, "status", "boundVolumeSnapshotContentName"); err != nil {
				return err
			}
			return k8sClient.Status().Update(ctx, live)
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
		// Guard the fixture itself: a pruned spec.mode would silently turn this into a CAPTURE leaf, no import
		// logic would run at all, and the whole spec would assert nothing.
		Eventually(func(g Gomega) {
			fresh := &unstructured.Unstructured{}
			fresh.SetGroupVersionKind(vsGVK)
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: leafName}, fresh)).To(Succeed())
			mode, _, _ := unstructured.NestedString(fresh.Object, "spec", "mode")
			g.Expect(mode).To(Equal(string(storagev1alpha1.SnapshotModeImport)), "spec.mode must survive the CRD schema")
			bound, _, _ := unstructured.NestedString(fresh.Object, "status", "boundVolumeSnapshotContentName")
			g.Expect(bound).To(Equal(vscName), "status.boundVolumeSnapshotContentName must survive the CRD schema")
			uid, _, _ := unstructured.NestedString(fresh.Object, "status", "sourceRef", "uid")
			g.Expect(uid).To(Equal(capturedPVCUID), "the captured source identity must survive the CRD schema")
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("creating the PopulateData DataImport that staged the bytes")
		di := &unstructured.Unstructured{}
		di.SetGroupVersionKind(dataImportGVKForTest)
		di.SetNamespace(ns)
		di.SetName("native-import-di")
		Expect(unstructured.SetNestedMap(di.Object, map[string]interface{}{
			"ttl":                  "24h",
			"waitForFirstConsumer": false,
			"mode":                 snapshot.DataImportModePopulateData,
			"snapshotRef": map[string]interface{}{
				"apiVersion": vsGVK.GroupVersion().String(), "kind": vsGVK.Kind, "name": leafName,
			},
			"storageParams": map[string]interface{}{
				"storageClassName": importScratchClass,
				"size":             "10Gi",
				"volumeMode":       importedVolumeMode,
			},
		}, "spec")).To(Succeed())
		Expect(k8sClient.Create(ctx, di)).To(Succeed())
		// A Block import carries no filesystem, so this DataImport publishes status.volumeMode and NO
		// status.data.fsType: asserting that absence is what pins "empty means not known, never defaulted".
		// The Filesystem case — a real fsType travelling end to end — is covered by the generic-import spec,
		// whose DataImport publishes one.
		Eventually(func() error {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(dataImportGVKForTest)
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: di.GetName()}, live); err != nil {
				return err
			}
			if err := unstructured.SetNestedMap(live.Object, map[string]interface{}{
				"apiVersion": snapshot.CSISnapshotAPIVersion,
				"kind":       snapshot.KindVolumeSnapshotContent,
				"name":       vscName,
				"uid":        string(vscUID),
			}, "status", "data", "artifactRef"); err != nil {
				return err
			}
			if err := unstructured.SetNestedField(live.Object, importedVolumeMode, "status", "volumeMode"); err != nil {
				return err
			}
			return k8sClient.Status().Update(ctx, live)
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("waiting for the runtime to create and bind this leaf's SnapshotContent")
		// Found by its spec.snapshotRef rather than by a name pattern, so the spec does not depend on the
		// content-naming helper.
		var contentName string
		Eventually(func(g Gomega) {
			list := &storagev1alpha1.SnapshotContentList{}
			g.Expect(k8sClient.List(ctx, list)).To(Succeed())
			for i := range list.Items {
				ref := list.Items[i].Spec.SnapshotRef
				if ref != nil && ref.Kind == vsGVK.Kind && ref.Name == leafName && ref.Namespace == ns {
					contentName = list.Items[i].Name
					return
				}
			}
			g.Expect(contentName).NotTo(BeEmpty(), "no SnapshotContent bound to the import leaf yet")
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed())

		contentData := func(g Gomega) storagev1alpha1.SnapshotDataBinding {
			live := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, live)).To(Succeed())
			g.Expect(live.Status.Data).NotTo(BeNil(), "the aggregator must publish status.data for an import data leg")
			return *live.Status.Data
		}
		expectImportMetadata := func(g Gomega) {
			d := contentData(g)
			g.Expect(d.VolumeMode).To(Equal(importedVolumeMode), "volumeMode must come from the DataImport, not from the live namesake PVC")
			g.Expect(d.FsType).To(BeEmpty(), "a Block import has no filesystem; none may be invented for it")
			g.Expect(d.StorageClassName).To(Equal(importScratchClass), "the class must come from the DataImport, not from the live namesake PVC")
			g.Expect(d.AccessModes).To(BeEmpty(), "the live namesake's access modes must not be published either")
			g.Expect(d.Size).NotTo(BeEmpty(), "the durable restore size must be enriched from the artifact")
		}

		By("waiting until the aggregator publishes the import volume metadata")
		Eventually(expectImportMetadata, 60*time.Second, 200*time.Millisecond).Should(Succeed())

		By("rewinding status.data to its pre-fix shape: the live namesake's metadata")
		// This is what the previous code published for such a leaf — whatever the enricher read off the PVC
		// that happened to answer to the captured name. It matches artifactRef and carries a size, so a latch
		// without per-field import terms considers it complete and freezes the stranger's values in place.
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			live := &storagev1alpha1.SnapshotContent{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, live); err != nil {
				return err
			}
			live.Status.Data.VolumeMode = string(corev1.PersistentVolumeFilesystem)
			live.Status.Data.FsType = strangerFsType
			live.Status.Data.StorageClassName = strangerClass
			return k8sClient.Status().Update(ctx, live)
		})).To(Succeed())

		By("waiting for the stale content to catch up to the DataImport values")
		Eventually(expectImportMetadata, 60*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})
