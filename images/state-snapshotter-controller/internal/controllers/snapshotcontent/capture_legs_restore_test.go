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
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

const strayMCRName = "mcr-stray"

func captureLegsStrayMCR() *ssv1alpha1.ManifestCaptureRequest {
	mcr := &ssv1alpha1.ManifestCaptureRequest{ObjectMeta: metav1.ObjectMeta{Namespace: projTestNS, Name: strayMCRName}}
	mcr.Status.CheckpointName = "mcp-other"
	return mcr
}

func TestReconcileOwnerCaptureLegs_RestoreFromDurablePointer(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)
	owner := captureLegsOwner(clSnapGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))
	rec := events.NewFakeRecorder(8)
	registerSnapshotContentMetrics()
	before := testutil.ToFloat64(captureStateRestoredTotal)

	var falseLegs []string
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped(clMCPName, captureLegsData()), captureLegsReadyOwnedMCP()).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if u, ok := obj.(*unstructured.Unstructured); ok {
					for _, leg := range []string{"manifestCaptured", "dataCaptured"} {
						v, found, _ := unstructured.NestedBool(u.Object, "status", "captureState", "commonController", leg)
						if found && !v {
							falseLegs = append(falseLegs, leg)
						}
					}
				}
				return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := captureLegsController(cl, clSnapGVK, true)
	r.Recorder = rec

	if _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil)); err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}
	if got, found := captureLegsOwnerLatch(t, cl, clSnapGVK, "manifestCaptured"); !found || !got {
		t.Fatalf("want manifestCaptured=true from the published checkpoint, got found=%v value=%v", found, got)
	}
	if got, found := captureLegsOwnerLatch(t, cl, clSnapGVK, "dataCaptured"); !found || !got {
		t.Fatalf("want dataCaptured=true from published status.data, got found=%v value=%v", found, got)
	}
	if len(falseLegs) != 0 {
		t.Fatalf("restore must not write an intermediate false, got %v", falseLegs)
	}
	if delta := testutil.ToFloat64(captureStateRestoredTotal) - before; delta != 1 {
		t.Fatalf("one confirmed restore of commonController is +1, got delta=%v", delta)
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, EventReasonCaptureStateRestored) {
			t.Fatalf("event %q must use reason %s", ev, EventReasonCaptureStateRestored)
		}
		if !strings.Contains(ev, "manifestCaptured") || !strings.Contains(ev, "dataCaptured") {
			t.Fatalf("event message must list restored legs, got %q", ev)
		}
	default:
		t.Fatal("expected CaptureStateRestored event")
	}

	if _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil)); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if delta := testutil.ToFloat64(captureStateRestoredTotal) - before; delta != 1 {
		t.Fatalf("a no-op reconcile must not increment again, got delta=%v", delta)
	}
}

func TestReconcileOwnerCaptureLegs_RestoreWaitsForBoundAndPlanned(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bound string
		phase string
	}{
		{name: "not bound", bound: "other", phase: string(storagev1alpha1.SnapshotCapturePhasePlanned)},
		{name: "before Planned", bound: projTestContent, phase: string(storagev1alpha1.SnapshotCapturePhasePlanning)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := captureLegsScheme(t)
			owner := captureLegsOwner(clSnapGVK, tc.bound, "", "", tc.phase)
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(owner).
				WithObjects(owner, captureLegsContentTyped(clMCPName, captureLegsData()), captureLegsReadyOwnedMCP()).
				Build()
			r := captureLegsController(cl, clSnapGVK, true)
			if _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil)); err != nil {
				t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
			}
			if _, found := captureLegsOwnerLatch(t, cl, clSnapGVK, "manifestCaptured"); found {
				t.Fatalf("core must not restore latch without bound+Planned")
			}
		})
	}
}

