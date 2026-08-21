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

package snapshotcontent

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// The generic-import data leg is the only route that can explain a PENDING leg, and these specs pin what it
// reports through the projection the aggregation actually calls — not just through the message builder.
// The two invariants under test are the ones the import cause must never break: it stays a DETAIL (no
// terminal reason is ever produced, so the client keeps its path to another attempt), and it is reported only
// while this leg is what holds the content back.
var _ = Describe("the generic-import data leg reports why a pending import will not deliver", func() {
	const (
		expiredCause = "DataImport ns1/di-1 is in phase Expired and produced no data artifact: " +
			"its spec.ttl idle window elapsed before the upload finished; " +
			"create a new DataImport for this node to retry"
		failedCause = "DataImport ns1/di-1 is in phase Failed and produced no data artifact; " +
			"create a new DataImport for this node to retry"
	)

	// stalledDataImport is the PopulateData DataImport of the leaf fixture in a phase from which no artifact
	// will come: it targets the leaf and publishes NO status.data at all, which is the state of an import
	// that never finished its upload.
	stalledDataImport := func(phase string) *unstructured.Unstructured {
		di := &unstructured.Unstructured{}
		di.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "storage-foundation.deckhouse.io", Version: "v1alpha1", Kind: "DataImport"})
		di.SetNamespace(importDataImportNS)
		di.SetName(importDataImportName)
		Expect(unstructured.SetNestedField(di.Object, snapshot.DataImportModePopulateData, "spec", "mode")).To(Succeed())
		Expect(unstructured.SetNestedMap(di.Object, map[string]interface{}{
			"apiVersion": importLeafAPIVer, "kind": importLeafKind, "name": importLeafObjName,
		}, "spec", "snapshotRef")).To(Succeed())
		Expect(unstructured.SetNestedMap(di.Object, map[string]interface{}{
			"storageClassName": importScratchStorageClass, "size": "10Gi",
			"volumeMode": string(corev1.PersistentVolumeFilesystem),
		}, "spec", "storageParams")).To(Succeed())
		if phase != "" {
			Expect(unstructured.SetNestedField(di.Object, phase, "status", "phase")).To(Succeed())
		}
		return di
	}

	// publishedContent is a leaf content whose data leg is already durable (artifact plus the restore size),
	// i.e. one that no longer waits for any import.
	publishedContent := func() *storagev1alpha1.SnapshotContent {
		return &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: projTestContent},
			Status: storagev1alpha1.SnapshotContentStatus{
				Data: &storagev1alpha1.SnapshotDataBinding{
					SourceRef: storagev1alpha1.SnapshotSubjectRef{
						APIVersion: importLeafAPIVer, Kind: importLeafKind,
						Namespace: projTestNS, Name: importLeafObjName,
					},
					ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
						APIVersion: snapshot.CSISnapshotAPIVersion,
						Kind:       snapshot.KindVolumeSnapshotContent,
						Name:       projTestVSCName,
					},
					Size: "500Mi",
				},
			},
		}
	}

	// project runs the data-leg projection over a data-bearing import leaf, with the given content and
	// DataImport as the only relevant cluster state.
	project := func(content *storagev1alpha1.SnapshotContent, di *unstructured.Unstructured, dataBearing bool) (requeue bool, pendingDetail string) {
		GinkgoHelper()
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(storagev1alpha1.AddToScheme(scheme)).To(Succeed())
		scheme.AddKnownTypeWithName(dataImportListGVK, &unstructured.UnstructuredList{})
		cl := fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
			WithObjects(content, di).Build()
		reg := snapshot.NewGVKRegistry()
		reg.MarkRequiresDataArtifact(importLeafKind, dataBearing)
		r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: reg}

		requeue, termReason, termMessage, pendingDetail, err := r.reconcileDataLegProjection(
			context.Background(), projContentObj(), importOwnerLeaf(), projTestNS, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(termReason).To(BeEmpty(), "a stalled import must never become terminal: another DataImport may still be created")
		Expect(termMessage).To(BeEmpty())
		return requeue, pendingDetail
	}

	emptyContent := func() *storagev1alpha1.SnapshotContent {
		return &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: projTestContent}}
	}

	It("reports the elapsed idle window of an Expired import while the leg is unpublished", func() {
		requeue, detail := project(emptyContent(), stalledDataImport(snapshot.DataImportPhaseExpired), true)

		Expect(requeue).To(BeTrue(), "the leg is still pending")
		Expect(detail).To(Equal(expiredCause))
	})

	It("reports a Failed import without claiming an idle window", func() {
		requeue, detail := project(emptyContent(), stalledDataImport(snapshot.DataImportPhaseFailed), true)

		Expect(requeue).To(BeTrue())
		Expect(detail).To(Equal(failedCause))
	})

	It("reports nothing for an import that has not reached a phase yet", func() {
		requeue, detail := project(emptyContent(), stalledDataImport(""), true)

		Expect(requeue).To(BeTrue(), "an import still converging keeps the leg pending")
		Expect(detail).To(BeEmpty())
	})

	It("leaves a leaf whose data leg is already published alone", func() {
		requeue, detail := project(publishedContent(), stalledDataImport(snapshot.DataImportPhaseExpired), true)

		Expect(requeue).To(BeFalse(), "a published data leg is durable and does not wait for any import")
		Expect(detail).To(BeEmpty(), "reporting a cause here would describe a captured leaf as stuck")
	})

	It("reports nothing for a structural import node, which has no data leg to explain", func() {
		requeue, detail := project(emptyContent(), stalledDataImport(snapshot.DataImportPhaseExpired), false)

		Expect(requeue).To(BeFalse())
		Expect(detail).To(BeEmpty())
	})
})
