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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// A DataImport that stops without producing an artifact — its spec.ttl idle window elapsed mid-upload, or it
// failed — used to be invisible from the snapshot tree: the leaf reported the generic "waiting for the data
// leg" and the root reported only WHICH child it was waiting for, while the explanation lived on a third
// object that is reaped soon after. The cause now travels in the condition MESSAGES, with the reasons left
// exactly as they were (DataCapturePending on the leaf, ChildrenPending on the root) because a stalled import
// does not close the node: the client is free to create another DataImport for it.
//
// Envtest earns its keep over the unit coverage here: the messages are asserted where a user reads them —
// after the aggregation has published them through a real CRD and the Ready mirror has copied them onto the
// namespaced Snapshots — and the settled state is observed while the real controller keeps re-reconciling it,
// which is the only way to see whether re-deriving the cause rewrites the status.
var _ = Describe("Integration: a stalled import explains itself on the leaf and on the root", func() {
	const (
		importSnapshotKind = "TestSnapshot"
		importSnapshotAPI  = "test.deckhouse.io/v1alpha1"
		importScratchClass = "sc-import-cause"
		dataImportName     = "import-cause-di"
	)

	testSnapshotGVK := schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: importSnapshotKind}

	BeforeEach(func() {
		ctx := context.Background()
		// The reverse-lookup lists DataImports on every pass; without the CRD the List fails before the
		// branch under test is reached.
		integrationEnsureDataImportCRD(ctx, cfg)
		markKindDataBearingForSpec(importSnapshotKind)
	})

	It("names the stalled DataImport on the leaf, quotes it on the root, and settles without rewriting status", func() {
		ctx := context.Background()

		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ss-import-cause-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns := nsObj.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})

		// The root of the fixture is a snapshot with no domain half at all (no capture phase, no
		// ManifestCaptureRequest, no declared children): every projection of the aggregator is then a no-op on
		// it, so the seeded root status below is stable and the spec observes the child aggregation only.
		By("creating the root snapshot and its SnapshotContent")
		root := &unstructured.Unstructured{}
		root.SetGroupVersionKind(testSnapshotGVK)
		root.SetNamespace(ns)
		root.SetName("import-cause-root")
		Expect(k8sClient.Create(ctx, root)).To(Succeed())

		rootContent := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "import-cause-root-content-"},
			Spec: storagev1alpha1.SnapshotContentSpec{
				DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyDelete,
				SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
					APIVersion: importSnapshotAPI, Kind: importSnapshotKind,
					Name: root.GetName(), Namespace: ns, UID: root.GetUID(),
				},
			},
		}
		Expect(k8sClient.Create(ctx, rootContent)).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(),
				&storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: rootContent.Name}}))
		})
		bindSnapshotToContent(ctx, testSnapshotGVK, client.ObjectKeyFromObject(root), rootContent.Name)

		By("creating the imported data leaf as the client leaves it: import mode, bound to its own content")
		leaf := &unstructured.Unstructured{}
		leaf.SetGroupVersionKind(testSnapshotGVK)
		leaf.SetNamespace(ns)
		leaf.SetName("import-cause-leaf")
		Expect(unstructured.SetNestedField(leaf.Object, string(storagev1alpha1.SnapshotModeImport), "spec", "mode")).To(Succeed())
		Expect(k8sClient.Create(ctx, leaf)).To(Succeed())
		leafUID := leaf.GetUID()
		Expect(leafUID).NotTo(BeEmpty())
		// Guard the fixture itself: a pruned spec.mode would turn this into a capture leaf and the whole spec
		// would assert nothing.
		Eventually(func(g Gomega) {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(testSnapshotGVK)
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(leaf), live)).To(Succeed())
			mode, _, _ := unstructured.NestedString(live.Object, "spec", "mode")
			g.Expect(mode).To(Equal(string(storagev1alpha1.SnapshotModeImport)), "spec.mode must survive the CRD schema")
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		leafContent := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName:    "import-cause-leaf-content-",
				OwnerReferences: []metav1.OwnerReference{snaphelpersContentOwnerRefForTest(rootContent)},
			},
			Spec: storagev1alpha1.SnapshotContentSpec{
				DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyDelete,
				SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
					APIVersion: importSnapshotAPI, Kind: importSnapshotKind,
					Name: leaf.GetName(), Namespace: ns, UID: leafUID,
				},
			},
		}
		Expect(k8sClient.Create(ctx, leafContent)).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(),
				&storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: leafContent.Name}}))
		})
		bindSnapshotToContent(ctx, testSnapshotGVK, client.ObjectKeyFromObject(leaf), leafContent.Name)

		// A second waiting child, so the spec covers WHICH child the root quotes and not merely "the only
		// one". It waits on its own manifest leg (no checkpoint), i.e. for an unrelated reason. Its
		// spec.snapshotRef anchors on the already-created root (a live existence anchor, not a real child
		// graph): the Ready mirror only writes onto a snapshot that is bound to THIS content, so pointing at
		// the root leaves the root's own mirror untouched.
		By("creating a second waiting child, held by its own manifest leg")
		otherContent := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName:    "import-cause-other-content-",
				OwnerReferences: []metav1.OwnerReference{snaphelpersContentOwnerRefForTest(rootContent)},
			},
			Spec: storagev1alpha1.SnapshotContentSpec{
				DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyDelete,
				SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
					APIVersion: importSnapshotAPI, Kind: importSnapshotKind,
					Name: root.GetName(), Namespace: ns, UID: root.GetUID(),
				},
			},
		}
		Expect(k8sClient.Create(ctx, otherContent)).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(),
				&storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: otherContent.Name}}))
		})

		By("linking both children into the root's frozen edge set, the leaf first")
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			live := &storagev1alpha1.SnapshotContent{}
			if err := mgr.GetAPIReader().Get(ctx, client.ObjectKey{Name: rootContent.Name}, live); err != nil {
				return err
			}
			live.Status.ChildrenSnapshotContentRefs = []storagev1alpha1.SnapshotContentChildRef{
				{Name: leafContent.Name}, {Name: otherContent.Name},
			}
			live.Status.ManifestCheckpointName = "mcp-" + rootContent.Name
			return k8sClient.Status().Update(ctx, live)
		})).To(Succeed())

		By("publishing the manifest legs of the root and of the leaf")
		var rootContentUID, leafContentUID string
		Eventually(func(g Gomega) {
			for name, into := range map[string]*string{rootContent.Name: &rootContentUID, leafContent.Name: &leafContentUID} {
				live := &storagev1alpha1.SnapshotContent{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, live)).To(Succeed())
				*into = string(live.UID)
				g.Expect(*into).NotTo(BeEmpty())
			}
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
		readyCheckpoint := func(mcpName, contentName, contentUID string) {
			GinkgoHelper()
			ensureManifestCheckpointStatus(ctx, mcpName, contentName, contentUID, func(m *ssv1alpha1.ManifestCheckpoint) {
				meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
					Type:    ssv1alpha1.ManifestCheckpointConditionTypeReady,
					Status:  metav1.ConditionTrue,
					Reason:  ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
					Message: "checkpoint ready",
				})
			})
			DeferCleanup(func() {
				_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(),
					&ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: mcpName}}))
			})
		}
		readyCheckpoint("mcp-"+rootContent.Name, rootContent.Name, rootContentUID)
		// The leaf's manifest leg is UPLOADED, not captured: the checkpoint carries the deterministic
		// reconstructed name keyed to the leaf UID, and the aggregator publishes the pointer itself.
		readyCheckpoint(usecase.ReconstructedManifestCheckpointName(leafUID, ""), leafContent.Name, leafContentUID)

		By("creating the leaf's DataImport and expiring it before it produced an artifact")
		di := &unstructured.Unstructured{}
		di.SetGroupVersionKind(dataImportGVKForTest)
		di.SetNamespace(ns)
		di.SetName(dataImportName)
		Expect(unstructured.SetNestedMap(di.Object, map[string]interface{}{
			"ttl":                  "24h",
			"waitForFirstConsumer": false,
			"mode":                 snapshot.DataImportModePopulateData,
			"snapshotRef": map[string]interface{}{
				"apiVersion": importSnapshotAPI, "kind": importSnapshotKind, "name": leaf.GetName(),
			},
			"storageParams": map[string]interface{}{
				"storageClassName": importScratchClass,
				"size":             "10Gi",
				"volumeMode":       string(corev1.PersistentVolumeFilesystem),
			},
		}, "spec")).To(Succeed())
		Expect(k8sClient.Create(ctx, di)).To(Succeed())
		// No status.data is ever published on it: that is the whole point — the upload never finished.
		Eventually(func() error {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(dataImportGVKForTest)
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: dataImportName}, live); err != nil {
				return err
			}
			if err := unstructured.SetNestedField(live.Object, snapshot.DataImportPhaseExpired, "status", "phase"); err != nil {
				return err
			}
			return k8sClient.Status().Update(ctx, live)
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		expectedCause := fmt.Sprintf(
			"DataImport %s/%s is in phase Expired and produced no data artifact: "+
				"its spec.ttl idle window elapsed before the upload finished; "+
				"create a new DataImport for this node to retry", ns, dataImportName)
		expectedRootQuote := fmt.Sprintf(
			"child SnapshotContent %s is not ready yet: leaf=%s reason=%s message=%s",
			leafContent.Name, leafContent.Name, snapshot.ReasonDataCapturePending, expectedCause)

		By("the leaf must stay pending with the cause in its message, and keep the pending reason")
		Eventually(func(g Gomega) {
			cond := contentReadyCondition(g, leafContent.Name)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(snapshot.ReasonDataCapturePending),
				"a stalled import must not introduce a terminal or a new reason")
			g.Expect(cond.Message).To(Equal(expectedCause))
		}, 90*time.Second, 200*time.Millisecond).Should(Succeed())

		By("the leaf Snapshot must carry the same cause, since that is where a user looks")
		Eventually(func(g Gomega) {
			cond := snapshotReadyCondition(g, testSnapshotGVK, client.ObjectKeyFromObject(leaf))
			g.Expect(cond.Reason).To(Equal(snapshot.ReasonDataCapturePending))
			g.Expect(cond.Message).To(Equal(expectedCause))
		}, 90*time.Second, 200*time.Millisecond).Should(Succeed())

		By("the root must quote the first waiting child that has a reason and still name the other one")
		Eventually(func(g Gomega) {
			cond := contentReadyCondition(g, rootContent.Name)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(snapshot.ReasonChildrenPending),
				"the root stays non-terminal: the client may create another DataImport")
			g.Expect(cond.Message).To(Equal(fmt.Sprintf(
				"waiting for child snapshot contents: 0/2 ready; pending: %s, %s; %s",
				leafContent.Name, otherContent.Name, expectedRootQuote)))
		}, 90*time.Second, 200*time.Millisecond).Should(Succeed())

		By("the root Snapshot must carry the same aggregate message")
		Eventually(func(g Gomega) {
			cond := snapshotReadyCondition(g, testSnapshotGVK, client.ObjectKeyFromObject(root))
			g.Expect(cond.Reason).To(Equal(snapshot.ReasonChildrenPending))
			g.Expect(cond.Message).To(ContainSubstring(expectedRootQuote))
		}, 90*time.Second, 200*time.Millisecond).Should(Succeed())

		// The controller re-reconciles a pending node roughly twice a second, and it re-derives this cause on
		// every pass. Were the cause built from anything that moves — a timestamp, an elapsed duration, a byte
		// count — each pass would write the status again, forever, on an object nobody is waiting for any more.
		// Nothing else writes these four objects at this point, so an unchanged resourceVersion is exactly the
		// statement "the settled tree is quiet".
		By("the settled tree must stop being written to")
		type watched struct {
			what string
			read func() string
		}
		contentRV := func(name string) func() string {
			return func() string {
				live := &storagev1alpha1.SnapshotContent{}
				Expect(mgr.GetAPIReader().Get(ctx, client.ObjectKey{Name: name}, live)).To(Succeed())
				return live.GetResourceVersion()
			}
		}
		snapshotRV := func(key client.ObjectKey) func() string {
			return func() string {
				live := &unstructured.Unstructured{}
				live.SetGroupVersionKind(testSnapshotGVK)
				Expect(mgr.GetAPIReader().Get(ctx, key, live)).To(Succeed())
				return live.GetResourceVersion()
			}
		}
		observed := []watched{
			{"leaf SnapshotContent", contentRV(leafContent.Name)},
			{"root SnapshotContent", contentRV(rootContent.Name)},
			{"leaf Snapshot", snapshotRV(client.ObjectKeyFromObject(leaf))},
			{"root Snapshot", snapshotRV(client.ObjectKeyFromObject(root))},
		}
		baseline := make([]string, len(observed))
		for i, o := range observed {
			baseline[i] = o.read()
		}
		polls := 0
		Consistently(func(g Gomega) {
			polls++
			for i, o := range observed {
				g.Expect(o.read()).To(Equal(baseline[i]),
					"%s was written again while the tree was settled", o.what)
			}
		}, 4*time.Second, 250*time.Millisecond).Should(Succeed())
		// The window is the spec's own limit: it covers ~8 reconcile passes of the 500 ms pending poll. A cause
		// that only moves on a slower clock than that would pass here and has to be caught by the exact-literal
		// unit coverage of the message instead.
		GinkgoWriter.Printf("import cause: %d objects checked over %d polls, resourceVersions %v\n",
			len(observed), polls, baseline)
	})
})

