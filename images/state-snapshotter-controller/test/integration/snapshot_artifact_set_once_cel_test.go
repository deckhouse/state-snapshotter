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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// A published snapshot artifact is set-once, and these are the admission contract tests for that: what the
// core keeps by discipline (it holds a published identity and restores a wiped latch from it), the API makes
// unbreakable for every OTHER writer too — including one whose model has no such field and replaces the
// whole status. Each axis is guarded by a pair of rules: the field rule refuses a CHANGE, and a companion on
// the root object refuses a REMOVAL, which a transition rule on the field itself cannot see. The pairs are
// asserted one axis at a time; a rejection here is an apierrors.IsInvalid from CEL, not a controller
// decision, so it holds against kubectl and against a foreign client alike.
//
// The suite installing at all is part of the coverage: an over-budget CEL cost estimate makes the apiserver
// refuse the whole CRD, so it would fail BeforeSuite rather than one spec.
var _ = Describe("Integration: a published snapshot artifact is set-once", func() {
	newContent := func(ctx context.Context, prefix string) client.ObjectKey {
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
		return client.ObjectKey{Name: name}
	}

	// updateContentStatus mutates a freshly read content inside Eventually and asserts the apiserver's
	// verdict: wantRejection empty means the update must be accepted, otherwise it must be rejected as
	// Invalid AND the message must name the rule that did it — with five rules on one field group, "some
	// rule refused this" would not distinguish a namespace rewrite caught by the namespace rule from one
	// caught by accident. Everything about the read is retryable on purpose: the client is the manager's
	// cached one, so a just-written status may not be in the cache yet, and the SnapshotContent controller
	// writes its own finalizer and conditions concurrently. The mutation gets the attempt's Gomega so it can
	// state what it needs to be there (a published binding to mutate) and be retried instead of
	// dereferencing a stale nil.
	updateContentStatus := func(ctx context.Context, key client.ObjectKey, wantRejection string, mutate func(Gomega, *storagev1alpha1.SnapshotContent)) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			mutate(g, fresh)
			err := k8sClient.Status().Update(ctx, fresh)
			if wantRejection == "" {
				g.Expect(err).NotTo(HaveOccurred())
				return
			}
			g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected a CEL Invalid rejection, got %v", err)
			g.Expect(err.Error()).To(ContainSubstring(wantRejection))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
	}

	// publishedData reads the binding for one attempt of an Eventually — the cached client may still be
	// behind the last accepted write, which is a retry, not a verdict.
	publishedData := func(g Gomega, ctx context.Context, key client.ObjectKey) *storagev1alpha1.SnapshotDataBinding {
		fresh := &storagev1alpha1.SnapshotContent{}
		g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
		g.Expect(fresh.Status.Data).NotTo(BeNil())
		return fresh.Status.Data
	}

	It("accepts the first manifest checkpoint name and then refuses to replace or remove it", func() {
		ctx := context.Background()
		key := newContent(ctx, "set-once-mcp-content-")

		By("accepting the first publish")
		updateContentStatus(ctx, key, "", func(_ Gomega, c *storagev1alpha1.SnapshotContent) {
			c.Status.ManifestCheckpointName = "mcp-first"
		})

		By("refusing a later checkpoint from a second capture")
		updateContentStatus(ctx, key, "status.manifestCheckpointName is set-once: the published ManifestCheckpoint name cannot be replaced",
			func(_ Gomega, c *storagev1alpha1.SnapshotContent) {
				c.Status.ManifestCheckpointName = "mcp-second"
			})

		By("refusing removal — what a status replace by a writer without the field would do")
		updateContentStatus(ctx, key, "status.manifestCheckpointName is set-once: a published ManifestCheckpoint name cannot be removed",
			func(_ Gomega, c *storagev1alpha1.SnapshotContent) {
				c.Status.ManifestCheckpointName = ""
			})

		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.Status.ManifestCheckpointName).To(Equal("mcp-first"))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
	})

	It("accepts the first data binding, backfills uid and size, and refuses to move the identity", func() {
		ctx := context.Background()
		key := newContent(ctx, "set-once-data-content-")

		By("accepting a first publish that carries neither uid nor size")
		updateContentStatus(ctx, key, "", func(_ Gomega, c *storagev1alpha1.SnapshotContent) {
			c.Status.Data = &storagev1alpha1.SnapshotDataBinding{
				SourceRef: storagev1alpha1.SnapshotSubjectRef{
					APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-first", Namespace: "ns-first",
				},
				ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
					APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-first",
				},
			}
		})

		By("accepting both uid and size arriving in one patch — enrichment, not identity")
		updateContentStatus(ctx, key, "", func(g Gomega, c *storagev1alpha1.SnapshotContent) {
			g.Expect(c.Status.Data).NotTo(BeNil(), "the first publish must be observable before it is enriched")
			c.Status.Data.ArtifactRef.UID = types.UID("vsc-first-uid")
			c.Status.Data.SourceRef.UID = types.UID("pvc-first-uid")
			c.Status.Data.Size = "500Mi"
			c.Status.Data.VolumeMode = "Filesystem"
			c.Status.Data.StorageClassName = "sc-first"
		})

		const (
			artifactIdentityRule = "status.data.artifactRef identity (apiVersion, kind, name) is set-once"
			artifactUIDRule      = "status.data.artifactRef.uid is set-once"
			sourceIdentityRule   = "status.data.sourceRef identity (apiVersion, kind, name) is set-once"
			sourceNamespaceRule  = "status.data.sourceRef.namespace is part of the set-once identity"
			sourceUIDRule        = "status.data.sourceRef.uid is set-once"
		)
		for _, tc := range []struct {
			name      string
			rejection string
			mutate    func(*storagev1alpha1.SnapshotDataBinding)
		}{
			{name: "a different data artifact", rejection: artifactIdentityRule, mutate: func(d *storagev1alpha1.SnapshotDataBinding) { d.ArtifactRef.Name = "vsc-second" }},
			{name: "a different artifact kind", rejection: artifactIdentityRule, mutate: func(d *storagev1alpha1.SnapshotDataBinding) { d.ArtifactRef.Kind = "OtherArtifact" }},
			{name: "a published artifact uid replaced", rejection: artifactUIDRule, mutate: func(d *storagev1alpha1.SnapshotDataBinding) { d.ArtifactRef.UID = types.UID("vsc-second-uid") }},
			{name: "a published artifact uid dropped", rejection: artifactUIDRule, mutate: func(d *storagev1alpha1.SnapshotDataBinding) { d.ArtifactRef.UID = "" }},
			{name: "a different captured source", rejection: sourceIdentityRule, mutate: func(d *storagev1alpha1.SnapshotDataBinding) { d.SourceRef.Name = "pvc-second" }},
			{name: "the same source name in another namespace", rejection: sourceNamespaceRule, mutate: func(d *storagev1alpha1.SnapshotDataBinding) { d.SourceRef.Namespace = "ns-second" }},
			{name: "the source namespace dropped", rejection: sourceNamespaceRule, mutate: func(d *storagev1alpha1.SnapshotDataBinding) { d.SourceRef.Namespace = "" }},
			{name: "a published source uid replaced", rejection: sourceUIDRule, mutate: func(d *storagev1alpha1.SnapshotDataBinding) { d.SourceRef.UID = types.UID("pvc-second-uid") }},
		} {
			By("refusing " + tc.name)
			updateContentStatus(ctx, key, tc.rejection, func(g Gomega, c *storagev1alpha1.SnapshotContent) {
				g.Expect(c.Status.Data).NotTo(BeNil())
				tc.mutate(c.Status.Data)
			})
		}

		By("refusing removal of the whole binding")
		updateContentStatus(ctx, key, "status.data is set-once: a published data binding cannot be removed",
			func(g Gomega, c *storagev1alpha1.SnapshotContent) {
				g.Expect(c.Status.Data).NotTo(BeNil())
				c.Status.Data = nil
			})

		By("accepting volume metadata that lands later against the same identity")
		updateContentStatus(ctx, key, "", func(g Gomega, c *storagev1alpha1.SnapshotContent) {
			g.Expect(c.Status.Data).NotTo(BeNil())
			c.Status.Data.FsType = "ext4"
		})

		// The binding every rejected attempt left behind: the identity of the first publish, plus every
		// piece of enrichment that was allowed to land.
		Eventually(func(g Gomega) {
			data := publishedData(g, ctx, key)
			g.Expect(data.ArtifactRef.Name).To(Equal("vsc-first"))
			g.Expect(data.ArtifactRef.Kind).To(Equal("VolumeSnapshotContent"))
			g.Expect(data.ArtifactRef.UID).To(Equal(types.UID("vsc-first-uid")))
			g.Expect(data.SourceRef.Name).To(Equal("pvc-first"))
			g.Expect(data.SourceRef.Namespace).To(Equal("ns-first"))
			g.Expect(data.SourceRef.UID).To(Equal(types.UID("pvc-first-uid")))
			g.Expect(data.Size).To(Equal("500Mi"))
			g.Expect(data.FsType).To(Equal("ext4"))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
	})

	// The frozen-set rule itself (no add, remove, reorder, replace) is asserted in the
	// childrenSnapshotContentRefs frozen-set spec. What is asserted here is only the companion it lacked: a
	// published child set cannot be dropped by a writer that omits the field.
	It("refuses to remove a published child set", func() {
		ctx := context.Background()
		key := newContent(ctx, "set-once-children-content-")

		updateContentStatus(ctx, key, "", func(_ Gomega, c *storagev1alpha1.SnapshotContent) {
			c.Status.ChildrenSnapshotContentRefs = []storagev1alpha1.SnapshotContentChildRef{{Name: "child-first"}}
		})
		updateContentStatus(ctx, key, "status.childrenSnapshotContentRefs is frozen once set: a published child set cannot be removed",
			func(g Gomega, c *storagev1alpha1.SnapshotContent) {
				g.Expect(c.Status.ChildrenSnapshotContentRefs).NotTo(BeEmpty(), "the first publish must be observable before it is dropped")
				c.Status.ChildrenSnapshotContentRefs = nil
			})

		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.Status.ChildrenSnapshotContentRefs).To(HaveLen(1))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
	})

	It("keeps a Snapshot's first content binding and refuses to replace or remove it", func() {
		ctx := context.Background()
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "set-once-bound-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns.Name}}))
		})
		snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: "snap"}}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		key := client.ObjectKey{Namespace: ns.Name, Name: snap.Name}

		// The first publish is the binder's own: a Snapshot starts with an empty status, so set-once passes
		// through the transition that creates the binding.
		var bound string
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			bound = fresh.Status.BoundSnapshotContentName
			g.Expect(bound).NotTo(BeEmpty())
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed(), "the binder must be able to publish the first binding")

		updateSnapshotStatus := func(wantRejection, value string) {
			GinkgoHelper()
			Eventually(func(g Gomega) {
				fresh := &storagev1alpha1.Snapshot{}
				g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
				fresh.Status.BoundSnapshotContentName = value
				err := k8sClient.Status().Update(ctx, fresh)
				if wantRejection == "" {
					g.Expect(err).NotTo(HaveOccurred())
					return
				}
				g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected a CEL Invalid rejection, got %v", err)
				g.Expect(err.Error()).To(ContainSubstring(wantRejection))
			}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
		}

		By("refusing a re-point onto another content")
		updateSnapshotStatus("status.boundSnapshotContentName is set-once: the bound SnapshotContent name cannot be replaced", "some-other-content")

		By("refusing removal")
		updateSnapshotStatus("status.boundSnapshotContentName is set-once: the bound SnapshotContent name cannot be removed", "")

		By("accepting the binder's idempotent re-publish of the same name")
		updateSnapshotStatus("", bound)
	})

	// The regression the set-once binding must not break: a SnapshotContent that outlived its Snapshot is
	// recovered by re-pointing spec.snapshotRef at a freshly created subject. Nothing about that path writes
	// over a published binding — the surviving content keeps its artifacts, and the new Snapshot's status
	// starts empty, so its binding is a first publish.
	It("still allows a recycle-bin restore onto a freshly created Snapshot", func() {
		ctx := context.Background()
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "set-once-recycle-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns.Name}}))
		})

		content := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "set-once-recycle-content-"},
			Spec: storagev1alpha1.SnapshotContentSpec{
				DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
				SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
					APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
					Kind:       "Snapshot",
					Namespace:  ns.Name,
					Name:       "gone-snapshot",
					UID:        types.UID("11111111-1111-4111-8111-111111111111"),
				},
			},
		}
		Expect(k8sClient.Create(ctx, content)).To(Succeed())
		key := client.ObjectKey{Name: content.Name}
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: key.Name}}))
		})

		By("publishing the artifacts the surviving content carries into the bin")
		updateContentStatus(ctx, key, "", func(_ Gomega, c *storagev1alpha1.SnapshotContent) {
			c.Status.ManifestCheckpointName = "mcp-recycled"
			c.Status.BoundSnapshotDeleted = true
		})

		By("re-pointing spec.snapshotRef onto the recreated subject")
		newSnap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: "recreated"}}
		Expect(k8sClient.Create(ctx, newSnap)).To(Succeed())
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			fresh.Spec.SnapshotRef = &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "Snapshot",
				Namespace:  ns.Name,
				Name:       newSnap.Name,
				UID:        newSnap.UID,
			}
			g.Expect(k8sClient.Update(ctx, fresh)).To(Succeed())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed(), "the recycle-bin carve-out must survive the set-once rules")

		By("keeping the recovered artifact pointer intact")
		Eventually(func(g Gomega) {
			fresh := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, key, fresh)).To(Succeed())
			g.Expect(fresh.Status.ManifestCheckpointName).To(Equal("mcp-recycled"))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("letting the recreated Snapshot publish its own first binding")
		Eventually(func(g Gomega) {
			s := &storagev1alpha1.Snapshot{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: newSnap.Name}, s)).To(Succeed())
			g.Expect(s.Status.BoundSnapshotContentName).NotTo(BeEmpty())
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})
