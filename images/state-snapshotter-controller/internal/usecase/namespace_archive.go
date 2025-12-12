/*
Copyright 2025 Flant JSC

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

package usecase

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/common"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// NamespaceArchiveService handles creation of namespace archives
type NamespaceArchiveService struct {
	client client.Client
	dyn    dynamic.Interface
	logger logger.LoggerInterface
}

// NewNamespaceArchiveService creates a new NamespaceArchiveService
func NewNamespaceArchiveService(client client.Client, dyn dynamic.Interface, logger logger.LoggerInterface) *NamespaceArchiveService {
	return &NamespaceArchiveService{
		client: client,
		dyn:    dyn,
		logger: logger,
	}
}

// CreateNamespaceArchive is not used in state-snapshotter module
// This method was moved to backup module where it's still used
//
// getNamespaceResources is kept as a helper function for backup module compatibility,
// but it should NOT be used for ManifestCaptureRequest creation.
// MCR targets must be explicitly provided by the caller to ensure RBAC compliance.

// getNamespaceResources lists namespaced resources in the given namespace using the dynamic client.
// Cluster-scoped objects can be added later; for now we collect only namespaced resources.
// The function applies exclusion rules and aggressively cleans runtime/noisy fields.
// It also validates that resulting objects keep Kind and APIVersion.
//
//nolint:unparam // error return is kept for future error handling
func (s *NamespaceArchiveService) getNamespaceResources(ctx context.Context, namespace string) ([]unstructured.Unstructured, error) {
	var out []unstructured.Unstructured

	namespaced := []schema.GroupVersionResource{
		{Group: "", Version: "v1", Resource: "pods"},
		{Group: "", Version: "v1", Resource: "services"},
		{Group: "", Version: "v1", Resource: "configmaps"},
		{Group: "", Version: "v1", Resource: "secrets"},
		{Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
		{Group: "apps", Version: "v1", Resource: "deployments"},
		{Group: "apps", Version: "v1", Resource: "replicasets"},
		{Group: "apps", Version: "v1", Resource: "statefulsets"},
		{Group: "apps", Version: "v1", Resource: "daemonsets"},
		{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
		{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
		// Custom resources
		{Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "virtualdisks"},
		{Group: "virtualization.deckhouse.io", Version: "v1alpha2", Resource: "virtualmachines"},
		// Virtualization internal resources
		{Group: "cdi.internal.virtualization.deckhouse.io", Version: "v1beta1", Resource: "internalvirtualizationdatavolumes"},
		{Group: "clone.internal.virtualization.deckhouse.io", Version: "v1alpha1", Resource: "internalvirtualizationvirtualmachineclones"},
		// Deckhouse resources
		{Group: "deckhouse.io", Version: "v1alpha1", Resource: "authorizationrules"},
		{Group: "deckhouse.io", Version: "v1alpha1", Resource: "podloggingconfigs"},
		// {Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"},
	}

	found, kept, skipped := 0, 0, 0
	kindCount := make(map[string]int)

	for _, gvr := range namespaced {
		lst, err := s.dyn.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			// Check if it's a "resource not found" error (CRD not installed)
			if strings.Contains(err.Error(), "could not find the requested resource") {
				s.logger.Info("Skipping resource type (CRD not installed)", "gvr", gvr.String(), "namespace", namespace)
			} else {
				s.logger.Error(err, "Failed to list resources", "gvr", gvr.String(), "namespace", namespace)
			}
			continue
		}

		found += len(lst.Items)

		for _, item := range lst.Items {
			// Base filter
			if common.ShouldSkipObjectWithLog(&item, s.logger) {
				skipped++
				continue
			}

			// Aggressive cleanup
			cleaned := s.cleanResourceForBackup(&item)
			if cleaned == nil {
				skipped++
				s.logger.Info("Excluded after cleaning (nil)",
					"gvr", gvr.String(),
					"name", item.GetName(),
					"kind", item.GetKind(),
					"reason", "cleanResourceForBackup returned nil")
				continue
			}

			// Strict validation: keep objects only if Kind and APIVersion are present
			if cleaned.GetKind() == "" || cleaned.GetAPIVersion() == "" {
				skipped++
				s.logger.Info("Excluded after cleaning (missing kind/apiVersion)",
					"gvr", gvr.String(),
					"name", item.GetName(),
					"originalKind", item.GetKind(),
					"cleanedKind", cleaned.GetKind(),
					"cleanedAPIVersion", cleaned.GetAPIVersion(),
				)
				continue
			}

			out = append(out, *cleaned)
			kindCount[cleaned.GetKind()]++
			kept++
			s.logger.Info("Resource kept for archive",
				"gvr", gvr.String(),
				"name", item.GetName(),
				"kind", cleaned.GetKind(),
				"namespace", cleaned.GetNamespace())
		}
	}

	s.logger.Info("Namespace resources collected",
		"namespace", namespace,
		"found", found,
		"kept", kept,
		"skipped", skipped,
		"byKind", kindCount,
	)

	return out, nil
}

// cleanResourceForBackup removes runtime fields and shrinks metadata.
// Returns nil if the object should not be archived at all.
// Now uses common.CleanObjectForSnapshot for consistency.
// Uses default excludeAnnotations (nil) - ConfigMap customization is not applied here.
func (s *NamespaceArchiveService) cleanResourceForBackup(u *unstructured.Unstructured) *unstructured.Unstructured {
	return common.CleanObjectForSnapshot(u, nil)
}

// Methods that use NamespaceSnapshot (CreateNamespaceArchive, createConfigMapsWithYAML, etc.)
// are not used in state-snapshotter module and have been removed.
// Only getNamespaceResources and cleanResourceForBackup are used here.
