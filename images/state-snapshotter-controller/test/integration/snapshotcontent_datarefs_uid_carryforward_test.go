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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
)

// The writer of status.data has to honour the set-once rules the API now carries, because it is the writer
// that hands the apiserver a binding assembled from whatever is live THIS pass. Two uids are frozen there,
// and the live value of either can contradict the published one: an object deleted and recreated under the
// captured name is a different object with the same name.
//
// This has to be an envtest. The rule lives in the CRD and is enforced by the apiserver, so a fake client
// accepts the very write a cluster refuses, and a unit test of this writer is green either way. What the
// refusal costs is not a lost field: while status.data.Size is still empty the binding is reassembled on
// every pass, so a rejected patch is a rejection this content never gets past, on every pass, forever, and
// the content never goes Ready.
//
// k8sClient is the manager's cached client and the SnapshotContent controller writes to these objects
// concurrently, so every read here is retryable and the publish itself is retried: a publish that ran
// against a stale cache is a retry, not a verdict. What is NOT retryable is the published uid — once the
// writer has seen the published binding, the value must stay the captured one.
var _ = Describe("Integration: the data-binding writer carries published uids forward", func() {
	const (
		eventuallyTimeout = 30 * time.Second
		eventuallyPoll    = 200 * time.Millisecond
	)

	newContent := func(ctx context.Context, prefix string) string {
		GinkgoHelper()
		content := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{GenerateName: prefix},
			Spec:       retainContentSpec(),
		}
		Expect(k8sClient.Create(ctx, content)).To(Succeed())
		name := content.Name
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: name}}))
		})
		return name
	}

	// binding is the shape the projection assembles: the captured identity, plus the uids read off the live
	// objects this pass.
	binding := func(sourceUID, artifactUID types.UID) *storagev1alpha1.SnapshotDataBinding {
		return &storagev1alpha1.SnapshotDataBinding{
			SourceRef: storagev1alpha1.SnapshotSubjectRef{
				APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-captured", Namespace: "ns-captured",
				UID: sourceUID,
			},
			ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
				APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-captured",
				UID: artifactUID,
			},
		}
	}

	publish := func(ctx context.Context, name string, ref *storagev1alpha1.SnapshotDataBinding) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			g.Expect(snapshotcontent.PublishSnapshotContentDataRef(ctx, k8sClient, name, ref)).To(Succeed())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed(),
			"the writer must carry a published uid forward; handing the live one to the apiserver is a rejection this content never gets past")
	}

	// publishedUIDs reads the binding for one attempt of an Eventually/Consistently: the cached client may
	// still be behind the last accepted write, which is a retry, not a verdict.
	publishedUIDs := func(g Gomega, ctx context.Context, name string) (source, artifact types.UID) {
		fresh := &storagev1alpha1.SnapshotContent{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, fresh)).To(Succeed())
		g.Expect(fresh.Status.Data).NotTo(BeNil(), "a published binding must be observable")
		return fresh.Status.Data.SourceRef.UID, fresh.Status.Data.ArtifactRef.UID
	}

	// waitUIDs blocks until the cached client shows these uids. It is how a spec observes its own write.
	waitUIDs := func(ctx context.Context, name string, source, artifact types.UID) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			gotSource, gotArtifact := publishedUIDs(g, ctx, name)
			g.Expect(gotSource).To(Equal(source))
			g.Expect(gotArtifact).To(Equal(artifact))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	}

	// holdsUIDs requires the uids to STAY, which is the assertion a carry-forward needs: the value is
	// already correct when the spec starts looking, so an Eventually would pass against a writer that moves
	// it a moment later. The window covers the requeues a rejected patch would produce.
	holdsUIDs := func(ctx context.Context, name string, source, artifact types.UID, why string) {
		GinkgoHelper()
		Consistently(func(g Gomega) {
			gotSource, gotArtifact := publishedUIDs(g, ctx, name)
			g.Expect(gotSource).To(Equal(source), why)
			g.Expect(gotArtifact).To(Equal(artifact), why)
		}, 5*time.Second, eventuallyPoll).Should(Succeed())
	}

	It("keeps the published source uid when the live source was recreated under the captured name", func() {
		ctx := context.Background()
		name := newContent(ctx, "uid-carry-source-")

		By("publishing the binding the capture produced")
		publish(ctx, name, binding("pvc-uid-a", "vsc-uid-a"))
		waitUIDs(ctx, name, "pvc-uid-a", "vsc-uid-a")

		By("publishing again after the source was deleted and recreated under the same name")
		publish(ctx, name, binding("pvc-uid-b", "vsc-uid-a"))

		holdsUIDs(ctx, name, "pvc-uid-a", "vsc-uid-a",
			"the published source uid says WHICH object was captured — a recreated namesake is a different object")
	})

	It("keeps the published artifact uid when the live artifact was recreated under the captured name", func() {
		ctx := context.Background()
		name := newContent(ctx, "uid-carry-artifact-")

		publish(ctx, name, binding("pvc-uid-a", "vsc-uid-a"))
		waitUIDs(ctx, name, "pvc-uid-a", "vsc-uid-a")

		publish(ctx, name, binding("pvc-uid-a", "vsc-uid-b"))
		holdsUIDs(ctx, name, "pvc-uid-a", "vsc-uid-a",
			"the published artifact uid says WHICH artifact holds the snapshot")
	})

	It("still lets a uid that was unknown at first publish arrive later", func() {
		ctx := context.Background()
		name := newContent(ctx, "uid-carry-backfill-")

		By("publishing before either uid is known")
		publish(ctx, name, binding("", ""))
		waitUIDs(ctx, name, "", "")

		By("backfilling both uids on a later pass")
		publish(ctx, name, binding("pvc-uid-a", "vsc-uid-a"))

		By("observing that empty -> set is the legal direction and is not blocked by the carry-forward")
		waitUIDs(ctx, name, "pvc-uid-a", "vsc-uid-a")
	})
})
