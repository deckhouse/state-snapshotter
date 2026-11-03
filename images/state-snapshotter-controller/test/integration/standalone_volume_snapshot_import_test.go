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
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/sds-common-lib/logger"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumesnapshotimport"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// Exercises real CRD writes and the production import/manifest handlers. CSI and byte transfer are
// outside envtest: the DataImport artifact is supplied as storage-foundation would publish it.
// No sourceRef, CSI binding, Ready or public data descriptor is stamped onto the imported node.
var _ = Describe("Integration: standalone VolumeSnapshot import", Serial, Ordered, Label("isolated"), func() {
	BeforeAll(func() {
		pr7InstallCSIClassAndContentCRDs(context.Background())
		integrationEnsureDataImportCRD(context.Background(), cfg)
	})
	It("binds an uploaded volume without a parent and exposes its manifests and data for download", func() {
		ctx := context.Background()
		direct, err := client.New(cfg, client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred())
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "standalone-import-"}}
		Expect(direct.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func() { _ = direct.Delete(context.Background(), ns) })
		vsGVK := schema.GroupVersionKind{Group: snapshot.CSISnapshotGroup, Version: snapshot.CSISnapshotVersion, Kind: snapshot.KindVolumeSnapshot}
		vscGVK := vsGVK.GroupVersion().WithKind(snapshot.KindVolumeSnapshotContent)
		vs := &unstructured.Unstructured{}
		vs.SetGroupVersionKind(vsGVK)
		vs.SetNamespace(ns.Name)
		vs.SetName("selected-volume")
		Expect(unstructured.SetNestedField(vs.Object, string(storagev1alpha1.SnapshotModeImport), "spec", "mode")).To(Succeed())
		Expect(direct.Create(ctx, vs)).To(Succeed())
		key := client.ObjectKeyFromObject(vs)
		importer := &volumesnapshotimport.Controller{Client: direct, APIReader: direct}
		req := ctrl.Request{NamespacedName: key}
		content := &storagev1alpha1.SnapshotContent{}
		By("letting the generic binder create the root content and its ObjectKeeper")
		Eventually(func(g Gomega) {
			g.Expect(direct.Get(ctx, key, vs)).To(Succeed())
			name, _, e := unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName")
			g.Expect(e).NotTo(HaveOccurred())
			g.Expect(name).NotTo(BeEmpty())
			g.Expect(direct.Get(ctx, client.ObjectKey{Name: name}, content)).To(Succeed())
			g.Expect(content.OwnerReferences).To(HaveLen(1))
			g.Expect(content.OwnerReferences[0].Kind).To(Equal("ObjectKeeper"))
		}, 60*time.Second, 100*time.Millisecond).Should(Succeed())
		DeferCleanup(func() { _ = direct.Delete(context.Background(), content) })
		rootOwner := content.OwnerReferences[0]
		// The namespace is generated per test, so this payload is assembled from runtime identity.
		source := map[string]interface{}{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]interface{}{
			"namespace": ns.Name, "name": "original-pvc", "uid": "captured-pvc-uid",
		}}
		manifests, err := json.Marshal([]interface{}{source})
		Expect(err).NotTo(HaveOccurred())
		_, err = usecase.NewImportUploadService(direct).UploadToContent(ctx, content, manifests)
		Expect(err).NotTo(HaveOccurred())
		By("publishing the artifact produced by storage-foundation")
		vsc := &unstructured.Unstructured{}
		vsc.SetGroupVersionKind(vscGVK)
		vsc.SetName("imported-" + ns.Name)
		Expect(unstructured.SetNestedMap(vsc.Object, map[string]interface{}{
			"deletionPolicy": "Delete", "driver": "test.csi.storage.k8s.io", "source": map[string]interface{}{"snapshotHandle": vsc.GetName()},
			"volumeSnapshotRef": map[string]interface{}{"name": "staging-snapshot", "namespace": ns.Name},
		}, "spec")).To(Succeed())
		Expect(direct.Create(ctx, vsc)).To(Succeed())
		DeferCleanup(func() { _ = direct.Delete(context.Background(), vsc) })
		Expect(unstructured.SetNestedField(vsc.Object, true, "status", "readyToUse")).To(Succeed())
		Expect(unstructured.SetNestedField(vsc.Object, int64(1073741824), "status", "restoreSize")).To(Succeed())
		if crdServesStatusSubresource(ctx, "volumesnapshotcontents."+snapshot.CSISnapshotGroup, snapshot.CSISnapshotVersion) {
			Expect(direct.Status().Update(ctx, vsc)).To(Succeed())
		} else {
			Expect(direct.Update(ctx, vsc)).To(Succeed())
		}
		di := &unstructured.Unstructured{}
		di.SetGroupVersionKind(dataImportGVKForTest)
		di.SetNamespace(ns.Name)
		di.SetName("selected-import")
		Expect(unstructured.SetNestedMap(di.Object, map[string]interface{}{
			"mode": snapshot.DataImportModePopulateData, "ttl": "1h",
			"snapshotRef":   map[string]interface{}{"apiVersion": vsGVK.GroupVersion().String(), "kind": vsGVK.Kind, "name": vs.GetName()},
			"storageParams": map[string]interface{}{"storageClassName": "import-class", "size": "1Gi", "volumeMode": string(corev1.PersistentVolumeBlock)},
		}, "spec")).To(Succeed())
		Expect(direct.Create(ctx, di)).To(Succeed())
		Expect(unstructured.SetNestedMap(di.Object, map[string]interface{}{
			"apiVersion": vscGVK.GroupVersion().String(), "kind": vscGVK.Kind, "name": vsc.GetName(), "uid": string(vsc.GetUID()),
		}, "status", "data", "artifactRef")).To(Succeed())
		Expect(unstructured.SetNestedField(di.Object, string(corev1.PersistentVolumeBlock), "status", "volumeMode")).To(Succeed())
		Expect(direct.Status().Update(ctx, di)).To(Succeed())
		By("converging without an artificial parent or hand-written node status")
		Eventually(func(g Gomega) {
			_, e := importer.Reconcile(ctx, req)
			g.Expect(e).NotTo(HaveOccurred())
			g.Expect(direct.Get(ctx, key, vs)).To(Succeed())
			g.Expect(vs.GetOwnerReferences()).To(BeEmpty())
			bound, _, e := unstructured.NestedString(vs.Object, "status", "boundVolumeSnapshotContentName")
			g.Expect(e).NotTo(HaveOccurred())
			g.Expect(bound).To(Equal(vsc.GetName()))
			uid, _, e := unstructured.NestedString(vs.Object, "status", "sourceRef", "uid")
			g.Expect(e).NotTo(HaveOccurred())
			g.Expect(uid).To(Equal("captured-pvc-uid"))
			mode, _, e := unstructured.NestedString(vs.Object, "status", "data", "volumeMode")
			g.Expect(e).NotTo(HaveOccurred())
			g.Expect(mode).To(Equal(string(corev1.PersistentVolumeBlock)))
			like, e := snapshot.ExtractSnapshotLike(vs)
			g.Expect(e).NotTo(HaveOccurred())
			g.Expect(snapshot.IsReady(like)).To(BeTrue())
		}, 60*time.Second, 100*time.Millisecond).Should(Succeed())
		Expect(direct.Get(ctx, client.ObjectKeyFromObject(vsc), vsc)).To(Succeed())
		boundUID, _, err := unstructured.NestedString(vsc.Object, "spec", "volumeSnapshotRef", "uid")
		Expect(err).NotTo(HaveOccurred())
		Expect(boundUID).To(Equal(string(vs.GetUID())))
		Expect(direct.Get(ctx, client.ObjectKeyFromObject(content), content)).To(Succeed())
		Expect(content.OwnerReferences).To(ConsistOf(rootOwner))
		By("reading back the uploaded manifest through the download implementation")
		reader := usecase.NewAggregatedNamespaceManifests(direct, usecase.NewArchiveService(direct, direct, logger.NewNop()), nil)
		downloaded, err := reader.BuildSingleNodeJSONFromContent(ctx, content.Name)
		Expect(err).NotTo(HaveOccurred())
		Expect(downloaded).To(MatchJSON(manifests))
		By("retaining the downloadable descriptor after DataImport cleanup")
		Expect(direct.Delete(ctx, di)).To(Succeed())
		// Clear only the consumer mirror, to prove the steady-state importer can reconstruct it from content.
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if e := direct.Get(ctx, key, vs); e != nil {
				return e
			}
			unstructured.RemoveNestedField(vs.Object, "status", "data")
			return direct.Status().Update(ctx, vs)
		})).To(Succeed())
		Eventually(func(g Gomega) {
			_, e := importer.Reconcile(ctx, req)
			g.Expect(e).NotTo(HaveOccurred())
			g.Expect(direct.Get(ctx, key, vs)).To(Succeed())
			artifact, _, e := unstructured.NestedString(vs.Object, "status", "data", "artifactRef", "name")
			g.Expect(e).NotTo(HaveOccurred())
			g.Expect(artifact).To(Equal(vsc.GetName()))
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())
		GinkgoWriter.Println("checked 1 standalone CSI import, CSI back-reference, public data, manifest download and post-DataImport recovery")
	})
})