// bindSnapshotToContent publishes status.boundSnapshotContentName on a snapshot, which is the writer switch
// the Ready mirror keys on: without it the aggregator's mirror declines to write the owning snapshot.
func bindSnapshotToContent(ctx context.Context, gvk schema.GroupVersionKind, key client.ObjectKey, contentName string) {
	GinkgoHelper()
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(gvk)
		if err := mgr.GetAPIReader().Get(ctx, key, live); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(live.Object, contentName, "status", "boundSnapshotContentName"); err != nil {
			return err
		}
		return k8sClient.Status().Update(ctx, live)
	})).To(Succeed())
}

// contentReadyCondition reads a SnapshotContent's Ready condition, failing the enclosing assertion when the
// content or the condition is not there yet.
func contentReadyCondition(g Gomega, name string) *metav1.Condition {
	live := &storagev1alpha1.SnapshotContent{}
	g.Expect(k8sClient.Get(context.Background(), client.ObjectKey{Name: name}, live)).To(Succeed())
	cond := meta.FindStatusCondition(live.Status.Conditions, snapshot.ConditionReady)
	g.Expect(cond).NotTo(BeNil(), "SnapshotContent %s has no Ready condition yet", name)
	return cond
}

// snapshotReadyCondition reads the mirrored Ready condition off a namespaced snapshot CR.
func snapshotReadyCondition(g Gomega, gvk schema.GroupVersionKind, key client.ObjectKey) *metav1.Condition {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(gvk)
	g.Expect(k8sClient.Get(context.Background(), key, live)).To(Succeed())
	like, err := snapshot.ExtractSnapshotLike(live)
	g.Expect(err).NotTo(HaveOccurred())
	cond := snapshot.GetCondition(like, snapshot.ConditionReady)
	g.Expect(cond).NotTo(BeNil(), "snapshot %s has no Ready condition yet", key)
	return cond
}

