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
	"encoding/json"
	"net/http"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/restoretransform"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// RestoreTransformHandler is the demo domain controller's HTTP implementation of the restore
// transform contract (ADR 2026-06-13, PoC transport). It is a thin transport adapter over the same
// in-process demo RestoreTransformer: it decodes the envelope, runs the demo transform/suppress logic
// and returns the transformed object plus PVC suppress intent. This is the reference a domain team
// copies to expose their own transform endpoint; it introduces no Kubernetes APIService.
type RestoreTransformHandler struct {
	transformer RestoreTransformer
}

// NewRestoreTransformHandler returns the demo transform HTTP handler.
func NewRestoreTransformHandler() *RestoreTransformHandler { return &RestoreTransformHandler{} }

// SetupRoutes registers the transform endpoint on mux at the canonical contract path.
func (h *RestoreTransformHandler) SetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc(restoretransform.EndpointPath, h.ServeHTTP)
}

func (h *RestoreTransformHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var reqEnvelope restoretransform.RequestEnvelope
	if err := json.NewDecoder(r.Body).Decode(&reqEnvelope); err != nil {
		http.Error(w, "invalid transform request: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeResponse(w, h.transform(reqEnvelope.Request))
}

// transform runs the demo transform/suppress for a single object and builds the contract response.
func (h *RestoreTransformHandler) transform(req restoretransform.Request) restoretransform.Response {
	resp := restoretransform.Response{UID: req.UID, Allowed: true}
	if req.Object == nil {
		resp.Allowed = false
		resp.Status = &restoretransform.Status{Reason: "InvalidObject", Message: "request.object is empty"}
		return resp
	}

	obj := &unstructured.Unstructured{Object: req.Object}
	node := &restore.RestoreNode{SnapshotRef: snapshot.ObjectRef{
		APIVersion: req.Node.SnapshotRef.APIVersion,
		Kind:       req.Node.SnapshotRef.Kind,
		Name:       req.Node.SnapshotRef.Name,
		Namespace:  req.Node.SnapshotRef.Namespace,
	}}

	handled, err := h.transformer.TransformObject(node, obj, nil)
	if err != nil {
		resp.Allowed = false
		resp.Status = &restoretransform.Status{Reason: "TransformFailed", Message: err.Error()}
		return resp
	}

	// PVCs this demo object recreates on restore (suppress intent for the generic compiler).
	covered := h.transformer.CoveredPVCNames(node, []unstructured.Unstructured{*obj})
	for name := range covered {
		resp.SuppressRefs = append(resp.SuppressRefs, restoretransform.ObjectRef{
			APIVersion: "v1",
			Kind:       "PersistentVolumeClaim",
			Namespace:  req.RestoreContext.TargetNamespace,
			Name:       name,
		})
	}

	resp.Handled = handled || len(resp.SuppressRefs) > 0
	if resp.Handled {
		resp.Object = obj.Object
	}
	return resp
}

func writeResponse(w http.ResponseWriter, resp restoretransform.Response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(restoretransform.ResponseEnvelope{Response: resp})
}
