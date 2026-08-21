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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// The cause a waiting leaf knows must be readable on the ancestor a user starts from, so the aggregate
// ChildrenPending message quotes one waiting child's OWN reason and message on top of the progress list it
// always carried. These specs pin the selection rule and the shape of that quote; which cause a leaf reports
// is not their subject.
var _ = Describe("the ChildrenPending message quotes a waiting child's own cause", func() {
	const (
		importCause = "DataImport team-a/di-1 is in phase Expired and produced no data artifact"
		artifactMsg = "VolumeSnapshotContent vsc-x is not readyToUse"
	)

	// childrenPlan runs the child aggregation over a parent whose frozen edge set lists childNames in that
	// exact order. A name with no matching object stands for a child the cache has not delivered — which is
	// how a pending child with no reason at all occurs in production.
	childrenPlan := func(childNames []string, children ...*storagev1alpha1.SnapshotContent) (string, string) {
		GinkgoHelper()
		scheme := runtime.NewScheme()
		Expect(storagev1alpha1.AddToScheme(scheme)).To(Succeed())
		builder := fake.NewClientBuilder().WithScheme(scheme)
		for _, c := range children {
			builder = builder.WithObjects(c)
		}
		cl := builder.Build()
		r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

		ready, reason, message, err := r.validateCommonContentChildren(
			context.Background(), parentContentWithChildRefs("parent", childNames...))
		Expect(err).NotTo(HaveOccurred())
		Expect(ready).To(BeFalse())
		return reason, message
	}

	It("keeps the progress list and appends the cause of the single waiting child", func() {
		reason, message := childrenPlan(
			[]string{"leaf-c"},
			contentWithReadyCond("leaf-c", metav1.ConditionFalse, snapshot.ReasonDataCapturePending, importCause),
		)

		Expect(reason).To(Equal(snapshot.ReasonChildrenPending))
		Expect(message).To(HavePrefix("waiting for child snapshot contents: 0/1 ready; pending: leaf-c"))
		Expect(message).To(HaveSuffix(buildChildrenPendingMessage(
			"leaf-c", "leaf-c", snapshot.ReasonDataCapturePending, importCause)))
	})

	It("quotes the FIRST waiting child in edge order and only names the rest", func() {
		reason, message := childrenPlan(
			[]string{"leaf-a", "leaf-b"},
			contentWithReadyCond("leaf-a", metav1.ConditionFalse, snapshot.ReasonDataCapturePending, importCause),
			contentWithReadyCond("leaf-b", metav1.ConditionFalse, snapshot.ReasonArtifactNotReady, artifactMsg),
		)

		Expect(reason).To(Equal(snapshot.ReasonChildrenPending))
		Expect(message).To(ContainSubstring("pending: leaf-a, leaf-b"), "every waiting child stays named")
		Expect(message).To(HaveSuffix(buildChildrenPendingMessage(
			"leaf-a", "leaf-a", snapshot.ReasonDataCapturePending, importCause)))
		Expect(message).NotTo(ContainSubstring(artifactMsg), "only one cause is quoted")
		Expect(strings.Count(message, "child SnapshotContent ")).To(Equal(1), "exactly one quote per message")
	})

	It("skips the children that have no cause to state, and quotes the first that has one", func() {
		// Two ways a waiting child says nothing, both of them real: it is missing from the cache (no object
		// to read at all), and it exists but the aggregation has not written its Ready yet. Quoting either
		// would replace the one cause available with silence — and reading a condition that is not there is
		// how a nil dereference gets in.
		reason, message := childrenPlan(
			[]string{"leaf-uncached", "leaf-nocondition", "leaf-b"},
			&storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "leaf-nocondition"}},
			contentWithReadyCond("leaf-b", metav1.ConditionFalse, snapshot.ReasonArtifactNotReady, artifactMsg),
		)

		Expect(reason).To(Equal(snapshot.ReasonChildrenPending))
		Expect(message).To(ContainSubstring("pending: leaf-uncached, leaf-nocondition, leaf-b"),
			"every waiting child stays named")
		Expect(message).To(HaveSuffix(buildChildrenPendingMessage(
			"leaf-b", "leaf-b", snapshot.ReasonArtifactNotReady, artifactMsg)),
			"the first child with a reason is quoted, not merely the first child")
	})

	It("re-quotes the deepest waiting leaf instead of nesting an intermediate node's whole message", func() {
		// The intermediate child's message is exactly what this aggregation produces one level down, so the
		// spec cannot pass on a shape the code never writes.
		_, intermediateMessage := childrenPlan(
			[]string{"deep-leaf"},
			contentWithReadyCond("deep-leaf", metav1.ConditionFalse, snapshot.ReasonDataCapturePending, importCause),
		)

		reason, message := childrenPlan(
			[]string{"mid-c"},
			contentWithReadyCond("mid-c", metav1.ConditionFalse, snapshot.ReasonChildrenPending, intermediateMessage),
		)

		Expect(reason).To(Equal(snapshot.ReasonChildrenPending))
		Expect(message).To(HaveSuffix(buildChildrenPendingMessage(
			"mid-c", "deep-leaf", snapshot.ReasonDataCapturePending, importCause)),
			"the grandparent names its direct child but pins the leaf that actually waits")
		Expect(strings.Count(message, "child SnapshotContent ")).To(Equal(1),
			"the message must not grow by one nested copy per tree level")
	})
})