func TestReconcileOwnerCaptureLegs_ReapMismatchedMCRWhenNameSurvived(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)
	owner := captureLegsOwner(clSnapGVK, projTestContent, strayMCRName, "", string(storagev1alpha1.SnapshotCapturePhasePlanned))
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped(clMCPName, nil), captureLegsReadyOwnedMCP(), captureLegsStrayMCR()).
		Build()
	r := captureLegsController(cl, clSnapGVK, false)

	if _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil)); err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}
	if got, found := captureLegsOwnerLatch(t, cl, clSnapGVK, "manifestCaptured"); !found || !got {
		t.Fatalf("want manifestCaptured=true, got found=%v value=%v", found, got)
	}
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	err := cl.Get(ctx, client.ObjectKey{Namespace: projTestNS, Name: strayMCRName}, mcr)
	if err == nil {
		t.Fatal("mismatched MCR named on the owner must be reaped")
	}
	if client.IgnoreNotFound(err) != nil {
		t.Fatalf("get stray MCR: %v", err)
	}
}

func TestReconcileOwnerCaptureLegs_OrphanMCRLivesWhenNameWiped(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)
	owner := captureLegsOwner(clSnapGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped(clMCPName, nil), captureLegsReadyOwnedMCP(), captureLegsStrayMCR()).
		Build()
	r := captureLegsController(cl, clSnapGVK, false)

	if _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil)); err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}
	if got, found := captureLegsOwnerLatch(t, cl, clSnapGVK, "manifestCaptured"); !found || !got {
		t.Fatalf("want manifestCaptured=true, got found=%v value=%v", found, got)
	}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: projTestNS, Name: strayMCRName}, &ssv1alpha1.ManifestCaptureRequest{}); err != nil {
		t.Fatalf("orphan MCR with no name on the owner must live until owner delete, got %v", err)
	}
}

func TestReconcileOwnerCaptureLegs_RestoreSkippedWhenSchemaPrunesField(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)
	owner := captureLegsOwner(clSnapGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))
	rec := events.NewFakeRecorder(8)
	registerSnapshotContentMetrics()
	before := testutil.ToFloat64(captureStateRestoredTotal)

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped(clMCPName, nil), captureLegsReadyOwnedMCP()).
		Build()
	apiReader := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetName() == clOwnerName {
				unstructured.RemoveNestedField(u.Object, "status", "captureState", "commonController")
			}
			return nil
		},
	})
	r := &SnapshotContentController{Client: base, APIReader: apiReader, GVKRegistry: snapshot.NewGVKRegistry(), Recorder: rec}
	r.MarkDomainCaptureKind(clSnapGVK)

	requeue, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil))
	if err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}
	if delta := testutil.ToFloat64(captureStateRestoredTotal) - before; delta != 0 {
		t.Fatalf("a pruned field is not a restore, got delta=%v", delta)
	}
	select {
	case ev := <-rec.Events:
		if strings.Contains(ev, EventReasonCaptureStateRestored) {
			t.Fatalf("pruned restore must not emit CaptureStateRestored, got %q", ev)
		}
	default:
	}

	// The half that matters to the owner. Not counting a pruned write as a restore is bookkeeping; what the
	// durable pointer buys is that this pass never publishes the leg as false. A false here is the domain's
	// cue to capture the same manifests again, against a cluster that has moved on since the checkpoint this
	// content already points at — so an absent leg (which every domain reads as "wait") is the only correct
	// outcome, and the pass has to come back rather than carry on.
	if v, found := captureLegsOwnerLatch(t, base, clSnapGVK, "manifestCaptured"); found && !v {
		t.Fatal("the durable checkpoint proves the manifest leg captured; a pass that could not restore it must not declare it false")
	}
	if !requeue {
		t.Fatal("an unrestored leg must requeue: the leg is still undeclared and nothing else will come back for it")
	}
}

func TestReconcileOwnerCaptureLegs_PublishedDataLatchesAnyDomainKind(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)
	owner := captureLegsOwner(clSnapGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped("", captureLegsData())).
		Build()
	r := captureLegsController(cl, clSnapGVK, true)

	if _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil)); err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}
	if got, found := captureLegsOwnerLatch(t, cl, clSnapGVK, "dataCaptured"); !found || !got {
		t.Fatalf("published status.data must latch dataCaptured for any data-artifact kind, got found=%v value=%v", found, got)
	}
}

