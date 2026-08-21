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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// A data leg that is pending has, until now, said only that it is pending. The projection that knows WHY
// hands the aggregation a detail, and the aggregation substitutes it for the generic waiting text WITHOUT
// changing the reason: the situations it describes (an import whose DataImport reached Expired or Failed) do
// not close the node, so DataCapturePending must survive for every machine reader while the human-readable
// cause reaches the condition message.
var _ = Describe("the data-leg pending detail on the aggregated conditions", func() {
	const (
		genericWaitMessage = "waiting for the data leg to be published and the volume snapshot artifact to be ready"
		importCause        = "DataImport team-a/di-1 is in phase Expired and produced no data artifact: " +
			"its spec.ttl idle window elapsed before the upload finished; " +
			"create a new DataImport for this node to retry"
	)

	// aggregate publishes the conditions for a manifest-ready content whose data leg is pending, and returns
	// the DataReady and Ready conditions as persisted.
	aggregate := func(name, pendingDetail string) (dataReady, ready *metav1.Condition) {
		GinkgoHelper()
		ctx := context.Background()
		scheme := runtime.NewScheme()
		Expect(storagev1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(ssv1alpha1.AddToScheme(scheme)).To(Succeed())

		mcp := &ssv1alpha1.ManifestCheckpoint{}
		mcp.Name = "mcp-" + name
		mcp.Status.Conditions = []metav1.Condition{{
			Type:               ssv1alpha1.ManifestCheckpointConditionTypeReady,
			Status:             metav1.ConditionTrue,
			Reason:             ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
			Message:            "ok",
			LastTransitionTime: metav1.Now(),
		}}
		content := commonContentWithStatus(name, mcp.Name)
		cl := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(mcp, content).WithStatusSubresource(content).Build()
		r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

		isReady, err := r.reconcileCommonSnapshotContentStatus(ctx, content, true, pendingDetail, "", "", nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(isReady).To(BeFalse(), "a pending data leg must never read as Ready")

		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
		Expect(cl.Get(ctx, client.ObjectKey{Name: name}, fresh)).To(Succeed())
		contentLike, err := snapshot.ExtractSnapshotContentLike(fresh)
		Expect(err).NotTo(HaveOccurred())
		return snapshot.GetCondition(contentLike, snapshot.ConditionDataReady),
			snapshot.GetCondition(contentLike, snapshot.ConditionReady)
	}

	It("replaces the generic waiting text with the cause, keeping the pending reason", func() {
		dataReady, ready := aggregate("detail-present", importCause)

		Expect(dataReady).NotTo(BeNil())
		Expect(dataReady.Status).To(Equal(metav1.ConditionFalse))
		Expect(dataReady.Reason).To(Equal(snapshot.ReasonDataCapturePending),
			"the cause travels in the message; introducing a reason of its own would make the node look closed")
		Expect(dataReady.Message).To(Equal(importCause))

		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal(snapshot.ReasonDataCapturePending))
		Expect(ready.Message).To(Equal(importCause), "the derived Ready carries the cause to the leaf mirror")
	})

	It("keeps the generic waiting text when the projection knows no cause", func() {
		dataReady, ready := aggregate("detail-absent", "")

		Expect(dataReady).NotTo(BeNil())
		Expect(dataReady.Reason).To(Equal(snapshot.ReasonDataCapturePending))
		Expect(dataReady.Message).To(Equal(genericWaitMessage))
		Expect(ready.Message).To(Equal(genericWaitMessage))
	})
})
