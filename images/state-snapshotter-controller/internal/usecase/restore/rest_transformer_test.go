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

package restore

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/restoretransform"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func testRESTNode() *RestoreNode {
	return &RestoreNode{SnapshotRef: snapshot.ObjectRef{
		APIVersion: "demo/v1", Kind: "DemoSnapshot", Name: "snap", Namespace: "ns",
	}}
}

func testRESTObject() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "demo/v1",
		"kind":       "DemoThing",
		"metadata":   map[string]interface{}{"name": "thing", "namespace": "ns"},
		"spec":       map[string]interface{}{},
	}}
}

// transformerWithHandler wires a RESTTransformer to an httptest server running fn, returning the
// transformer and a cleanup func.
func transformerWithHandler(t *testing.T, fn func(req restoretransform.Request) restoretransform.Response) (*RESTTransformer, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env restoretransform.RequestEnvelope
		_ = json.NewDecoder(r.Body).Decode(&env)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(restoretransform.ResponseEnvelope{Response: fn(env.Request)})
	}))
	return NewRESTTransformer(srv.URL + restoretransform.EndpointPath), srv.Close
}

func TestRESTTransformer_AllowedFalseIsContractViolation(t *testing.T) {
	tr, closeSrv := transformerWithHandler(t, func(req restoretransform.Request) restoretransform.Response {
		return restoretransform.Response{UID: req.UID, Handled: false, Allowed: false,
			Status: &restoretransform.Status{Reason: "InvalidObject", Message: "nope"}}
	})
	defer closeSrv()

	handled, err := tr.TransformObject(testRESTNode(), testRESTObject(), nil)
	if handled {
		t.Fatal("expected not handled on allowed=false")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}

func TestRESTTransformer_IdentityMismatchIsContractViolation(t *testing.T) {
	tr, closeSrv := transformerWithHandler(t, func(req restoretransform.Request) restoretransform.Response {
		// Return a handled object with a different name than requested.
		obj := map[string]interface{}{
			"apiVersion": req.TargetRef.APIVersion,
			"kind":       req.TargetRef.Kind,
			"metadata":   map[string]interface{}{"name": "tampered", "namespace": req.TargetRef.Namespace},
		}
		return restoretransform.Response{UID: req.UID, Handled: true, Allowed: true, Object: obj}
	})
	defer closeSrv()

	_, err := tr.TransformObject(testRESTNode(), testRESTObject(), nil)
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation on identity mismatch, got %v", err)
	}
}

func TestRESTTransformer_UIDMismatchIsContractViolation(t *testing.T) {
	tr, closeSrv := transformerWithHandler(t, func(req restoretransform.Request) restoretransform.Response {
		return restoretransform.Response{UID: "wrong-uid", Handled: false, Allowed: true}
	})
	defer closeSrv()

	_, err := tr.TransformObject(testRESTNode(), testRESTObject(), nil)
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation on uid mismatch, got %v", err)
	}
}

func TestRESTTransformer_HandledTrueWithoutObjectIsContractViolation(t *testing.T) {
	tr, closeSrv := transformerWithHandler(t, func(req restoretransform.Request) restoretransform.Response {
		return restoretransform.Response{UID: req.UID, Handled: true, Allowed: true, Object: nil}
	})
	defer closeSrv()

	_, err := tr.TransformObject(testRESTNode(), testRESTObject(), nil)
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation when handled=true without object, got %v", err)
	}
}

func TestRESTTransformer_NilNodeAndNilObjectDoNotPanic(t *testing.T) {
	tr, closeSrv := transformerWithHandler(t, func(req restoretransform.Request) restoretransform.Response {
		return restoretransform.Response{UID: req.UID, Allowed: true}
	})
	defer closeSrv()

	if _, err := tr.TransformObject(nil, testRESTObject(), nil); !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation on nil node, got %v", err)
	}
	if _, err := tr.TransformObject(testRESTNode(), nil, nil); !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation on nil object, got %v", err)
	}
	if covered := tr.CoveredPVCNames(nil, nil); len(covered) != 0 {
		t.Fatalf("expected empty covered on nil node, got %v", covered)
	}
}

// TestRESTTransformer_NonPVCSuppressFailsClosed verifies a suppressRef the generic compiler cannot
// interpret (non-PVC) is not silently ignored: it fails closed via the stashed error.
func TestRESTTransformer_NonPVCSuppressFailsClosed(t *testing.T) {
	var captured error
	tr, closeSrv := transformerWithHandler(t, func(req restoretransform.Request) restoretransform.Response {
		return restoretransform.Response{UID: req.UID, Handled: true, Allowed: true,
			SuppressRefs: []restoretransform.ObjectRef{{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "ns", Name: "web"}}}
	})
	defer closeSrv()
	tr.WithErrorSink(func(err error) { captured = err })

	node := testRESTNode()
	covered := tr.CoveredPVCNames(node, []unstructured.Unstructured{*testRESTObject()})
	if len(covered) != 0 {
		t.Fatalf("expected no covered PVCs on unsupported suppressRef, got %v", covered)
	}
	if !errors.Is(captured, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation reported to sink, got %v", captured)
	}
	if _, err := tr.TransformObject(node, testRESTObject(), nil); !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected TransformObject to surface ErrContractViolation, got %v", err)
	}
}

// TestRESTTransformer_SuppressFailsClosed verifies that a transport error during CoveredPVCNames is not
// silently ignored: it is stashed and surfaced by the next TransformObject, aborting the restore.
func TestRESTTransformer_SuppressFailsClosed(t *testing.T) {
	var captured error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	tr := NewRESTTransformer(srv.URL + restoretransform.EndpointPath).WithErrorSink(func(err error) { captured = err })

	node := testRESTNode()
	covered := tr.CoveredPVCNames(node, []unstructured.Unstructured{*testRESTObject()})
	if len(covered) != 0 {
		t.Fatalf("expected no covered PVCs on transport error, got %v", covered)
	}
	if captured == nil {
		t.Fatal("expected suppress error to be reported to the error sink")
	}

	// The next TransformObject of the same node must fail closed with the stashed error.
	_, err := tr.TransformObject(node, testRESTObject(), nil)
	if err == nil {
		t.Fatal("expected TransformObject to surface the stashed suppress error")
	}
}
