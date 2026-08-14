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

package genericbinder

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	deckhousev1alpha1 "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/deckhouseio/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// A DataImport lives only as long as the import needs it: storage-foundation reaps it on its own idle TTL.
// "No DataImport" is therefore the PERMANENT end state of every imported data leaf, not a transient
// pre-creation window, and reconcileGenericImport must tell the two apart by the content's published data
// leg. The tests below pin both halves plus the degradations that used to be unreachable behind the
// unconditional 5s poll: without them the leaf silently stops mirroring content.status.data (the descriptor
// d8 reads) and stops reporting ContentMissing/Deleting, while burning ~12 reconciles/min forever.
//
// Ready is deliberately NOT asserted as an output of the binder anywhere here: the single post-bind writer
// of the steady-state Snapshot.Ready is the aggregator (snapshotcontent/ready_mirror.go). The leaf fixtures
// carry Ready=True as an INPUT, so that the function's !Ready poll tail (a legitimate requeue while the
// content is still converging) cannot be mistaken for the defect under test.

const importTestScratchClass = "sc-import"

// importTestScheme adds the SVDM DataImport/DataImportList (read cross-service as unstructured, so the fake
// client needs them registered for the reverse-lookup List) and the ObjectKeeper used by the root path.
func importTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := domainTestScheme(t)
	diGVK := schema.GroupVersionKind{Group: "storage-foundation.deckhouse.io", Version: "v1alpha1", Kind: "DataImport"}
	scheme.AddKnownTypeWithName(diGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(diGVK.GroupVersion().WithKind("DataImportList"), &unstructured.UnstructuredList{})
	if err := deckhousev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add deckhouse.io scheme: %v", err)
	}
	return scheme
}

// importTestLeaf builds an imported data leaf as the binder sees it after d8 uploaded it: spec.mode=Import,
// a child->parent ownerRef, and a bound SnapshotContent. It carries Ready=True because that is the steady
// state the aggregator leaves it in — see the file comment.
func importTestLeaf(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	obj.SetGroupVersionKind(domainSnapshotGVK)
	obj.SetNamespace(domainTestNS)
	obj.SetName(domainTestSnap)
	obj.SetUID(types.UID(domainTestSnapUID))
	obj.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: domainSnapshotGVK.GroupVersion().String(),
		Kind:       domainSnapshotGVK.Kind,
		Name:       domainTestParentSnap,
		UID:        types.UID(domainTestParentSnapUID),
	}})
	if err := unstructured.SetNestedField(obj.Object, string(storagev1alpha1.SnapshotModeImport), "spec", "mode"); err != nil {
		t.Fatalf("set spec.mode Import: %v", err)
	}
	if err := unstructured.SetNestedField(obj.Object, domainTestContent, "status", "boundSnapshotContentName"); err != nil {
		t.Fatalf("set boundSnapshotContentName: %v", err)
	}
	importTestMarkReady(t, obj)
	return obj
}

// importTestMarkReady stamps Ready=True on a leaf, mimicking the aggregator's post-bind Ready mirror.
func importTestMarkReady(t *testing.T, obj *unstructured.Unstructured) {
	t.Helper()
	like, err := snapshot.ExtractSnapshotLike(obj)
	if err != nil {
		t.Fatalf("extract snapshot like: %v", err)
	}
	snapshot.SetCondition(like, snapshot.ConditionReady, metav1.ConditionTrue, snapshot.ReasonCompleted, "imported")
}

// importTestCheckpoint is the reconstructed ManifestCheckpoint the import upload created for the leaf; the
// binder waits for it before touching the data leg.
func importTestCheckpoint() *ssv1alpha1.ManifestCheckpoint {
	return &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: usecase.ReconstructedManifestCheckpointName(types.UID(domainTestSnapUID), "")},
	}
}

// importTestContentData is the descriptor the aggregator publishes for an imported leaf: the source is the
// leaf itself (there is no live source PVC) and storageClassName comes from the DataImport's
// spec.storageParams.storageClassName.
func importTestContentData() *storagev1alpha1.SnapshotDataBinding {
	return &storagev1alpha1.SnapshotDataBinding{
		SourceRef: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: domainSnapshotGVK.GroupVersion().String(), Kind: domainSnapshotGVK.Kind,
			Name: domainTestSnap, Namespace: domainTestNS, UID: types.UID(domainTestSnapUID),
		},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: domainTestVSCName,
		},
		StorageClassName: importTestScratchClass,
		Size:             "10Gi",
	}
}