func TestReconcileOwnerCaptureLegs_RestoreDoesNotNeedLiveMCR(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)
	owner := captureLegsOwner(clSnapGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped(clMCPName, nil), captureLegsReadyOwnedMCP()).
		Build()
	r := captureLegsController(cl, clSnapGVK, false)

	if _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil)); err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}
	if got, found := captureLegsOwnerLatch(t, cl, clSnapGVK, "manifestCaptured"); !found || !got {
		t.Fatalf("latch is copied from the published checkpoint, not from a live MCR, got found=%v value=%v", found, got)
	}
}

// The legs are declared in ONE write, and the test watches the writes rather than the end state, because
// the end state is right either way — what was wrong was the moment in between.
//
// A data-artifact kind whose manifest leg the durable checkpoint proves captured, and whose data leg it
// does not: if the restore and the declaration are separate patches, the owner passes through
// manifestCaptured=true with dataCaptured not declared at all. Every declared leg is then captured, which
// is what a domain waits for, so it publishes its terminal phase — and a terminal phase is never
// recomputed, so the data leg it was still owed never arrives.
//
// The assertion is therefore on every status patch this pass makes: none of them may leave the owner with
// the manifest leg latched and an applicable data leg undeclared.
func TestReconcileOwnerCaptureLegs_DeclaresApplicableLegsInOneWrite(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)
	owner := captureLegsOwner(clSnapGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped(clMCPName, nil), captureLegsReadyOwnedMCP()).
		Build()

	type legsAfterWrite struct{ manifest, data string }
	var writes []legsAfterWrite
	read := func(u *unstructured.Unstructured, leg string) string {
		v, found, _ := unstructured.NestedBool(u.Object, "status", "captureState", "commonController", leg)
		switch {
		case !found:
			return "absent"
		case v:
			return "true"
		default:
			return "false"
		}
	}
	spy := interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetName() == clOwnerName {
				writes = append(writes, legsAfterWrite{manifest: read(u, "manifestCaptured"), data: read(u, "dataCaptured")})
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	r := captureLegsController(spy, clSnapGVK, true)

	if _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil)); err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}

	if len(writes) == 0 {
		t.Fatal("no capture-leg write was made at all")
	}
	for i, w := range writes {
		if w.manifest == "true" && w.data == "absent" {
			t.Fatalf("write %d left manifestCaptured=true with the data leg undeclared: a domain reading that "+
				"sees every declared leg captured and goes terminal, and the data leg never arrives (writes=%+v)", i, writes)
		}
	}

	if v, found := captureLegsOwnerLatch(t, base, clSnapGVK, "manifestCaptured"); !found || !v {
		t.Fatalf("the durable checkpoint must restore manifestCaptured, got found=%v value=%v", found, v)
	}
	if v, found := captureLegsOwnerLatch(t, base, clSnapGVK, "dataCaptured"); !found || v {
		t.Fatalf("the data leg is applicable and unproven, so it must be declared false, got found=%v value=%v", found, v)
	}
}

// One patch is what this controller controls; what survives it is not. A structural schema that declares
// one leg and not the other, or a writer that replaces the status between the write and the read, leaves
// exactly half of the declaration — and half of it is not a smaller truth, it is a different one: a
// manifest leg latched beside a data leg that is simply absent reads, to anything that does not know the
// kind, as a node with no data line whose capture is finished.
//
// So the confirming read checks every leg this pass wrote, declared falses included, and a pass that lost
// part of its write declares nothing settled and comes back.
func TestReconcileOwnerCaptureLegs_PartiallyPersistedDeclarationIsNotSettled(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)
	owner := captureLegsOwner(clSnapGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped(clMCPName, nil), captureLegsReadyOwnedMCP()).
		Build()

	// The apiserver keeps the manifest leg and drops the data leg: the target state is
	// manifestCaptured=true + dataCaptured=false, and only the first half is readable afterwards.
	dropData := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetName() == clOwnerName {
				unstructured.RemoveNestedField(u.Object, "status", "captureState", "commonController", "dataCaptured")
			}
			return nil
		},
	})
	rec := events.NewFakeRecorder(8)
	r := &SnapshotContentController{Client: base, APIReader: dropData, GVKRegistry: snapshot.NewGVKRegistry(), Recorder: rec}
	r.MarkDomainCaptureKind(clSnapGVK)
	r.MarkRequiresDataArtifact(clSnapGVK.Kind, true)
	registerSnapshotContentMetrics()
	before := testutil.ToFloat64(captureStateRestoredTotal)

	requeue, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil))
	if err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}
	if !requeue {
		t.Fatal("a declaration that only half survived must requeue: the data leg of a data-artifact kind is " +
			"still undeclared, and the next pass is the only thing that will write it again")
	}
	if delta := testutil.ToFloat64(captureStateRestoredTotal) - before; delta != 0 {
		t.Fatalf("a half-persisted write is not a completed restore, got delta=%v", delta)
	}
	select {
	case ev := <-rec.Events:
		if strings.Contains(ev, EventReasonCaptureStateRestored) {
			t.Fatalf("a half-persisted write must not report a restore, got %q", ev)
		}
	default:
	}
}

