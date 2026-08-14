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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// An imported leaf outlives its DataImport: storage-foundation reaps it on its own idle TTL, so "no
// DataImport" is this leaf's PERMANENT state. The binder used to read that as "the import has not started
// yet" and return a 5s requeue before doing any leaf-facing work, which froze the leaf's status.data mirror
// — the descriptor d8 reads — against whatever it held at the moment the DataImport died.
//
// The scenario is chosen to be RED on the pre-fix code: asserting "the leaf is still Ready" or "its class
// did not disappear" would pass without the fix too (Ready is written by the aggregator, and the mirror had
// already run while the DataImport was alive). So it drives a CHANGE through instead: with no DataImport in
// the namespace, the content's published class is rewritten and the leaf must pick the new value up. Before
// the fix the mirror is unreachable and the value never moves.
//
// Envtest earns its keep here over the unit tests: the leaf status.data patch goes through a real
// structural CRD (a wire-shape drift is pruned by the apiserver instead of being accepted by a fake client),
// and the DataImport reverse-lookup runs as a real List against a served CRD with zero matching objects.
// Requeue timings stay asserted at unit level, where they are deterministic — this spec asserts the outcome.
var _ = Describe("Integration: an imported leaf keeps mirroring after its DataImport is reaped", func() {
	const (
		importSnapshotKind = "TestSnapshot"
		importSnapshotAPI  = "test.deckhouse.io/v1alpha1"
		classBeforeReap    = "sc-import-staged"
		classAfterFix      = "sc-import-corrected"
	)

	testSnapshotGVK := schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: importSnapshotKind}

	BeforeEach(func() {
		// The reverse-lookup lists DataImports even when there are none; without the CRD the List fails
		// before the branch under test is reached.
		integrationEnsureDataImportCRD(context.Background(), cfg)
	})

	// newImportBinder builds a binder that treats the test snapshot kind as data-bearing (in production the
	// CSD sets requiresDataArtifact) and maps it onto the common SnapshotContent. It is reconciled directly
	// rather than registered with the manager: the suite manager runs no binder for this GVK, and a
	// registered one would race asynchronously with the deterministic steps below.
	newImportBinder := func() *controllers.GenericSnapshotBinderController {
		GinkgoHelper()
		binder, err := controllers.NewGenericSnapshotBinderController(
			mgr.GetClient(), mgr.GetAPIReader(), scheme, testCfg, nil,
		)
		Expect(err).NotTo(HaveOccurred())
		commonContentGVK := unifiedbootstrap.CommonSnapshotContentGVK()
		Expect(binder.GVKRegistry.RegisterSnapshotContentMapping(
			testSnapshotGVK.Kind, testSnapshotGVK.GroupVersion().String(),
			commonContentGVK.Kind, commonContentGVK.GroupVersion().String(),
		)).To(Succeed())
		binder.SnapshotGVKs = []schema.GroupVersionKind{testSnapshotGVK}
		binder.MarkRequiresDataArtifact(testSnapshotGVK.Kind, true)
		return binder
	}

	It("carries a content-side change onto the leaf with no DataImport in the namespace", func() {
		ctx := context.Background()

		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ss-import-steady-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns := nsObj.Name
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})

		By("creating the parent snapshot and its SnapshotContent")
		parent := &unstructured.Unstructured{}
		parent.SetGroupVersionKind(testSnapshotGVK)
		parent.SetNamespace(ns)
		parent.SetName("import-steady-parent")
		Expect(unstructured.SetNestedField(parent.Object, string(storagev1alpha1.SnapshotModeImport), "spec", "mode")).To(Succeed())
		Expect(k8sClient.Create(ctx, parent)).To(Succeed())

		parentContent := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "import-steady-parent-content-"},
			Spec: storagev1alpha1.SnapshotContentSpec{
				DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyDelete,
				SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
					APIVersion: importSnapshotAPI, Kind: importSnapshotKind,
					Name: parent.GetName(), Namespace: ns, UID: parent.GetUID(),
				},
			},
		}
		Expect(k8sClient.Create(ctx, parentContent)).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(),
				&storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: parentContent.Name}}))
		})

		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(testSnapshotGVK)
			if err := mgr.GetAPIReader().Get(ctx, client.ObjectKeyFromObject(parent), live); err != nil {
				return err
			}
			if err := unstructured.SetNestedField(live.Object, parentContent.Name, "status", "boundSnapshotContentName"); err != nil {
				return err
			}
			return k8sClient.Status().Update(ctx, live)
		})).To(Succeed())
		parentUID := parent.GetUID()

		By("creating the imported data leaf as d8 leaves it: import mode, child->parent ownerRef")
		leaf := &unstructured.Unstructured{}
		leaf.SetGroupVersionKind(testSnapshotGVK)
		leaf.SetNamespace(ns)
		leaf.SetName("import-steady-leaf")
		leaf.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: importSnapshotAPI, Kind: importSnapshotKind, Name: parent.GetName(), UID: parentUID,
		}})
		Expect(unstructured.SetNestedField(leaf.Object, string(storagev1alpha1.SnapshotModeImport), "spec", "mode")).To(Succeed())
		Expect(k8sClient.Create(ctx, leaf)).To(Succeed())
		leafUID := leaf.GetUID()
		Expect(leafUID).NotTo(BeEmpty())
		// Guard the fixture itself: a pruned spec.mode would turn this into a capture leaf and the whole
		// spec would assert nothing.
		Eventually(func(g Gomega) {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(testSnapshotGVK)
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(leaf), live)).To(Succeed())
			mode, _, _ := unstructured.NestedString(live.Object, "spec", "mode")
			g.Expect(mode).To(Equal(string(storagev1alpha1.SnapshotModeImport)), "spec.mode must survive the CRD schema")
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("creating the leaf's SnapshotContent with the data leg the aggregator published")
		leafContent := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName:    "import-steady-leaf-content-",
				OwnerReferences: []metav1.OwnerReference{snaphelpersContentOwnerRefForTest(parentContent)},
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
		publishContentClass := func(class string) {
			GinkgoHelper()
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				live := &storagev1alpha1.SnapshotContent{}
				if err := mgr.GetAPIReader().Get(ctx, client.ObjectKey{Name: leafContent.Name}, live); err != nil {
					return err
				}
				live.Status.Data = &storagev1alpha1.SnapshotDataBinding{
					SourceRef: storagev1alpha1.SnapshotSubjectRef{
						APIVersion: importSnapshotAPI, Kind: importSnapshotKind,
						Name: leaf.GetName(), Namespace: ns, UID: leafUID,
					},
					ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
						APIVersion: snapshot.CSISnapshotAPIVersion,
						Kind:       snapshot.KindVolumeSnapshotContent,
						Name:       "import-steady-vsc",
					},
					StorageClassName: class,
					Size:             "10Gi",
				}
				return k8sClient.Status().Update(ctx, live)
			})).To(Succeed())
		}
		publishContentClass(classBeforeReap)

		By("binding the leaf to its content and stamping the Ready the aggregator owns")
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(testSnapshotGVK)
			if err := mgr.GetAPIReader().Get(ctx, client.ObjectKeyFromObject(leaf), live); err != nil {
				return err
			}
			if err := unstructured.SetNestedField(live.Object, leafContent.Name, "status", "boundSnapshotContentName"); err != nil {
				return err
			}
			like, err := snapshot.ExtractSnapshotLike(live)
			if err != nil {
				return err
			}
			snapshot.SetCondition(like, snapshot.ConditionReady, metav1.ConditionTrue, snapshot.ReasonCompleted, "imported")
			return k8sClient.Status().Update(ctx, live)
		})).To(Succeed())

		By("creating the reconstructed ManifestCheckpoint the import upload produced")
		mcp := &ssv1alpha1.ManifestCheckpoint{
			ObjectMeta: metav1.ObjectMeta{Name: usecase.ReconstructedManifestCheckpointName(leafUID, "")},
		}
		Expect(k8sClient.Create(ctx, mcp)).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(),
				&ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: mcp.Name}}))
		})

		binder := newImportBinder()
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: leaf.GetName()}}
		leafClass := func(g Gomega) string {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(testSnapshotGVK)
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(leaf), live)).To(Succeed())
			class, _, _ := unstructured.NestedString(live.Object, "status", "data", "storageClassName")
			return class
		}

		// No DataImport is ever created in this namespace: this leaf's own was reaped by its idle TTL long
		// before the reconcile below. That is the whole point — it must not be read as "not started yet".
		By("reconciling with no DataImport: the leaf must still mirror the published data leg")
		Eventually(func(g Gomega) {
			_, err := binder.Reconcile(ctx, req)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(leafClass(g)).To(Equal(classBeforeReap))
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed())

		By("rewriting the class on the content and reconciling again: the change must reach the leaf")
		publishContentClass(classAfterFix)
		Eventually(func(g Gomega) {
			_, err := binder.Reconcile(ctx, req)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(leafClass(g)).To(Equal(classAfterFix))
		}, 60*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})

// snaphelpersContentOwnerRefForTest builds the lifecycle ownerRef the binder expects a child content to
// already carry (parent SnapshotContent, controller=true), so EnsureLifecycleOwnerRef is a no-op and the
// reconcile reaches the data leg on its first pass.
func snaphelpersContentOwnerRefForTest(parent *storagev1alpha1.SnapshotContent) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "SnapshotContent",
		Name:       parent.Name,
		UID:        parent.UID,
		Controller: &controller,
	}
}