func importTestReconcile(t *testing.T, objs ...client.Object) (ctrl.Result, client.Client, error) {
	t.Helper()
	scheme := importTestScheme(t)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(domainSnapshotStatusStub(), &storagev1alpha1.SnapshotContent{}).
		WithObjects(objs...).
		Build()
	r := newDomainMirrorReconcileController(t, cl, scheme)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: domainTestNS, Name: domainTestSnap}})
	return res, cl, err
}

func importTestFreshLeaf(t *testing.T, cl client.Client) *unstructured.Unstructured {
	t.Helper()
	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(domainSnapshotGVK)
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: domainTestNS, Name: domainTestSnap}, fresh); err != nil {
		t.Fatalf("get import leaf: %v", err)
	}
	return fresh
}

func importTestLeafReady(t *testing.T, cl client.Client) *metav1.Condition {
	t.Helper()
	like, err := snapshot.ExtractSnapshotLike(importTestFreshLeaf(t, cl))
	if err != nil {
		t.Fatalf("extract fresh snapshot like: %v", err)
	}
	return snapshot.GetCondition(like, snapshot.ConditionReady)
}

// Steady state of a FINISHED import: the DataImport is gone (idle-TTL reaped) but the content carries its
// published data leg. The binder must stop polling and still do its leaf-facing work — mirror
// content.status.data (what d8 reads on export) and the content's excludedRefs. Before the fix this state
// returned a 5s requeue before any of it, forever, so a class published on the content after the DataImport
// died could never reach the leaf.
func TestReconcileGenericImport_SteadyStateWithoutDataImportMirrorsAndStopsPolling(t *testing.T) {
	content := domainTestLeafContentOwnedByParent(importTestContentData())
	content.Status.ExcludedRefs = []storagev1alpha1.ExcludedObjectRef{{
		APIVersion: "v1", Kind: "Secret", Name: "skipped-secret",
	}}

	res, cl, err := importTestReconcile(t,
		importTestLeaf(t), domainTestParentSnapshot(t), domainTestParentContentObj(), content, importTestCheckpoint())
	if err != nil {
		t.Fatalf("Reconcile must not fail in the DataImport-less steady state: %v", err)
	}
	if res.RequeueAfter != 0 || res.Requeue {
		t.Fatalf("a finished import must stop polling, got %+v", res)
	}

	fresh := importTestFreshLeaf(t, cl)
	if sc, _, _ := unstructured.NestedString(fresh.Object, "status", "data", "storageClassName"); sc != importTestScratchClass {
		t.Fatalf("status.data.storageClassName = %q, want %q (the mirror did not run)", sc, importTestScratchClass)
	}
	if art, _, _ := unstructured.NestedString(fresh.Object, "status", "data", "artifactRef", "name"); art != domainTestVSCName {
		t.Fatalf("status.data.artifactRef.name = %q, want %q", art, domainTestVSCName)
	}
	excluded, found, _ := unstructured.NestedSlice(fresh.Object, "status", "excludedRefs")
	if !found || len(excluded) != 1 {
		t.Fatalf("content excludedRefs must be mirrored onto the leaf, got found=%v len=%d", found, len(excluded))
	}
}

// Genuinely pending import: no DataImport AND no published data leg on the content — d8 may not have created
// the DataImport yet, or the artifact is not produced. This must keep the 5s poll, since nothing wakes the
// binder (there is no DataImport watch). Regression guard for the fix above.
func TestReconcileGenericImport_PendingWithoutDataImportKeepsPolling(t *testing.T) {
	res, cl, err := importTestReconcile(t,
		importTestLeaf(t), domainTestParentSnapshot(t), domainTestParentContentObj(),
		domainTestLeafContentOwnedByParent(nil), importTestCheckpoint())
	if err != nil {
		t.Fatalf("Reconcile on a pending import must not fail: %v", err)
	}
	if res.RequeueAfter != importContentPollInterval {
		t.Fatalf("a pending import must keep polling, got RequeueAfter=%v want %v", res.RequeueAfter, importContentPollInterval)
	}
	if _, found, _ := unstructured.NestedMap(importTestFreshLeaf(t, cl).Object, "status", "data"); found {
		t.Fatalf("status.data must not be written while the content has published none")
	}
}

