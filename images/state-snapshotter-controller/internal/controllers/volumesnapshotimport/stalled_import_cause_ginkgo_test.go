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
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// The single Ginkgo entry point of this package. The table-driven tests around it use plain testing.
func TestVolumeSnapshotImportGinkgo(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "volumesnapshotimport Suite")
}

// An imported VolumeSnapshot leaf whose DataImport reached Expired or Failed without producing an artifact
// used to sit silently pending: the explanation existed only on the DataImport, which is a different object
// and, once reaped, no object at all. The binder now writes it onto the VolumeSnapshot the operator is
// already looking at — while deliberately continuing to poll, because the client may create another
// DataImport for the same leaf and nothing here watches DataImport.
var _ = Describe("an import VolumeSnapshot whose DataImport will not deliver", func() {
	const (
		expiredCause = "DataImport team-a/di-stalled is in phase Expired and produced no data artifact: " +
			"its spec.ttl idle window elapsed before the upload finished; " +
			"create a new DataImport for this node to retry"
	)

	// stalledDataImport targets the leaf VolumeSnapshot by spec.snapshotRef (the reverse-lookup key) and
	// publishes no status.data: the shape of an import that never finished its upload.
	stalledDataImport := func(phase string) *unstructured.Unstructured {
		di := &unstructured.Unstructured{}
		di.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "storage-foundation.deckhouse.io", Version: "v1alpha1", Kind: "DataImport"})
		di.SetNamespace(vsImportNS)
		di.SetName("di-stalled")
		Expect(unstructured.SetNestedField(di.Object, snapshotpkg.DataImportModePopulateData, "spec", "mode")).To(Succeed())
		Expect(unstructured.SetNestedMap(di.Object, map[string]interface{}{
			"apiVersion": snapshotpkg.CSISnapshotAPIVersion,
			"kind":       snapshotpkg.KindVolumeSnapshot,
			"name":       vsImportName,
		}, "spec", "snapshotRef")).To(Succeed())
		Expect(unstructured.SetNestedField(di.Object, phase, "status", "phase")).To(Succeed())
		return di
	}

	volumeSnapshotError := func(cl client.Client) string {
		GinkgoHelper()
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(csiVolumeSnapshotGVK)
		Expect(cl.Get(context.Background(), client.ObjectKey{Namespace: vsImportNS, Name: vsImportName}, fresh)).To(Succeed())
		message, _, _ := unstructured.NestedString(fresh.Object, "status", "error", "message")
		return message
	}

	It("records the cause on status.error and keeps polling for a retry", func() {
		res, cl, err := vsImportReconcile(vsImportLeaf(""), vsImportLeafContent(nil),
			stalledDataImport(snapshotpkg.DataImportPhaseExpired))

		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(importPollInterval),
			"a stalled import is not the end of the leaf: another DataImport may be created, and nothing else would notice it")
		Expect(volumeSnapshotError(cl)).To(Equal(expiredCause))
	})

	It("writes nothing while the import can still deliver", func() {
		res, cl, err := vsImportReconcile(vsImportLeaf(""), vsImportLeafContent(nil),
			stalledDataImport("Completed"))

		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(importPollInterval))
		Expect(volumeSnapshotError(cl)).To(BeEmpty(),
			"a converging import must not be described as stuck")
	})

	// The other half of a RECOVERABLE cause: it has to be taken back. Every other message this controller
	// writes to status.error describes a leaf that will not come back, so nothing here ever removed one —
	// and the message this spec's subject writes advises creating a new DataImport, which is exactly what the
	// client does next. Left standing it would tell an operator to redo what already worked.
	Context("after the client retries", func() {
		// standingCause is the message an earlier, expired import left on the leaf.
		standingCause := func(vs *unstructured.Unstructured) *unstructured.Unstructured {
			Expect(unstructured.SetNestedField(vs.Object, expiredCause, "status", "error", "message")).To(Succeed())
			Expect(unstructured.SetNestedField(vs.Object, "2026-08-19T00:00:00Z", "status", "error", "time")).To(Succeed())
			return vs
		}

		It("takes the cause back as soon as a replacement import is under way", func() {
			res, cl, err := vsImportReconcile(standingCause(vsImportLeaf("")), vsImportLeafContent(nil),
				stalledDataImport("Running"))

			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(importPollInterval))
			Expect(volumeSnapshotError(cl)).To(BeEmpty(),
				"the leaf is uploading again; the message of the import it replaced is spent")
		})

		It("takes the cause back once the leaf owns its artifact", func() {
			// The end state of a successful retry: the artifact is bound, the aggregator published the data
			// leg, and the DataImport has been reaped. This exit is the one such a leaf actually reaches —
			// the pending branch above is never entered again — so without a removal here the leaf would
			// carry "produced no data artifact" while holding one.
			res, cl, err := vsImportReconcile(
				standingCause(vsImportLeaf(vsImportVSCName)), vsImportLeafContent(vsImportPublishedData()))

			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeZero(), "a finished import stops polling")
			Expect(volumeSnapshotError(cl)).To(BeEmpty())
		})

		It("never describes a leaf that already owns its data leg as stuck", func() {
			// A second import may be created and expire against a leaf that is already complete (nothing
			// stops a client from asking twice). Its phase says nothing about a node whose artifact is
			// durable, so the aggregator gates its own cause on exactly this — and so must this branch, which
			// only knows whether the DataImport produced an artifact, not whether the leaf has one.
			res, cl, err := vsImportReconcile(vsImportLeaf(""), vsImportLeafContent(vsImportPublishedData()),
				stalledDataImport(snapshotpkg.DataImportPhaseExpired))

			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(importPollInterval))
			Expect(volumeSnapshotError(cl)).To(BeEmpty())
		})

		It("removes a cause that outlived the data leg it complained about", func() {
			res, cl, err := vsImportReconcile(
				standingCause(vsImportLeaf("")), vsImportLeafContent(vsImportPublishedData()),
				stalledDataImport(snapshotpkg.DataImportPhaseExpired))

			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(importPollInterval))
			Expect(volumeSnapshotError(cl)).To(BeEmpty())
		})
	})

	// status.error is one channel for every import diagnosis, and which of them a later pass may take back is
	// decided by WHERE each is diagnosed. Three are settled above the branch that revisits the field, so
	// arriving there refutes them; one is settled below it and therefore survives. These specs pin both sides
	// by RECONCILING each fault into place rather than by writing a copy of its wording.
	Context("a diagnosis of another kind", func() {
		// deliveringDataImport is an import that DID produce an artifact, so the pass walks past the pending
		// branch and reaches what lies beyond it.
		deliveringDataImport := func(artifactKind, artifactName string) *unstructured.Unstructured {
			di := stalledDataImport("Completed")
			di.SetName("di-delivered")
			Expect(unstructured.SetNestedMap(di.Object, map[string]interface{}{
				"apiVersion": snapshotpkg.CSISnapshotAPIVersion,
				"kind":       artifactKind,
				"name":       artifactName,
			}, "status", "data", "artifactRef")).To(Succeed())
			return di
		}

		// faultThenReplacement records the diagnosis of a first pass, then swaps the DataImports for a single
		// converging replacement — what a client retrying leaves behind — and runs a second pass over the same
		// state. It returns the message after each pass.
		faultThenReplacement := func(imports []*unstructured.Unstructured, extra ...client.Object) (afterFault, afterReplacement string) {
			GinkgoHelper()
			ctx := context.Background()
			objects := make([]client.Object, 0, len(imports)+len(extra))
			for _, di := range imports {
				objects = append(objects, di)
			}
			cl := vsImportFixture(vsImportLeaf(""), vsImportLeafContent(nil), interceptor.Funcs{},
				append(objects, extra...)...)
			r := &Controller{Client: cl, APIReader: cl}

			_, err := r.Reconcile(ctx, vsImportRequest())
			Expect(err).NotTo(HaveOccurred())
			afterFault = volumeSnapshotError(cl)
			Expect(afterFault).NotTo(BeEmpty(), "the first pass must record a diagnosis for this spec to be about anything")

			for _, di := range imports {
				Expect(cl.Delete(ctx, di)).To(Succeed())
			}
			Expect(cl.Create(ctx, stalledDataImport("Running"))).To(Succeed())

			res, err := r.Reconcile(ctx, vsImportRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(importPollInterval), "the replacement import is still converging")
			return afterFault, volumeSnapshotError(cl)
		}

		It("survives a converging replacement import: a checkpoint carrying no PersistentVolumeClaim", func() {
			ctx := context.Background()
			// The Retain handoff reads the produced artifact before the manifests are resolved, so the
			// VolumeSnapshotContent has to exist for the pass to reach the fault at all.
			vsc := &unstructured.Unstructured{}
			vsc.SetGroupVersionKind(csiVolumeSnapshotContentGVK)
			vsc.SetName(vsImportVSCName)

			cl := vsImportFixture(vsImportLeaf(""), vsImportLeafContent(nil), interceptor.Funcs{},
				deliveringDataImport(snapshotpkg.KindVolumeSnapshotContent, vsImportVSCName), vsc)
			r := &Controller{Client: cl, APIReader: cl}

			// The checkpoint is Ready but carries no chunks, so it yields no PersistentVolumeClaim manifest —
			// a fault of the CHECKPOINT, which another import cannot mend, and which is diagnosed below the
			// branch that would otherwise take it back.
			mcp := &ssv1alpha1.ManifestCheckpoint{}
			mcpKey := client.ObjectKey{Name: usecase.ReconstructedManifestCheckpointName(types.UID(vsImportUID), "")}
			Expect(cl.Get(ctx, mcpKey, mcp)).To(Succeed())
			meta.SetStatusCondition(&mcp.Status.Conditions, metav1.Condition{
				Type:   ssv1alpha1.ManifestCheckpointConditionTypeReady,
				Status: metav1.ConditionTrue,
				Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
			})
			// A plain Update, not Status(): this fixture registers a status subresource for the objects the
			// controller writes, and the checkpoint is not one of them, so its status travels with the object.
			Expect(cl.Update(ctx, mcp)).To(Succeed())

			_, err := r.Reconcile(ctx, vsImportRequest())
			Expect(err).NotTo(HaveOccurred())
			checkpointFault := volumeSnapshotError(cl)
			Expect(checkpointFault).To(ContainSubstring("PersistentVolumeClaim"),
				"the first pass must record the checkpoint diagnosis this spec defends")
			Expect(isCheckpointManifestFault(checkpointFault)).To(BeTrue(),
				"the message the code writes must be the one the recogniser knows")

			Expect(cl.Delete(ctx, deliveringDataImport(snapshotpkg.KindVolumeSnapshotContent, vsImportVSCName))).To(Succeed())
			Expect(cl.Create(ctx, stalledDataImport("Running"))).To(Succeed())

			res, err := r.Reconcile(ctx, vsImportRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(importPollInterval), "the replacement import is still converging")
			Expect(volumeSnapshotError(cl)).To(Equal(checkpointFault),
				"a fault no import can fix must outlive the import that ran into it")
		})

		It("is taken back by a converging replacement import: an artifact the binding cannot represent", func() {
			// A client can create an import of the wrong shape, get a PersistentVolume artifact the legacy
			// binding cannot represent, then delete it and create a correct one. The complaint is about THAT
			// import, and the pass that reaches the pending branch has already established there is no such
			// artifact now, so keeping the message would leave an error on a leaf that goes on to succeed.
			afterFault, afterReplacement := faultThenReplacement(
				[]*unstructured.Unstructured{deliveringDataImport("PersistentVolume", "pv-1")})

			Expect(afterFault).To(ContainSubstring("PersistentVolume"))
			Expect(isCheckpointManifestFault(afterFault)).To(BeFalse())
			Expect(afterReplacement).To(BeEmpty())
		})

		It("is taken back by a converging replacement import: two DataImports claiming the leaf", func() {
			// The window where an expired import has not been deleted yet and its replacement already exists:
			// the reverse-lookup fails closed on the pair. Once the extra one is gone the leaf converges, and
			// the pass that gets to the pending branch has itself resolved exactly one import — so the
			// ambiguity is disproved by being here.
			first := stalledDataImport("Running")
			second := stalledDataImport(snapshotpkg.DataImportPhaseExpired)
			second.SetName("di-expired")

			afterFault, afterReplacement := faultThenReplacement(
				[]*unstructured.Unstructured{first, second})

			Expect(afterFault).To(ContainSubstring("exactly one is required"))
			Expect(isCheckpointManifestFault(afterFault)).To(BeFalse())
			Expect(afterReplacement).To(BeEmpty())
		})
	})

	// One definition per sentence, and the recogniser derived from it: these entries prove each format is
	// matched by the pattern built from that same format, and that a message of another class is not.
	DescribeTable("recognising a checkpoint-manifest fault",
		func(message string, want bool) {
			Expect(isCheckpointManifestFault(message)).To(Equal(want))
		},
		Entry("a checkpoint with no PersistentVolumeClaim",
			fmt.Sprintf(checkpointManifestNoPVCFormat, "nss-mcp-abc"), true),
		Entry("a checkpoint with more than one PersistentVolumeClaim",
			fmt.Sprintf(checkpointManifestMultiplePVCFormat, "nss-mcp-abc"), true),
		Entry("a checkpoint whose chunks cannot be read",
			fmt.Sprintf(checkpointManifestUnreadableFormat, "nss-mcp-abc", usecase.ErrCorruptManifestChunk), true),
		Entry("a stalled-import cause, which a later pass refutes",
			expiredCause, false),
		Entry("no message at all", "", false),
	)

	// Taking a message back on every pass would be the same status churn the cause itself was built to avoid,
	// so both removals must be no-ops when there is nothing to remove. Counted rather than inferred: this
	// asserts the number of status writes a settled pass performs, which is what "quiet" means.
	It("writes nothing at all while there is no cause standing", func() {
		patches := 0
		cl := vsImportFixture(vsImportLeaf(""), vsImportLeafContent(nil), interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string,
				obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				patches++
				return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			},
		}, stalledDataImport("Running"))
		r := &Controller{Client: cl, APIReader: cl}

		const passes = 4
		for range passes {
			res, err := r.Reconcile(context.Background(), vsImportRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(importPollInterval))
		}

		GinkgoWriter.Printf("stalled-cause upkeep: %d status patches over %d settled passes\n", patches, passes)
		Expect(patches).To(BeZero(), "a leaf with no cause standing must not be patched at all")
		Expect(volumeSnapshotError(cl)).To(BeEmpty())
	})
})

