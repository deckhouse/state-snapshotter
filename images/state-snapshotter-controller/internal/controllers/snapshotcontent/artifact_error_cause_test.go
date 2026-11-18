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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// A CSI snapshot that the driver refuses (no free space in the pool, for example) leaves the
// VolumeSnapshotContent readyToUse=false with the driver's error in status.error, and the CSI snapshotter
// keeps retrying. The capture keeps waiting, so the reason stays pending, but the error must reach the
// condition message of the leaf and, through the child aggregation, of the ancestors a user reads.
var _ = Describe("the error a volume snapshot reports while the capture waits for it", func() {
	const (
		csiError = `failed to take snapshot of the volume pvc-1: "rpc error: code = FailedPrecondition ` +
			`desc = not enough space in pool thinpool (lvg lvg-1): 0; need at least 1073741824"`
		errorLeafMessage = "waiting for volume snapshot artifacts: 0/1 ready; pending: vsc-err; " +
			"VolumeSnapshotContent vsc-err of VolumeSnapshot team-a/nss-vs-1 reports an error: " + csiError
	)

	newScheme := func() *runtime.Scheme {
		GinkgoHelper()
		scheme := runtime.NewScheme()
		Expect(storagev1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(ssv1alpha1.AddToScheme(scheme)).To(Succeed())
		return scheme
	}

	// vsc builds a not-ready VolumeSnapshotContent bound to VolumeSnapshot team-a/nss-vs-1, reporting errMsg
	// in status.error when it is not empty.
	vsc := func(name, errMsg string) *unstructured.Unstructured {
		obj := volumeSnapshotContentObject(name, false)
		Expect(unstructured.SetNestedField(obj.Object, "team-a", "spec", "volumeSnapshotRef", "namespace")).To(Succeed())
		Expect(unstructured.SetNestedField(obj.Object, "nss-vs-1", "spec", "volumeSnapshotRef", "name")).To(Succeed())
		if errMsg != "" {
			Expect(unstructured.SetNestedField(obj.Object, errMsg, "status", "error", "message")).To(Succeed())
			Expect(unstructured.SetNestedField(obj.Object, "2026-11-18T15:00:11Z", "status", "error", "time")).To(Succeed())
		}
		return obj
	}

	// leafReady derives the Ready condition of a manifest-ready content whose single data artifact is vscObj.
	leafReady := func(vscObj *unstructured.Unstructured) (string, string) {
		GinkgoHelper()
		mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
		cl := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(mcp, vscObj).Build()
		r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

		content := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
			"kind":       "SnapshotContent",
			"metadata":   map[string]interface{}{"name": "vs-content"},
			"status": map[string]interface{}{
				"manifestCheckpointName": "mcp-ok",
				"data":                   dataRefEntry("pvc-1", vscObj.GetName()),
			},
		}}
		content.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())

		plan, err := r.buildCommonSnapshotContentStatusPlan(context.Background(), content, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(plan.readyStatus).To(Equal(metav1.ConditionFalse))
		Expect(plan.volumeFailed).To(BeFalse(), "an error the CSI snapshotter retries must not fail the node")
		return plan.readyReason, plan.readyMessage
	}

	It("names the VolumeSnapshot and quotes its error on the waiting leaf, keeping the pending reason", func() {
		reason, message := leafReady(vsc("vsc-err", csiError))

		Expect(reason).To(Equal(snapshot.ReasonDataCapturePending))
		Expect(message).To(Equal(errorLeafMessage))
	})

	It("keeps the plain progress message while the artifact reports no error", func() {
		reason, message := leafReady(vsc("vsc-err", ""))

		Expect(reason).To(Equal(snapshot.ReasonDataCapturePending))
		Expect(message).To(Equal("waiting for volume snapshot artifacts: 0/1 ready; pending: vsc-err"))
	})

	It("carries the error up to the parent that waits for the leaf", func() {
		leafReason, leafMessage := leafReady(vsc("vsc-err", csiError))

		cl := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(
			contentWithReadyCond("vs-content-ok", metav1.ConditionTrue, snapshot.ReasonCompleted, "ready"),
			contentWithReadyCond("vs-content", metav1.ConditionFalse, leafReason, leafMessage),
		).Build()
		r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

		ready, reason, message, err := r.validateCommonContentChildren(
			context.Background(), parentContentWithChildRefs("root", "vs-content-ok", "vs-content"))
		Expect(err).NotTo(HaveOccurred())
		Expect(ready).To(BeFalse())
		Expect(reason).To(Equal(snapshot.ReasonChildrenPending), "the parent keeps waiting, it does not fail")
		Expect(message).To(Equal("waiting for child snapshot contents: 1/2 ready; pending: vs-content; " +
			buildChildrenPendingMessage("vs-content", "vs-content", snapshot.ReasonDataCapturePending, errorLeafMessage)))
	})

	Context("for a VolumeSnapshot not bound to a VolumeSnapshotContent yet", func() {
		project := func(errMsg string) (bool, string) {
			GinkgoHelper()
			owner := &unstructured.Unstructured{}
			owner.SetAPIVersion(snapshot.CSISnapshotAPIVersion)
			owner.SetKind(snapshot.KindVolumeSnapshot)
			owner.SetNamespace("team-a")
			owner.SetName("nss-vs-1")
			if errMsg != "" {
				Expect(unstructured.SetNestedField(owner.Object, errMsg, "status", "error", "message")).To(Succeed())
			}
			content := commonContentWithStatus("vs-content", "mcp-ok")
			cl := fake.NewClientBuilder().WithScheme(newScheme()).Build()
			r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

			requeue, termReason, termMessage, pendingDetail, err := r.projectContentDataLegFromBoundVSC(
				context.Background(), content, owner, "team-a")
			Expect(err).NotTo(HaveOccurred())
			Expect(termReason).To(BeEmpty(), "an unbound VolumeSnapshot is retried, never terminal")
			Expect(termMessage).To(BeEmpty())
			return requeue, pendingDetail
		}

		It("hands the aggregation the error the VolumeSnapshot reports as the cause of the wait", func() {
			const bindError = `Failed to create snapshot content with error snapshot controller failed to update ` +
				`nss-vs-1 on API server: cannot get claim from snapshot`
			requeue, detail := project(bindError)

			Expect(requeue).To(BeTrue())
			Expect(detail).To(Equal("VolumeSnapshot team-a/nss-vs-1 is not bound to a VolumeSnapshotContent " +
				"and reports an error: " + bindError))
		})

		It("hands no cause while the VolumeSnapshot reports no error", func() {
			requeue, detail := project("")

			Expect(requeue).To(BeTrue())
			Expect(detail).To(BeEmpty())
		})
	})
})
