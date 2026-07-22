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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

func TestSnapshotContentRequeue(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "SnapshotContent requeue classification")
}

var _ = DescribeTable("classifying active SnapshotContent requeues",
	func(status metav1.ConditionStatus, reason string, ready, projectionRequeue, want bool) {
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(
			contentWithReadyCond("requeue-classification", status, reason, reason),
		)
		Expect(err).NotTo(HaveOccurred())
		content := &unstructured.Unstructured{Object: raw}
		content.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())

		Expect(snapshotContentNeedsActiveRequeue(content, ready, projectionRequeue)).To(Equal(want))
	},
	Entry("stops for stable Ready=True",
		metav1.ConditionTrue, snapshot.ReasonCompleted, true, false, false),
	Entry("finishes a same-pass projection for Ready=True",
		metav1.ConditionTrue, snapshot.ReasonCompleted, true, true, true),
	Entry("keeps the safety poll for pending Ready=False",
		metav1.ConditionFalse, snapshot.ReasonChildrenPending, false, false, true),
	Entry("stops a canonical terminal despite a projection requeue",
		metav1.ConditionFalse, snapshot.ReasonVolumeCaptureFailed, false, true, false),
	Entry("stops a content-internal terminal despite a projection requeue",
		metav1.ConditionFalse, snapshot.ReasonDomainCaptureFailed, false, true, false),
)