// The deny-list is only as good as the producer and the recogniser sharing one text, and asserting that the
// recogniser matches its own format constant proves nothing about what the code writes. So these specs call
// the PRODUCER on a checkpoint that carries each fault, and require the message it returns to be recognised.
// A future reword that bypasses the shared constant then fails here instead of silently making that fault
// erasable by the pending branch — which is the defect this gate exists for.
var _ = Describe("the checkpoint-manifest faults the resolver states", func() {
	const checkpointName = "nss-mcp-fault-fixture"

	// encodedChunkPayload is the wire form of a chunk: base64(gzip(json array of objects)), plus the SHA256 of
	// the compressed bytes the resolver verifies. Written out here rather than borrowed because the writer
	// that produces it in production is not exported, and the format is three well-known steps.
	encodedChunkPayload := func(objects ...map[string]interface{}) (data, checksum string) {
		GinkgoHelper()
		raw, err := json.Marshal(objects)
		Expect(err).NotTo(HaveOccurred())
		var compressed bytes.Buffer
		gz := gzip.NewWriter(&compressed)
		_, err = gz.Write(raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(gz.Close()).To(Succeed())
		sum := sha256.Sum256(compressed.Bytes())
		return base64.StdEncoding.EncodeToString(compressed.Bytes()), hex.EncodeToString(sum[:])
	}

	manifestObject := func(kind, name string) map[string]interface{} {
		return map[string]interface{}{
			"apiVersion": corePVCAPIVersion,
			"kind":       kind,
			"metadata":   map[string]interface{}{"name": name, "namespace": vsImportNS},
		}
	}

	// resolveWith runs the real resolver over a checkpoint whose single chunk carries the given wire payload.
	// An empty checksum is passed for a payload that is not meant to decode at all.
	resolveWith := func(data, checksum string) string {
		GinkgoHelper()
		chunkName := checkpointName + "-chunk-0"
		mcp := &ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: checkpointName}}
		mcp.Status.Chunks = []ssv1alpha1.ChunkInfo{{Name: chunkName, Index: 0, Checksum: checksum}}
		chunk := &ssv1alpha1.ManifestCheckpointContentChunk{
			ObjectMeta: metav1.ObjectMeta{Name: chunkName},
			Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
				CheckpointName: checkpointName,
				Index:          0,
				Data:           data,
			},
		}
		cl := fake.NewClientBuilder().WithScheme(vsImportScheme()).WithObjects(mcp, chunk).Build()
		r := &Controller{Client: cl, APIReader: cl}

		pvc, terminalMessage, err := r.resolveImportedOrphanPVC(context.Background(), mcp)
		Expect(err).NotTo(HaveOccurred(), "a fault of the stored manifests is terminal, not a transient error")
		Expect(pvc).To(BeNil())
		Expect(terminalMessage).NotTo(BeEmpty())
		Expect(terminalMessage).To(ContainSubstring(checkpointName), "the diagnosis must name the checkpoint")
		return terminalMessage
	}

	It("states a recognisable fault for a checkpoint carrying more than one PersistentVolumeClaim", func() {
		message := resolveWith(encodedChunkPayload(
			manifestObject(kindPersistentVolumeClaim, "bk-pvc-a"),
			manifestObject(kindPersistentVolumeClaim, "bk-pvc-b")))

		Expect(message).To(ContainSubstring("more than one"))
		Expect(isCheckpointManifestFault(message)).To(BeTrue(),
			"the resolver must state this fault in the form the recogniser protects from removal")
	})

	It("states a recognisable fault for a checkpoint whose chunk cannot be read", func() {
		// Not base64 at all: the stored bytes are bad, so no retry of the same chunk can succeed.
		message := resolveWith("!! not base64 !!", "")

		Expect(message).To(ContainSubstring("unreadable"))
		Expect(isCheckpointManifestFault(message)).To(BeTrue(),
			"the resolver must state this fault in the form the recogniser protects from removal")
	})

	It("states a recognisable fault for a checkpoint carrying no PersistentVolumeClaim at all", func() {
		message := resolveWith(encodedChunkPayload(manifestObject("ConfigMap", "not-a-pvc")))

		Expect(message).To(ContainSubstring("no PersistentVolumeClaim"))
		Expect(isCheckpointManifestFault(message)).To(BeTrue(),
			"the resolver must state this fault in the form the recogniser protects from removal")
	})
})
