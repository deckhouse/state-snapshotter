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
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// snapshotOwnerGVK stands in for any owning xxxSnapshot kind (the core Snapshot is registered in the test
// scheme; mapVCRToOwningContent is kind-agnostic — it reads status.boundSnapshotContentName off whatever
// GVK the VCR's controller-ownerRef names).
var snapshotOwnerGVK = storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot")

func vcrWithOwner(owner *metav1.OwnerReference) *unstructured.Unstructured {
	vcr := &unstructured.Unstructured{}
	vcr.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	vcr.SetNamespace("ns1")
	vcr.SetName("vcr-a")
	if owner != nil {
		vcr.SetOwnerReferences([]metav1.OwnerReference{*owner})
	}
	return vcr
}

func ownerRefToSnapshot(name string, controller bool) metav1.OwnerReference {
	ref := metav1.OwnerReference{
		APIVersion: snapshotOwnerGVK.GroupVersion().String(),
		Kind:       snapshotOwnerGVK.Kind,
		Name:       name,
	}
	if controller {
		ref.Controller = ptr.To(true)
	}
	return ref
}

func boundSnapshot(namespace, name, boundContent string) *storagev1alpha1.Snapshot {
	return &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Status:     storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: boundContent},
	}
}

func vcrTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	return scheme
}

// TestMapVCRToOwningContent_RoutesViaOwnerSnapshot verifies a VCR routes to its owning snapshot's bound
// SnapshotContent (VCR controller-ownerRef -> snapshot -> status.boundSnapshotContentName).
func TestMapVCRToOwningContent_RoutesViaOwnerSnapshot(t *testing.T) {
	scheme := vcrTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(boundSnapshot("ns1", "snap-a", "content-a")).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl}

	owner := ownerRefToSnapshot("snap-a", true)
	reqs := r.mapVCRToOwningContent(context.Background(), vcrWithOwner(&owner))
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d (%v)", len(reqs), reqs)
	}
	if reqs[0].Name != "content-a" || reqs[0].Namespace != "" {
		t.Fatalf("want cluster-scoped request for content-a, got %#v", reqs[0].NamespacedName)
	}
}

// TestMapVCRToOwningContent_Negatives verifies best-effort no-op routing (nil) when the VCR has no
// controller-owner, when the owner snapshot is missing, and when it is not bound yet.
func TestMapVCRToOwningContent_Negatives(t *testing.T) {
	scheme := vcrTestScheme(t)

	t.Run("no controller owner", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		r := &SnapshotContentController{Client: cl, APIReader: cl}
		// A non-controller ownerRef must be ignored (GetControllerOf returns nil).
		owner := ownerRefToSnapshot("snap-a", false)
		if reqs := r.mapVCRToOwningContent(context.Background(), vcrWithOwner(&owner)); reqs != nil {
			t.Fatalf("want nil for non-controller owner, got %v", reqs)
		}
	})

	t.Run("owner snapshot not found", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		r := &SnapshotContentController{Client: cl, APIReader: cl}
		owner := ownerRefToSnapshot("ghost", true)
		if reqs := r.mapVCRToOwningContent(context.Background(), vcrWithOwner(&owner)); reqs != nil {
			t.Fatalf("want nil for missing owner snapshot, got %v", reqs)
		}
	})

	t.Run("owner not bound", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(boundSnapshot("ns1", "snap-a", "")).Build()
		r := &SnapshotContentController{Client: cl, APIReader: cl}
		owner := ownerRefToSnapshot("snap-a", true)
		if reqs := r.mapVCRToOwningContent(context.Background(), vcrWithOwner(&owner)); reqs != nil {
			t.Fatalf("want nil for unbound owner snapshot, got %v", reqs)
		}
	})
}

// recordingController is a fake controller.Controller handle that records Watch calls.
type recordingController struct {
	watchCount int
}

func (c *recordingController) Reconcile(context.Context, reconcile.Request) (reconcile.Result, error) {
	return reconcile.Result{}, nil
}
func (c *recordingController) Watch(source.TypedSource[reconcile.Request]) error {
	c.watchCount++
	return nil
}
func (c *recordingController) Start(context.Context) error { return nil }
func (c *recordingController) GetLogger() logr.Logger      { return logr.Discard() }

func mappableVCRRESTMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(vcpkg.VolumeCaptureRequestGVK, meta.RESTScopeNamespace)
	return m
}

// TestAddVolumeCaptureRequestWatch_SkipWhenNotMappable verifies a not-yet-installed VCR CRD degrades to
// "no watch" (no error, no Watch call, not marked added).
func TestAddVolumeCaptureRequestWatch_SkipWhenNotMappable(t *testing.T) {
	handle := &recordingController{}
	r := &SnapshotContentController{
		RESTMapper:         meta.NewDefaultRESTMapper(nil), // empty: VCR GVK not mappable
		contentControllers: map[string]controller.Controller{unifiedbootstrap.CommonSnapshotContentGVK().String(): handle},
	}
	if err := r.addVolumeCaptureRequestWatch(nil); err != nil {
		t.Fatalf("addVolumeCaptureRequestWatch (not mappable): unexpected error %v", err)
	}
	if handle.watchCount != 0 {
		t.Fatalf("want 0 Watch calls when GVK not mappable, got %d", handle.watchCount)
	}
	if r.vcrWatchAdded {
		t.Fatalf("vcrWatchAdded must stay false when the watch was skipped")
	}
}

// TestAddVolumeCaptureRequestWatch_AddsOnceIdempotent verifies the watch is added exactly once on the
// existing content controller handle and repeat calls are no-ops.
func TestAddVolumeCaptureRequestWatch_AddsOnceIdempotent(t *testing.T) {
	handle := &recordingController{}
	r := &SnapshotContentController{
		RESTMapper:         mappableVCRRESTMapper(),
		contentControllers: map[string]controller.Controller{unifiedbootstrap.CommonSnapshotContentGVK().String(): handle},
	}
	if err := r.addVolumeCaptureRequestWatch(nil); err != nil {
		t.Fatalf("addVolumeCaptureRequestWatch (first): %v", err)
	}
	if handle.watchCount != 1 {
		t.Fatalf("want 1 Watch call after first add, got %d", handle.watchCount)
	}
	if !r.vcrWatchAdded {
		t.Fatalf("vcrWatchAdded must be true after a successful add")
	}
	if err := r.addVolumeCaptureRequestWatch(nil); err != nil {
		t.Fatalf("addVolumeCaptureRequestWatch (second): %v", err)
	}
	if handle.watchCount != 1 {
		t.Fatalf("want Watch NOT called again (idempotent), got %d calls", handle.watchCount)
	}
}

// TestAddVolumeCaptureRequestWatch_ErrsWithoutHandle verifies a missing content controller handle is a
// programmer error (SetupWithManager must run first), surfaced instead of silently dropping the watch.
func TestAddVolumeCaptureRequestWatch_ErrsWithoutHandle(t *testing.T) {
	r := &SnapshotContentController{RESTMapper: mappableVCRRESTMapper()}
	if err := r.addVolumeCaptureRequestWatch(nil); err == nil {
		t.Fatalf("want an error when no content controller handle is built, got nil")
	}
	if r.vcrWatchAdded {
		t.Fatalf("vcrWatchAdded must stay false when the add failed")
	}
}
