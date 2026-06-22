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

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// subresources.snapshot.storage.k8s.io is OUR aggregated group-version for the generic-PVC extended
// VolumeSnapshot connector (C8). It is deliberately distinct from the CSI CRD group
// snapshot.storage.k8s.io (the real VolumeSnapshot/VolumeSnapshotContent objects live there) so this
// APIService does NOT intercept the CSI group. It exposes a VIRTUAL volumesnapshots resource with NO
// storage — only the same connector subresources as subresources.state-snapshotter: manifests-download
// (GET, export), manifests-and-children-refs-upload (POST, import), manifests-with-data-restoration
// (GET, restore). Each is keyed by the namespaced VolumeSnapshot name and resolves through
// VolumeSnapshot.status.boundSnapshotContentName -> SnapshotContent, reusing the same usecases as the
// core/generic snapshot kinds.
const (
	vsConnectorGroup        = "subresources.snapshot.storage.k8s.io"
	vsConnectorVersion      = "v1"
	vsConnectorGroupVersion = vsConnectorGroup + "/" + vsConnectorVersion
	vsConnectorResource     = "volumesnapshots"
)

// volumeSnapshotGVK is the underlying real CSI VolumeSnapshot GVK the connector resolves against
// (snapshot.storage.k8s.io/v1, Kind VolumeSnapshot) — NOT the virtual connector group.
func volumeSnapshotGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: snapshot.CSISnapshotGroup, Version: snapshot.CSISnapshotVersion, Kind: snapshot.KindVolumeSnapshot}
}

// SetupVolumeSnapshotConnectorRoutes registers the subresources.snapshot.storage.k8s.io group: discovery
// plus the namespaced volumesnapshots connector subresources. Longer prefixes win in http.ServeMux, so
// the /v1/namespaces/ subtree is dispatched by the connector router while bare /v1 and the group root are
// served by discovery.
func (h *RestoreHandler) SetupVolumeSnapshotConnectorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/apis/"+vsConnectorGroup, h.handleVolumeSnapshotGroupDiscovery)
	mux.HandleFunc("/apis/"+vsConnectorGroup+"/", h.handleVolumeSnapshotGroupDiscovery)
	mux.HandleFunc("/apis/"+vsConnectorGroupVersion, h.handleVolumeSnapshotResourceListDiscovery)
	mux.HandleFunc("/apis/"+vsConnectorGroupVersion+"/", h.handleVolumeSnapshotResourceListDiscovery)
	mux.HandleFunc("/apis/"+vsConnectorGroupVersion+"/namespaces/", h.handleVolumeSnapshotNamespaced)
}

// handleVolumeSnapshotGroupDiscovery serves GET /apis/subresources.snapshot.storage.k8s.io (APIGroup).
func (h *RestoreHandler) handleVolumeSnapshotGroupDiscovery(w http.ResponseWriter, r *http.Request) {
	if !h.requireMethod(w, r, http.MethodGet) {
		return
	}
	if strings.TrimSuffix(r.URL.Path, "/") != "/apis/"+vsConnectorGroup {
		h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "resource not found")
		return
	}
	apiGroup := metav1.APIGroup{
		TypeMeta: metav1.TypeMeta{Kind: "APIGroup", APIVersion: "v1"},
		Name:     vsConnectorGroup,
		Versions: []metav1.GroupVersionForDiscovery{
			{GroupVersion: vsConnectorGroupVersion, Version: vsConnectorVersion},
		},
		PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: vsConnectorGroupVersion, Version: vsConnectorVersion},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(apiGroup)
}

