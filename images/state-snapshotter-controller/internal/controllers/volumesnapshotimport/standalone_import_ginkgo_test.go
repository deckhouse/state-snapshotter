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

package volumesnapshotimport

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
)

// The content shell belongs to the generic binder. These cases check that the CSI importer waits for
// that shell, then processes a parentless node. The envtest covers artifact binding through the API.
var _ = Describe("a standalone imported VolumeSnapshot", func() {
	It("mirrors its published data after the DataImport is gone without a parent", func() {
		ctx := context.Background()
		vs := vsImportLeaf(vsImportVSCName)
		vs.SetOwnerReferences(nil)
		content := vsImportLeafContent(vsImportPublishedData())
		content.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "deckhouse.io/v1alpha1", Kind: "ObjectKeeper", Name: "root-keeper", UID: "keeper-uid"}})
		owners := content.GetOwnerReferences()
		mcp := &ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: usecase.ReconstructedManifestCheckpointName(vs.GetUID(), "")}}
		cl := fake.NewClientBuilder().WithScheme(vsImportScheme()).WithStatusSubresource(vs, &storagev1alpha1.SnapshotContent{}).WithObjects(vs, content, mcp).Build()
		r := &Controller{Client: cl, APIReader: cl}
		result, err := r.Reconcile(ctx, vsImportRequest())
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())
		Expect(cl.Get(ctx, client.ObjectKeyFromObject(vs), vs)).To(Succeed())
		data, found, err := unstructured.NestedMap(vs.Object, "status", "data")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		want, err := runtime.DefaultUnstructuredConverter.ToUnstructured(content.Status.Data)
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(Equal(want))
		Expect(cl.Get(ctx, client.ObjectKeyFromObject(content), content)).To(Succeed())
		Expect(content.GetOwnerReferences()).To(Equal(owners))
		rv := vs.GetResourceVersion()
		_, err = r.Reconcile(ctx, vsImportRequest())
		Expect(err).NotTo(HaveOccurred())
		Expect(cl.Get(ctx, client.ObjectKeyFromObject(vs), vs)).To(Succeed())
		Expect(vs.GetResourceVersion()).To(Equal(rv))
		GinkgoWriter.Println("checked standalone data mirror, root ownership and idempotence")
	})
	It("waits for the generic binder without creating a second root content", func() {
		vs := vsImportLeaf("")
		vs.SetOwnerReferences(nil)
		unstructured.RemoveNestedField(vs.Object, "status", "boundSnapshotContentName")
		cl := fake.NewClientBuilder().WithScheme(vsImportScheme()).WithObjects(vs).Build()
		r := &Controller{Client: cl, APIReader: cl}
		result, err := r.Reconcile(context.Background(), vsImportRequest())
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(importPollInterval))
		contents := &storagev1alpha1.SnapshotContentList{}
		Expect(cl.List(context.Background(), contents)).To(Succeed())
		Expect(contents.Items).To(BeEmpty())
	})
})