// The quote format is shared by the terminal and the waiting states and differs only in its verb. These
// specs check the SKELETON of that format — a fixed leaf/reason/message triple with no real cause text in it
// — deliberately: a real waiting cause may legitimately contain the word "failed" (a DataImport in phase
// Failed is named by the leaf's own message), so asserting "the assembled message says nothing about
// failure" would go red on correct code, and asserting it on an Expired fixture would quietly check nothing.
var _ = Describe("the child-quote format", func() {
	const (
		directChild = "child-c"
		leaf        = "leaf-c"
		leafReason  = "SomeReason"
		leafMessage = "some cause"
	)

	It("says the waiting child failed nowhere", func() {
		Expect(buildChildrenPendingMessage(directChild, leaf, leafReason, leafMessage)).To(Equal(
			"child SnapshotContent child-c is not ready yet: leaf=leaf-c reason=SomeReason message=some cause"))
	})

	It("keeps the canonical terminal form, and its parse, untouched", func() {
		terminal := buildChildrenFailedMessage(directChild, leaf, leafReason, leafMessage)
		Expect(terminal).To(Equal(
			"child SnapshotContent child-c failed: leaf=leaf-c reason=SomeReason message=some cause"))

		gotLeaf, gotReason, gotMessage, ok := parseChildrenFailedLeaf(terminal)
		Expect(ok).To(BeTrue())
		Expect([]string{gotLeaf, gotReason, gotMessage}).To(Equal([]string{leaf, leafReason, leafMessage}))
	})

	It("is never read as a failure when it is a waiting quote", func() {
		_, _, _, ok := parseChildrenFailedLeaf(buildChildrenPendingMessage(directChild, leaf, leafReason, leafMessage))
		Expect(ok).To(BeFalse())
	})

	It("stays parseable where it actually appears: after the progress fragment of a pending message", func() {
		embedded := "waiting for child snapshot contents: 0/1 ready; pending: " + directChild + "; " +
			buildChildrenPendingMessage(directChild, leaf, leafReason, leafMessage)

		gotLeaf, gotReason, gotMessage, ok := parseChildrenPendingLeaf(embedded)
		Expect(ok).To(BeTrue())
		Expect([]string{gotLeaf, gotReason, gotMessage}).To(Equal([]string{leaf, leafReason, leafMessage}))
	})

	It("is never read as a waiting quote when it is a terminal one", func() {
		_, _, _, ok := parseChildrenPendingLeaf(buildChildrenFailedMessage(directChild, leaf, leafReason, leafMessage))
		Expect(ok).To(BeFalse())
	})
})