// Degradation 1, unreachable before the fix: the bound content is being deleted. With the DataImport gone the
// binder used to return before checkConsistencyAndSetReady, so the leaf kept advertising a stale Ready=True.
func TestReconcileGenericImport_ContentDeletingDegradesLeafWithoutDataImport(t *testing.T) {
	deleting := domainTestLeafContentOwnedByParent(importTestContentData())
	now := metav1.Now()
	deleting.SetDeletionTimestamp(&now)
	deleting.SetFinalizers([]string{"test.state-snapshotter.deckhouse.io/hold"})

	res, cl, err := importTestReconcile(t,
		importTestLeaf(t), domainTestParentSnapshot(t), domainTestParentContentObj(), deleting, importTestCheckpoint())
	if err != nil {
		t.Fatalf("Reconcile must not fail while the bound content terminates: %v", err)
	}
	if res.RequeueAfter != 0 || res.Requeue {
		t.Fatalf("the degradation is watch-driven, not polled, got %+v", res)
	}
	got := importTestLeafReady(t, cl)
	if got == nil || got.Status != metav1.ConditionFalse || got.Reason != snapshot.ReasonDeleting {
		t.Fatalf("want Ready=False/%s on the leaf, got %+v", snapshot.ReasonDeleting, got)
	}
}

// Degradation 2, unreachable before the fix for a DIFFERENT reason: the bound content is gone. The import
// path returned the Get's NotFound as a reconcile error, so an imported leaf whose content was deleted
// error-requeued with backoff behind a stale Ready=True and never reported ContentMissing (the capture path
// has always degraded honestly here).
func TestReconcileGenericImport_ContentMissingDegradesLeafWithoutDataImport(t *testing.T) {
	// The leaf's bound content is intentionally absent; only the parent chain exists so the ownerRef
	// resolution succeeds and the import path reaches the content Get.
	res, cl, err := importTestReconcile(t,
		importTestLeaf(t), domainTestParentSnapshot(t), domainTestParentContentObj(), importTestCheckpoint())
	if err != nil {
		t.Fatalf("a gone bound content must degrade, not error-requeue: %v", err)
	}
	if res.RequeueAfter != 0 || res.Requeue {
		t.Fatalf("a gone content is terminal-shaped for the leaf; nothing to poll for, got %+v", res)
	}
	got := importTestLeafReady(t, cl)
	if got == nil || got.Status != metav1.ConditionFalse || got.Reason != snapshot.ReasonContentMissing {
		t.Fatalf("want Ready=False/%s on the leaf, got %+v", snapshot.ReasonContentMissing, got)
	}
}

// The degradation above is keyed on IsNotFound ALONE. A transient read failure on the bound content is not
// evidence that the content is gone, so it must stay a reconcile error (retried with backoff) and must not
// be laundered into a terminal-shaped Ready=False/ContentMissing on the leaf.
func TestReconcileGenericImport_TransientContentGetErrorStillRequeues(t *testing.T) {
	scheme := importTestScheme(t)
	apiDown := apierrors.NewServiceUnavailable("simulated apiserver outage")
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(domainSnapshotStatusStub(), &storagev1alpha1.SnapshotContent{}).
		WithObjects(importTestLeaf(t), domainTestParentSnapshot(t), domainTestParentContentObj(),
			domainTestLeafContentOwnedByParent(importTestContentData()), importTestCheckpoint()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*storagev1alpha1.SnapshotContent); ok && key.Name == domainTestContent {
					return apiDown
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := newDomainMirrorReconcileController(t, cl, scheme)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: domainTestNS, Name: domainTestSnap}})
	if err == nil {
		t.Fatalf("a transient content read failure must requeue as an error, not degrade the leaf")
	}
	if got := importTestLeafReady(t, cl); got == nil || got.Status != metav1.ConditionTrue {
		t.Fatalf("a transient read failure must not rewrite the leaf's Ready, got %+v", got)
	}
}

// Root guard for the same NotFound branch. A ROOT import snapshot's Ready belongs to the namespace Snapshot
// orchestrator, which holds exactly this case as ImportPending; the binder must keep returning the error
// there instead of co-writing a second Ready, or the two writers flap the condition forever. (The root is
// modelled with the domain kind — isRoot is structural, "no snapshot-suffixed ownerRef", so the code path is
// the same one the unified root Snapshot takes.)
func TestReconcileGenericImport_RootWithMissingContentDoesNotCoWriteReady(t *testing.T) {
	root := importTestLeaf(t)
	root.SetOwnerReferences(nil) // no parent snapshot => IsRootSnapshot
	// A root that is already bound to a content which no longer exists.
	res, cl, err := importTestReconcile(t, root, importTestCheckpoint())
	if err == nil {
		t.Fatalf("a root with a missing content must keep erroring (backoff), got result %+v", res)
	}
	got := importTestLeafReady(t, cl)
	if got == nil || got.Status != metav1.ConditionTrue {
		t.Fatalf("the binder must not write the root's Ready; it is the orchestrator's (got %+v)", got)
	}
}
