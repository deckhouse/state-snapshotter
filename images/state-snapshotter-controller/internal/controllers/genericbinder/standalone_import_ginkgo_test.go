//go:build !unit_ginkgo

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

package genericbinder

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	deckhousev1alpha1 "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/deckhouseio/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func TestStandaloneImportGinkgo(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Standalone import Suite")
}

// This fake-client regression checks the public data descriptor, including corrections after the
// DataImport is gone. It does not simulate the CSI/data-transfer controllers or apiserver pruning.
var _ = Describe("a standalone imported domain root", func() {
	It("publishes and refreshes the complete data descriptor without changing Ready", func() {
		ctx := context.Background()
		scheme := runtime.NewScheme()
		Expect(storagev1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(deckhousev1alpha1.AddToScheme(scheme)).To(Succeed())
		root := domainSnapshotStatusStub()
		root.SetNamespace(domainTestNS)
		root.SetName(domainTestSnap)
		root.SetUID(domainTestSnapUID)
		Expect(unstructured.SetNestedField(root.Object, string(storagev1alpha1.SnapshotModeImport), "spec", "mode")).To(Succeed())
		Expect(unstructured.SetNestedField(root.Object, domainTestContent, "status", "boundSnapshotContentName")).To(Succeed())
		like, err := snapshot.ExtractSnapshotLike(root)
		Expect(err).NotTo(HaveOccurred())
		snapshot.SetCondition(like, snapshot.ConditionReady, metav1.ConditionTrue, snapshot.ReasonCompleted, "imported")
		conditions, _, err := unstructured.NestedSlice(root.Object, "status", "conditions")
		Expect(err).NotTo(HaveOccurred())
		content := domainTestSnapshotContent()
		content.Status.Data = importTestContentData()
		cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(root, &storagev1alpha1.SnapshotContent{}).WithObjects(root, content).Build()
		reg := snapshot.NewGVKRegistry()
		Expect(reg.RegisterSnapshotContentMapping(domainSnapshotGVK.Kind, domainSnapshotGVK.GroupVersion().String(), "SnapshotContent", storagev1alpha1.SchemeGroupVersion.String())).To(Succeed())
		reg.MarkRequiresDataArtifact(domainSnapshotGVK.Kind, true)
		binder := &GenericSnapshotBinderController{Client: cl, APIReader: cl, Scheme: scheme, Config: &config.Options{}, GVKRegistry: reg, SnapshotGVKs: []schema.GroupVersionKind{domainSnapshotGVK}}
		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(root)}
		for _, size := range []string{"10Gi", "12Gi"} {
			Expect(cl.Get(ctx, client.ObjectKeyFromObject(content), content)).To(Succeed())
			content.Status.Data.Size = size
			Expect(cl.Status().Update(ctx, content)).To(Succeed())
			for i := 0; i < 3; i++ {
				_, err = binder.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
			}
			fresh := domainSnapshotStatusStub()
			Expect(cl.Get(ctx, req.NamespacedName, fresh)).To(Succeed())
			data, found, err := unstructured.NestedMap(fresh.Object, "status", "data")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			want, err := runtime.DefaultUnstructuredConverter.ToUnstructured(content.Status.Data)
			Expect(err).NotTo(HaveOccurred())
			Expect(data).To(Equal(want))
			got, _, err := unstructured.NestedSlice(fresh.Object, "status", "conditions")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(conditions))
			Expect(fresh.GetOwnerReferences()).To(BeEmpty())
			rv := fresh.GetResourceVersion()
			_, err = binder.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(cl.Get(ctx, req.NamespacedName, fresh)).To(Succeed())
			Expect(fresh.GetResourceVersion()).To(Equal(rv))
		}
		GinkgoWriter.Println("checked 2 data descriptors and stable Ready on a root with no DataImport")
	})
})