// The counterpart of the spec above: a leaf whose data leg IS published must not be described as stuck, no
// matter what phase its DataImport happens to have reached. The order of the two checks is the whole point —
// the artifact decides, the phase does not — because the import's obligation to the snapshot node ends with
// the artifact, and a node that already owns a durable one is finished. A phase-first implementation would
// knock a captured leaf out of Ready on an event that concerns nothing but the import's own lifetime.
//
// Label("isolated"): the published leg names a VolumeSnapshotContent, and readiness is only satisfied by
// reading that live artifact — the cluster-scoped CRD for it is deliberately absent from the shared pass.
var _ = Describe("Integration: a published data leg outlives its DataImport's phase", Serial, Ordered, Label("isolated"), func() {
	const (
		importSnapshotKind = "TestSnapshot"
		importSnapshotAPI  = "test.deckhouse.io/v1alpha1"
		importScratchClass = "sc-import-outlives"
		importObservedFs   = "ext4"
	)

	var (
		testSnapshotGVK = schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: importSnapshotKind}
		vscGVK          = schema.GroupVersionKind{Group: snapshot.CSISnapshotGroup, Version: snapshot.CSISnapshotVersion, Kind: snapshot.KindVolumeSnapshotContent}
	)

	BeforeAll(func() {
		ctx := context.Background()
		pr7InstallCSIClassAndContentCRDs(ctx)
		integrationEnsureDataImportCRD(ctx, cfg)
		markKindDataBearingForSpec(importSnapshotKind)
	})

	It("keeps the leaf Ready when its DataImport is Expired but its artifact was produced", func() {
		ctx := context.Background()

		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ss-import-outlives-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns := nsObj.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})

		By("creating the import-mode data leaf")
		leaf := &unstructured.Unstructured{}
		leaf.SetGroupVersionKind(testSnapshotGVK)
		leaf.SetNamespace(ns)
		leaf.SetName("outlives-leaf")
		Expect(unstructured.SetNestedField(leaf.Object, string(storagev1alpha1.SnapshotModeImport), "spec", "mode")).To(Succeed())
		Expect(k8sClient.Create(ctx, leaf)).To(Succeed())
		var leafUID types.UID
		Eventually(func(g Gomega) {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(testSnapshotGVK)
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(leaf), live)).To(Succeed())
			mode, _, _ := unstructured.NestedString(live.Object, "spec", "mode")
			g.Expect(mode).To(Equal(string(storagev1alpha1.SnapshotModeImport)), "spec.mode must survive the CRD schema")
			leafUID = live.GetUID()
			g.Expect(leafUID).NotTo(BeEmpty())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("creating the VolumeSnapshotContent the import produced, ready and sized")
		vscName := "outlives-vsc-" + ns
		vsc := &unstructured.Unstructured{}
		vsc.SetGroupVersionKind(vscGVK)
		vsc.SetName(vscName)
		Expect(unstructured.SetNestedMap(vsc.Object, map[string]interface{}{
			"deletionPolicy":    "Delete",
			"driver":            "test.csi.storage.k8s.io",
			"source":            map[string]interface{}{"snapshotHandle": "handle-" + vscName},
			"volumeSnapshotRef": map[string]interface{}{"name": "vs-" + vscName, "namespace": ns},
		}, "spec")).To(Succeed())
		Expect(k8sClient.Create(ctx, vsc)).To(Succeed())
		DeferCleanup(func() {
			gone := &unstructured.Unstructured{}
			gone.SetGroupVersionKind(vscGVK)
			gone.SetName(vscName)
			_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), gone))
		})
		vscHasStatusSubresource := crdServesStatusSubresource(ctx, "volumesnapshotcontents."+snapshot.CSISnapshotGroup, snapshot.CSISnapshotVersion)
		var vscUID types.UID
		Eventually(func() error {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(vscGVK)
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: vscName}, live); err != nil {
				return err
			}
			vscUID = live.GetUID()
			if err := unstructured.SetNestedField(live.Object, true, "status", "readyToUse"); err != nil {
				return err
			}
			if err := unstructured.SetNestedField(live.Object, int64(524288000), "status", "restoreSize"); err != nil {
				return err
			}
			if vscHasStatusSubresource {
				return k8sClient.Status().Update(ctx, live)
			}
			return k8sClient.Update(ctx, live)
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("creating the DataImport that produced it, then expiring the DataImport itself")
		di := &unstructured.Unstructured{}
		di.SetGroupVersionKind(dataImportGVKForTest)
		di.SetNamespace(ns)
		di.SetName("outlives-di")
		Expect(unstructured.SetNestedMap(di.Object, map[string]interface{}{
			"ttl":                  "24h",
			"waitForFirstConsumer": false,
			"mode":                 snapshot.DataImportModePopulateData,
			"snapshotRef": map[string]interface{}{
				"apiVersion": importSnapshotAPI, "kind": importSnapshotKind, "name": leaf.GetName(),
			},
			"storageParams": map[string]interface{}{
				"storageClassName": importScratchClass,
				"size":             "10Gi",
				"volumeMode":       string(corev1.PersistentVolumeFilesystem),
			},
		}, "spec")).To(Succeed())
		Expect(k8sClient.Create(ctx, di)).To(Succeed())
		Eventually(func() error {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(dataImportGVKForTest)
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: di.GetName()}, live); err != nil {
				return err
			}
			if err := unstructured.SetNestedMap(live.Object, map[string]interface{}{
				"apiVersion": snapshot.CSISnapshotAPIVersion,
				"kind":       snapshot.KindVolumeSnapshotContent,
				"name":       vscName,
				"uid":        string(vscUID),
			}, "status", "data", "artifactRef"); err != nil {
				return err
			}
			if err := unstructured.SetNestedField(live.Object, string(corev1.PersistentVolumeFilesystem), "status", "volumeMode"); err != nil {
				return err
			}
			if err := unstructured.SetNestedField(live.Object, importObservedFs, "status", "data", "fsType"); err != nil {
				return err
			}
			// The phase this spec turns on: terminal for the IMPORT, irrelevant to the node that already
			// owns the artifact above.
			if err := unstructured.SetNestedField(live.Object, snapshot.DataImportPhaseExpired, "status", "phase"); err != nil {
				return err
			}
			return k8sClient.Status().Update(ctx, live)
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("creating the leaf's SnapshotContent and its uploaded manifest leg")
		leafContent := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "outlives-leaf-content-"},
			Spec: storagev1alpha1.SnapshotContentSpec{
				DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyDelete,
				SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
					APIVersion: importSnapshotAPI, Kind: importSnapshotKind,
					Name: leaf.GetName(), Namespace: ns, UID: leafUID,
				},
			},
		}
		Expect(k8sClient.Create(ctx, leafContent)).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(),
				&storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: leafContent.Name}}))
		})
		bindSnapshotToContent(ctx, testSnapshotGVK, client.ObjectKeyFromObject(leaf), leafContent.Name)
		var leafContentUID string
		Eventually(func(g Gomega) {
			live := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: leafContent.Name}, live)).To(Succeed())
			leafContentUID = string(live.UID)
			g.Expect(leafContentUID).NotTo(BeEmpty())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
		mcpName := usecase.ReconstructedManifestCheckpointName(leafUID, "")
		ensureManifestCheckpointStatus(ctx, mcpName, leafContent.Name, leafContentUID, func(m *ssv1alpha1.ManifestCheckpoint) {
			meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
				Type:    ssv1alpha1.ManifestCheckpointConditionTypeReady,
				Status:  metav1.ConditionTrue,
				Reason:  ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
				Message: "checkpoint ready",
			})
		})
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(),
				&ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: mcpName}}))
		})

		By("the leaf must reach Ready=True and carry the published data leg")
		Eventually(func(g Gomega) {
			cond := contentReadyCondition(g, leafContent.Name)
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
				"an Expired DataImport must not unsettle a leaf whose artifact it already produced")
			g.Expect(cond.Reason).To(Equal(snapshot.ReasonCompleted))

			live := &storagev1alpha1.SnapshotContent{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: leafContent.Name}, live)).To(Succeed())
			g.Expect(live.Status.Data).NotTo(BeNil())
			g.Expect(live.Status.Data.ArtifactRef.Name).To(Equal(vscName))
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())

		By("the leaf Snapshot must mirror that readiness, not a stall")
		Eventually(func(g Gomega) {
			cond := snapshotReadyCondition(g, testSnapshotGVK, client.ObjectKeyFromObject(leaf))
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal(snapshot.ReasonCompleted))
		}, 120*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})

