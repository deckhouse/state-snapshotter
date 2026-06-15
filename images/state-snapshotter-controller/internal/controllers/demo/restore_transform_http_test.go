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

package demo

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/restoretransform"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// newRESTTransformerForTest spins up the demo transform HTTP handler and returns a generic REST
// transformer pointed at it, plus a cleanup func.
func newRESTTransformerForTest(t *testing.T) (*restore.RESTTransformer, func()) {
	t.Helper()
	mux := http.NewServeMux()
	NewRestoreTransformHandler().SetupRoutes(mux)
	srv := httptest.NewServer(mux)
	rest := restore.NewRESTTransformer(srv.URL + restoretransform.EndpointPath)
	return rest, srv.Close
}

// TestRESTTransform_EquivalentToInProcess_DiskUnderDiskSnapshot is the PoC smoke test: a DemoVirtualDisk
// transformed over the REST transport must be byte-for-byte equivalent to the in-process transformer,
// and the same PVC must be suppressed.
func TestRESTTransform_EquivalentToInProcess_DiskUnderDiskSnapshot(t *testing.T) {
	rest, closeSrv := newRESTTransformerForTest(t)
	defer closeSrv()

	node := &restore.RestoreNode{SnapshotRef: snapshot.ObjectRef{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDiskSnapshot,
		Name:       "disk-a-snap",
		Namespace:  "source-ns",
	}}

	inProcDisk := demoDisk("disk-a", "disk-a-pvc")
	inHandled, err := (RestoreTransformer{}).TransformObject(node, &inProcDisk, nil)
	if err != nil {
		t.Fatalf("in-process TransformObject: %v", err)
	}

	restDisk := demoDisk("disk-a", "disk-a-pvc")
	restHandled, err := rest.TransformObject(node, &restDisk, nil)
	if err != nil {
		t.Fatalf("REST TransformObject: %v", err)
	}

	if inHandled != restHandled || !restHandled {
		t.Fatalf("handled mismatch: in-process=%v rest=%v", inHandled, restHandled)
	}
	if !reflect.DeepEqual(inProcDisk.Object, restDisk.Object) {
		t.Fatalf("transformed object mismatch:\n in-process: %#v\n rest:       %#v", inProcDisk.Object, restDisk.Object)
	}

	rawDisk := demoDisk("disk-a", "disk-a-pvc")
	inCovered := (RestoreTransformer{}).CoveredPVCNames(node, []unstructured.Unstructured{rawDisk})
	restCovered := rest.CoveredPVCNames(node, []unstructured.Unstructured{rawDisk})
	if !reflect.DeepEqual(inCovered, restCovered) {
		t.Fatalf("covered PVCs mismatch: in-process=%v rest=%v", inCovered, restCovered)
	}
	if _, ok := restCovered["disk-a-pvc"]; !ok {
		t.Fatalf("expected disk-a-pvc covered over REST, got %v", restCovered)
	}
}

// TestRESTTransform_PassesThroughNonDomainObject confirms a non-demo object is reported not handled, so
// the generic compiler keeps its own sanitized object unchanged.
func TestRESTTransform_PassesThroughNonDomainObject(t *testing.T) {
	rest, closeSrv := newRESTTransformerForTest(t)
	defer closeSrv()

	node := &restore.RestoreNode{SnapshotRef: snapshot.ObjectRef{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDiskSnapshot,
		Name:       "disk-a-snap",
		Namespace:  "source-ns",
	}}
	cm := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "cfg", "namespace": "source-ns"},
	}}
	before := cm.DeepCopy()

	handled, err := rest.TransformObject(node, &cm, nil)
	if err != nil {
		t.Fatalf("REST TransformObject: %v", err)
	}
	if handled {
		t.Fatal("ConfigMap must not be handled by the demo domain")
	}
	if !reflect.DeepEqual(before.Object, cm.Object) {
		t.Fatalf("non-domain object must be unchanged, got %#v", cm.Object)
	}
}