// The owner this reconcile started from is a cache-age snapshot; the object it writes is read fresh, and a
// foreign writer fits between the two. That gap is not the one the partial-persistence test covers — there
// the write lands and half of it is lost afterwards; here the latch is gone BEFORE the write is composed,
// so the pass decides what to write about a leg that no longer says what it said.
//
// Deciding whether the durable artifact proves a leg captured must therefore not depend on the stale copy
// at all. When it did, a leg that was latched at the start and wiped a moment later arrived at the write
// with no proof attached — and the only branch left declared it false, with the checkpoint that proves it
// captured sitting in the content the same pass had just read.
func TestReconcileOwnerCaptureLegs_WipeBetweenTheReadsNeverDeclaresAProvenLegFalse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		leg     string
		initial map[string]bool
		content *storagev1alpha1.SnapshotContent
	}{
		{
			name:    "manifest leg latched at the start, gone by the write",
			leg:     "manifestCaptured",
			initial: map[string]bool{"manifestCaptured": true, "dataCaptured": false},
			content: captureLegsContentTyped(clMCPName, nil),
		},
		{
			name:    "data leg latched at the start, gone by the write",
			leg:     "dataCaptured",
			initial: map[string]bool{"manifestCaptured": false, "dataCaptured": true},
			content: captureLegsContentTyped("", captureLegsData()),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := captureLegsScheme(t)
			owner := captureLegsOwner(clSnapGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))
			for leg, v := range tc.initial {
				_ = unstructured.SetNestedField(owner.Object, v, "status", "captureState", "commonController", leg)
			}

			base := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(owner).
				WithObjects(owner, tc.content, captureLegsReadyOwnedMCP()).
				Build()

			// The foreign writer strikes exactly once, between the read this reconcile started from and the
			// read the write is composed against.
			ownerReads := 0
			wiping := interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}
					u, ok := obj.(*unstructured.Unstructured)
					if !ok || u.GetName() != clOwnerName {
						return nil
					}
					ownerReads++
					if ownerReads == 2 {
						unstructured.RemoveNestedField(u.Object, "status", "captureState", "commonController", tc.leg)
					}
					return nil
				},
			})
			r := &SnapshotContentController{Client: base, APIReader: wiping, GVKRegistry: snapshot.NewGVKRegistry()}
			r.MarkDomainCaptureKind(clSnapGVK)
			r.MarkRequiresDataArtifact(clSnapGVK.Kind, true)

			if _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil)); err != nil {
				t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
			}

			v, found := captureLegsOwnerLatch(t, base, clSnapGVK, tc.leg)
			if found && !v {
				t.Fatalf("%s was declared false while the durable artifact proves it captured: the leg was "+
					"latched when this pass began and wiped before it wrote, and losing the proof with it is "+
					"exactly how the recapture this path exists to prevent gets invited", tc.leg)
			}
			if !found || !v {
				t.Fatalf("%s = (found=%v value=%v), want it restored to true from the durable artifact", tc.leg, found, v)
			}
		})
	}
}