// markKindDataBearingForSpec makes the suite's content controller treat a snapshot kind as a DATA leaf for
// the duration of the calling spec, and puts the previous value back afterwards.
//
// The mark is needed at all because that controller is the only writer of these contents: to an unmarked kind
// an import leaf is a structural node whose empty data leg reads as Ready, which is the opposite of what a
// pending data leg must report. In production the same value comes per kind from the CustomSnapshotDefinition.
//
// The restore is what keeps it from reaching other specs: the registry is shared for the whole suite process,
// and a neighbouring spec drives a controller of its own over the SAME kind and asserts a status.data state
// (deliberately rewound to a pre-fix shape) that a second data-leg writer would heal away. Leaving the mark
// set therefore broke that spec depending on the order Ginkgo happened to pick. Specs never run concurrently
// within a process, so a save/restore pair confines the change to one spec.
func markKindDataBearingForSpec(kind string) {
	GinkgoHelper()
	Expect(integrationContentController).NotTo(BeNil(), "content controller must be built in BeforeSuite")
	previous := integrationContentController.GVKRegistry.RequiresDataArtifact(kind)
	integrationContentController.MarkRequiresDataArtifact(kind, true)
	DeferCleanup(func() {
		integrationContentController.MarkRequiresDataArtifact(kind, previous)
	})
}
