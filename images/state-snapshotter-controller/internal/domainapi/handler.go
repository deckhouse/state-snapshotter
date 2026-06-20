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

package domainapi

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

const (
	// domainGroup is intentionally DISTINCT from the core's aggregated group
	// (subresources.state-snapshotter.deckhouse.io). A Kubernetes APIService maps a group/version to
	// exactly one backing Service, so the domain controller must own its own group; the core remains the
	// single APIService for the generic subresources and orchestrates calls into this domain group.
	domainGroup        = "subresources.demo.state-snapshotter.deckhouse.io"
	domainVersion      = "v1alpha1"
	domainGroupVersion = domainGroup + "/" + domainVersion

	groupPath          = "/apis/" + domainGroup
	groupSubtreePath   = groupPath + "/"
	namespacedRelative = domainVersion + "/namespaces/"

	subresourceManifests        = "manifests"
	subresourceManifestsRestore = "manifests-with-data-restoration"
)

// Handler serves the domain aggregated API: discovery + restore subresources for demo snapshot kinds.
type Handler struct {
	service *RestoreService
	logger  logger.LoggerInterface
}

// NewHandler builds the domain API handler.
func NewHandler(service *RestoreService, log logger.LoggerInterface) *Handler {
	return &Handler{service: service, logger: log}
}

// SetupRoutes registers the domain API routes on mux.
func (h *Handler) SetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", h.handleHealth)
	mux.HandleFunc("/readyz", h.handleHealth)
	mux.HandleFunc("/livez", h.handleHealth)
	// Group discovery (exact path, no trailing slash).
	mux.HandleFunc(groupPath, h.handleGroupDiscovery)
	// Version discovery + namespaced restore subresources (subtree).
	mux.HandleFunc(groupSubtreePath, h.handleSubtree)
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) handleGroupDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "only GET is supported")
		return
	}
	group := map[string]interface{}{
		"kind":       "APIGroup",
		"apiVersion": "v1",
		"name":       domainGroup,
		"versions": []map[string]interface{}{
			{"groupVersion": domainGroupVersion, "version": domainVersion},
		},
		"preferredVersion": map[string]interface{}{"groupVersion": domainGroupVersion, "version": domainVersion},
	}
	h.writeJSON(w, group)
}

func (h *Handler) handleSubtree(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, groupSubtreePath)
	rel = strings.TrimSuffix(rel, "/")

	// Version discovery: /apis/<group>/v1alpha1
	if rel == domainVersion {
		h.handleResourceListDiscovery(w, r)
		return
	}

	// Namespaced restore subresources: /apis/<group>/v1alpha1/namespaces/<ns>/<resource>/<name>/<sub>
	if !strings.HasPrefix(rel, namespacedRelative) {
		h.writeError(w, http.StatusNotFound, "NotFound", "resource not found")
		return
	}
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "only GET is supported")
		return
	}
	parts := strings.Split(strings.TrimPrefix(rel, namespacedRelative), "/")
	if len(parts) != 4 {
		h.writeError(w, http.StatusNotFound, "NotFound", "subresource required")
		return
	}
	namespace, resource, name, subresource := parts[0], parts[1], parts[2], parts[3]
	if !IsDomainSnapshotResource(resource) {
		h.writeError(w, http.StatusNotFound, "NotFound", "unsupported resource")
		return
	}
	// Defense-in-depth: reject anything that is not a valid DNS-1123 namespace/name before it reaches
	// the core client path builder (path traversal / injection hardening).
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		h.writeError(w, http.StatusBadRequest, "BadRequest", "invalid namespace")
		return
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		h.writeError(w, http.StatusBadRequest, "BadRequest", "invalid name")
		return
	}

	switch subresource {
	case subresourceManifests:
		data, err := h.service.BaseManifests(r.Context(), resource, namespace, name)
		if err != nil {
			h.writeServiceError(w, err)
			return
		}
		h.writeManifests(w, r, data)
	case subresourceManifestsRestore:
		data, err := h.service.ManifestsWithDataRestoration(r.Context(), resource, namespace, name, r.URL.Query().Get("targetNamespace"))
		if err != nil {
			h.writeServiceError(w, err)
			return
		}
		h.writeManifests(w, r, data)
	default:
		h.writeError(w, http.StatusNotFound, "NotFound", "unknown subresource")
	}
}

func (h *Handler) handleResourceListDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "only GET is supported")
		return
	}
	resource := func(name, kind string) map[string]interface{} {
		return map[string]interface{}{
			"name":       name,
			"namespaced": true,
			"kind":       kind,
			"verbs":      []string{"get"},
		}
	}
	list := map[string]interface{}{
		"kind":         "APIResourceList",
		"apiVersion":   "v1",
		"groupVersion": domainGroupVersion,
		"resources": []map[string]interface{}{
			resource(ResourceDemoVirtualDiskSnapshot+"/"+subresourceManifests, "DemoVirtualDiskSnapshot"),
			resource(ResourceDemoVirtualDiskSnapshot+"/"+subresourceManifestsRestore, "DemoVirtualDiskSnapshot"),
			resource(ResourceDemoVirtualMachineSnapshot+"/"+subresourceManifests, "DemoVirtualMachineSnapshot"),
			resource(ResourceDemoVirtualMachineSnapshot+"/"+subresourceManifestsRestore, "DemoVirtualMachineSnapshot"),
		},
	}
	h.writeJSON(w, list)
}

func (h *Handler) writeManifests(w http.ResponseWriter, r *http.Request, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	if shouldCompressResponse(r) {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		_, _ = gz.Write(data)
		return
	}
	_, _ = w.Write(data)
}

func (h *Handler) writeJSON(w http.ResponseWriter, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (h *Handler) writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case apierrors.IsNotFound(err):
		h.writeError(w, http.StatusNotFound, "NotFound", err.Error())
	case apierrors.IsForbidden(err):
		h.writeError(w, http.StatusForbidden, "Forbidden", err.Error())
	default:
		h.writeError(w, http.StatusInternalServerError, "InternalError", err.Error())
	}
}

func (h *Handler) writeError(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	status := metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Message:  message,
		Reason:   metav1.StatusReason(reason),
		Code:     int32(code),
	}
	_ = json.NewEncoder(w).Encode(status)
}

func shouldCompressResponse(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}