// handleVolumeSnapshotResourceListDiscovery serves GET /apis/subresources.snapshot.storage.k8s.io/v1
// (APIResourceList). Only the connector subresources are advertised — there is no listable/gettable
// bare volumesnapshots resource (storage lives in the CSI CRD group).
func (h *RestoreHandler) handleVolumeSnapshotResourceListDiscovery(w http.ResponseWriter, r *http.Request) {
	if !h.requireMethod(w, r, http.MethodGet) {
		return
	}
	if strings.TrimSuffix(r.URL.Path, "/") != "/apis/"+vsConnectorGroupVersion {
		h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "resource not found")
		return
	}
	apiResourceList := metav1.APIResourceList{
		TypeMeta:     metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"},
		GroupVersion: vsConnectorGroupVersion,
		APIResources: []metav1.APIResource{
			{Name: vsConnectorResource + "/manifests-download", Namespaced: true, Kind: snapshot.KindVolumeSnapshot, Verbs: []string{"get"}},
			{Name: vsConnectorResource + "/manifests-with-data-restoration", Namespaced: true, Kind: snapshot.KindVolumeSnapshot, Verbs: []string{"get"}},
			{Name: vsConnectorResource + "/manifests-and-children-refs-upload", Namespaced: true, Kind: snapshot.KindVolumeSnapshot, Verbs: []string{"create"}},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(apiResourceList)
}

// handleVolumeSnapshotNamespaced parses
// /apis/subresources.snapshot.storage.k8s.io/v1/namespaces/<ns>/volumesnapshots/<name>/<subresource>
// and dispatches the connector subresource.
func (h *RestoreHandler) handleVolumeSnapshotNamespaced(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/apis/"+vsConnectorGroupVersion+"/namespaces/")
	path = strings.TrimSuffix(path, "/")
	parts := strings.Split(path, "/")
	if len(parts) != 4 || parts[1] != vsConnectorResource {
		h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "expected namespaces/<ns>/volumesnapshots/<name>/<subresource>")
		return
	}
	namespace, name, subresource := parts[0], parts[2], parts[3]
	switch subresource {
	case "manifests-download":
		if !h.requireMethod(w, r, http.MethodGet) {
			return
		}
		h.handleVolumeSnapshotManifestsDownload(w, r, namespace, name)
	case "manifests-with-data-restoration":
		if !h.requireMethod(w, r, http.MethodGet) {
			return
		}
		h.handleVolumeSnapshotManifestsWithDataRestoration(w, r, namespace, name)
	case "manifests-and-children-refs-upload":
		if !h.requireMethod(w, r, http.MethodPost) {
			return
		}
		// Reuse the generic per-CR upload path, keyed by the VolumeSnapshot CR identity.
		h.handleManifestsAndChildrenUpload(w, r, volumeSnapshotGVK(), namespace, name)
	default:
		h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "unknown subresource")
	}
}

// handleVolumeSnapshotManifestsDownload returns the own-node manifests of a generic-PVC extended
// VolumeSnapshot (single-node, raw with status), resolved via status.boundSnapshotContentName.
func (h *RestoreHandler) handleVolumeSnapshotManifestsDownload(w http.ResponseWriter, r *http.Request, namespace, name string) {
	start := time.Now()
	if h.nsAggregated == nil {
		h.writeKubernetesErrorResponse(w, http.StatusInternalServerError, "InternalError", "manifests handler not configured")
		return
	}
	data, err := h.nsAggregated.BuildSingleNodeJSONFromSnapshot(r.Context(), volumeSnapshotGVK(), namespace, name)
	if err != nil {
		h.writeAggregatedError(w, err)
		return
	}
	h.writeJSONResponse(w, r, data)
	h.logger.Info("Returned VolumeSnapshot per-CR manifests-download", "volumeSnapshot", name, "namespace", namespace, "duration", time.Since(start))
}

// handleVolumeSnapshotManifestsWithDataRestoration returns the apply-ready restore output for a
// generic-PVC extended VolumeSnapshot leaf (the PVC bound to its VolumeSnapshot dataSourceRef).
func (h *RestoreHandler) handleVolumeSnapshotManifestsWithDataRestoration(w http.ResponseWriter, r *http.Request, namespace, name string) {
	start := time.Now()
	// SnapshotName is intentionally omitted: BuildManifestsWithDataRestorationForVolumeSnapshot takes the
	// VS name explicitly and the compiler only consumes SnapshotNamespace/TargetNamespace.
	opts := restore.Options{
		SnapshotNamespace: namespace,
		TargetNamespace:   r.URL.Query().Get("targetNamespace"),
	}
	data, err := h.service.BuildManifestsWithDataRestorationForVolumeSnapshot(r.Context(), namespace, name, opts)
	if err != nil {
		h.writeRestoreError(w, err)
		return
	}
	h.writeJSONResponse(w, r, data)
	h.logger.Info("Returned VolumeSnapshot manifests-with-data-restoration", "volumeSnapshot", name, "namespace", namespace, "duration", time.Since(start))
}
