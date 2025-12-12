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

package handlers

import (
	"context"
	"fmt"
	"strings"

	"github.com/slok/kubewebhook/v2/pkg/model"
	kwhvalidating "github.com/slok/kubewebhook/v2/pkg/webhook/validating"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

// MCRValidate validates ManifestCaptureRequest by checking if the user who creates/updates the MCR
// has GET permissions for all target resources specified in the request.
// This ensures that users can only request backup of resources they have permission to read.
// The validation uses SubjectAccessReview to check permissions of the user who initiated the request
// (from arReview.UserInfo), not the webhook service account.
func MCRValidate(ctx context.Context, arReview *model.AdmissionReview, obj metav1.Object) (*kwhvalidating.ValidatorResult, error) {
	mcr, ok := obj.(*storagev1alpha1.ManifestCaptureRequest)
	if !ok {
		// If not a ManifestCaptureRequest, continue validation chain
		return &kwhvalidating.ValidatorResult{}, nil
	}

	// Skip validation for delete operations
	if mcr.ObjectMeta.DeletionTimestamp != nil || arReview.Operation == model.OperationDelete {
		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}

	// Validate that targets are not empty
	if len(mcr.Spec.Targets) == 0 {
		return &kwhvalidating.ValidatorResult{
			Valid:   false,
			Message: "At least one target must be specified",
		}, nil
	}

	// Validate that namespace is set (MCR is namespaced)
	if mcr.Namespace == "" {
		return &kwhvalidating.ValidatorResult{
			Valid:   false,
			Message: "ManifestCaptureRequest must be created in a namespace",
		}, nil
	}

	// Get Kubernetes client
	clientset, err := getKubernetesClient()
	if err != nil {
		klog.Errorf("Failed to create Kubernetes client: %v", err)
		return &kwhvalidating.ValidatorResult{
			Valid:   false,
			Message: fmt.Sprintf("Internal error: failed to create Kubernetes client: %v", err),
		}, nil
	}

	// Validate each target resource
	for i, target := range mcr.Spec.Targets {
		// Validate target fields
		if target.APIVersion == "" {
			return &kwhvalidating.ValidatorResult{
				Valid:   false,
				Message: fmt.Sprintf("Target %d: apiVersion is required", i),
			}, nil
		}
		if target.Kind == "" {
			return &kwhvalidating.ValidatorResult{
				Valid:   false,
				Message: fmt.Sprintf("Target %d: kind is required", i),
			}, nil
		}
		if target.Name == "" {
			return &kwhvalidating.ValidatorResult{
				Valid:   false,
				Message: fmt.Sprintf("Target %d: name is required", i),
			}, nil
		}

		// Parse API version
		gv, err := schema.ParseGroupVersion(target.APIVersion)
		if err != nil {
			return &kwhvalidating.ValidatorResult{
				Valid:   false,
				Message: fmt.Sprintf("Target %d: invalid apiVersion %s", i, target.APIVersion),
			}, nil
		}

		// Use Discovery API to get resource information (namespaced, plural name)
		resourceInfo, err := getResourceInfo(ctx, clientset, gv, target.Kind)
		if err != nil {
			klog.Errorf("Failed to discover resource info for target %d (%s/%s): %v", i, target.APIVersion, target.Kind, err)
			return &kwhvalidating.ValidatorResult{
				Valid:   false,
				Message: fmt.Sprintf("Target %d: failed to discover resource %s/%s: %v", i, target.APIVersion, target.Kind, err),
			}, nil
		}

		// Check if resource is namespaced (MCR only supports namespaced resources)
		if !resourceInfo.Namespaced {
			return &kwhvalidating.ValidatorResult{
				Valid:   false,
				Message: fmt.Sprintf("Target %d: resource %s/%s is cluster-scoped and cannot be captured", i, target.APIVersion, target.Kind),
			}, nil
		}

		// Check if resource exists in MCR namespace
		// MCR API requires all targets to be in the same namespace as the MCR
		actualNamespace, err := findResourceNamespace(ctx, gv, resourceInfo.Name, target.Name, mcr.Namespace)
		if err != nil {
			klog.Errorf("Failed to check resource namespace for target %d (%s/%s/%s): %v", i, target.APIVersion, target.Kind, target.Name, err)
			return &kwhvalidating.ValidatorResult{
				Valid:   false,
				Message: fmt.Sprintf("Target %d: failed to check resource %s/%s: %v", i, target.APIVersion, target.Kind, err),
			}, nil
		}

		// If resource not found in MCR namespace, reject
		if actualNamespace == "" {
			return &kwhvalidating.ValidatorResult{
				Valid:   false,
				Message: fmt.Sprintf("Target %d: resource %s/%s not found in namespace %q", i, target.APIVersion, target.Kind, mcr.Namespace),
			}, nil
		}

		// actualNamespace should always equal mcr.Namespace (MCR API requirement)
		// This check is defensive programming
		if actualNamespace != mcr.Namespace {
			return &kwhvalidating.ValidatorResult{
				Valid:   false,
				Message: fmt.Sprintf("Target %d: resource %s/%s is in namespace %q, but ManifestCaptureRequest is in namespace %q (resources must be in the same namespace as MCR)", i, target.APIVersion, target.Kind, actualNamespace, mcr.Namespace),
			}, nil
		}

		// Create SubjectAccessReview to check GET permission for the user who creates/updates the MCR
		// We check GET permission because to backup a resource, the user must be able to read it.
		// Note: We use the user info from arReview (the user who creates the MCR), not the webhook service account.
		// We use actualNamespace (which should equal mcr.Namespace after validation above) to ensure correct RBAC check.
		sar := &authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{
				User:   arReview.UserInfo.Username,
				Groups: arReview.UserInfo.Groups,
				UID:    arReview.UserInfo.UID,
				Extra:  convertExtra(arReview.UserInfo.Extra),
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: actualNamespace, // Use actual namespace where resource exists (should be mcr.Namespace)
					Verb:      "get",           // GET permission is required to read/backup the resource
					Group:     gv.Group,
					Version:   gv.Version,
					Resource:  resourceInfo.Name, // Use plural resource name from Discovery API
					Name:      target.Name,
				},
			},
		}

		// Perform the authorization check
		result, err := clientset.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
		if err != nil {
			klog.Errorf("Failed to create SubjectAccessReview for target %d (%s/%s/%s in namespace %s): %v",
				i, target.APIVersion, target.Kind, target.Name, actualNamespace, err)
			return &kwhvalidating.ValidatorResult{
				Valid:   false,
				Message: fmt.Sprintf("internal error: failed to check permissions for %s/%s in namespace %q", resourceInfo.Name, target.Name, actualNamespace),
			}, nil
		}

		// Check if the user has permission to read (GET) the resource
		// If not, reject the MCR creation/update - user cannot backup resources they cannot read
		if !result.Status.Allowed {
			reason := result.Status.Reason
			if reason == "" {
				reason = "forbidden by RBAC"
			}
			klog.Infof("User %s does not have GET permission for %s/%s/%s in namespace %s: %s",
				arReview.UserInfo.Username, target.APIVersion, target.Kind, target.Name, actualNamespace, reason)
			return &kwhvalidating.ValidatorResult{
				Valid:   false,
				Message: fmt.Sprintf("user %q cannot GET %s/%s in namespace %q: %s", arReview.UserInfo.Username, resourceInfo.Name, target.Name, actualNamespace, reason),
			}, nil
		}

		klog.V(4).Infof("User %s has GET permission for %s/%s/%s in namespace %s",
			arReview.UserInfo.Username, target.APIVersion, target.Kind, target.Name, actualNamespace)
	}

	return &kwhvalidating.ValidatorResult{Valid: true}, nil
}

// resourceInfo contains information about a Kubernetes resource from Discovery API
type resourceInfo struct {
	Name       string // Plural resource name (e.g., "pods", "configmaps")
	Namespaced bool   // Whether the resource is namespaced
}

// getResourceInfo uses Discovery API to get resource information (plural name, namespaced)
// This is more reliable than heuristics and allows us to validate that resources are namespaced
//
//nolint:unparam // error return is kept for future error handling
func getResourceInfo(_ context.Context, clientset kubernetes.Interface, gv schema.GroupVersion, kind string) (*resourceInfo, error) {
	// Get server resources for the API group/version
	resList, err := clientset.Discovery().ServerResourcesForGroupVersion(gv.String())
	if err != nil {
		// If discovery fails, fall back to heuristics (for CRDs that might not be discovered yet)
		return &resourceInfo{
			Name:       kindToResourceNameFallback(kind),
			Namespaced: true, // Assume namespaced if we can't discover (safer default)
		}, nil
	}

	// Find the resource by Kind
	for _, res := range resList.APIResources {
		if res.Kind == kind {
			return &resourceInfo{
				Name:       res.Name,
				Namespaced: res.Namespaced,
			}, nil
		}
	}

	// Resource not found in discovery - fall back to heuristics
	// This can happen for CRDs that are not yet fully registered
	klog.V(4).Infof("Resource %s/%s not found in discovery, using fallback heuristics", gv.String(), kind)
	return &resourceInfo{
		Name:       kindToResourceNameFallback(kind),
		Namespaced: true, // Assume namespaced if we can't discover (safer default)
	}, nil
}

// kindToResourceNameFallback converts a Kind to resource name using heuristics
// Used as fallback when Discovery API doesn't have the resource (e.g., CRDs not yet registered)
func kindToResourceNameFallback(kind string) string {
	// Common Kubernetes core resources
	kindToResource := map[string]string{
		"ConfigMap":             "configmaps",
		"Secret":                "secrets",
		"Service":               "services",
		"PersistentVolumeClaim": "persistentvolumeclaims",
		"Pod":                   "pods",
		"ServiceAccount":        "serviceaccounts",
		"Endpoints":             "endpoints",
		"Event":                 "events",
		"LimitRange":            "limitranges",
		"ResourceQuota":         "resourcequotas",
		"Deployment":            "deployments",
		"StatefulSet":           "statefulsets",
		"DaemonSet":             "daemonsets",
		"ReplicaSet":            "replicasets",
		"Job":                   "jobs",
		"CronJob":               "cronjobs",
		"Ingress":               "ingresses",
		"NetworkPolicy":         "networkpolicies",
		"Role":                  "roles",
		"RoleBinding":           "rolebindings",
	}

	if resource, ok := kindToResource[kind]; ok {
		return resource
	}

	// For custom resources, convert to lowercase and pluralize using heuristics
	lowerKind := strings.ToLower(kind)

	// Handle special cases for pluralization
	var resource string
	switch {
	case strings.HasSuffix(lowerKind, "y"):
		resource = strings.TrimSuffix(lowerKind, "y") + "ies"
	case strings.HasSuffix(lowerKind, "s") || strings.HasSuffix(lowerKind, "x") || strings.HasSuffix(lowerKind, "z"):
		resource = lowerKind + "es"
	case strings.HasSuffix(lowerKind, "ch") || strings.HasSuffix(lowerKind, "sh"):
		resource = lowerKind + "es"
	default:
		resource = lowerKind + "s"
	}

	return resource
}

// convertExtra converts the extra map from admission review to SubjectAccessReview format
func convertExtra(extra map[string]authenticationv1.ExtraValue) map[string]authorizationv1.ExtraValue {
	result := make(map[string]authorizationv1.ExtraValue)
	for k, v := range extra {
		result[k] = authorizationv1.ExtraValue(v)
	}
	return result
}

var (
	kubernetesClient kubernetes.Interface
	dynamicClient    dynamic.Interface
)

// SetKubernetesClient sets the Kubernetes client (called from main.go)
// Accepts both real and fake clientsets for testing (both implement kubernetes.Interface)
func SetKubernetesClient(client kubernetes.Interface) {
	kubernetesClient = client
}

// SetDynamicClient sets the dynamic client (called from main.go)
func SetDynamicClient(client dynamic.Interface) {
	dynamicClient = client
}

// getKubernetesClient returns the Kubernetes client
func getKubernetesClient() (kubernetes.Interface, error) {
	if kubernetesClient == nil {
		// Try to create in-cluster config
		config, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
		}
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			return nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
		}
		kubernetesClient = clientset
	}
	return kubernetesClient, nil
}

// getDynamicClient returns the dynamic client
func getDynamicClient() (dynamic.Interface, error) {
	if dynamicClient == nil {
		config, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
		}
		client, err := dynamic.NewForConfig(config)
		if err != nil {
			return nil, fmt.Errorf("failed to create dynamic client: %w", err)
		}
		dynamicClient = client
	}
	return dynamicClient, nil
}

// findResourceNamespace checks if a resource exists in the MCR namespace
// MCR API requires all targets to be in the same namespace as the MCR
// Returns the namespace if found (should always be mcrNamespace), or empty string if not found
func findResourceNamespace(ctx context.Context, gv schema.GroupVersion, resourceName string, targetName string, mcrNamespace string) (string, error) {
	dynClient, err := getDynamicClient()
	if err != nil {
		return "", fmt.Errorf("failed to get dynamic client: %w", err)
	}

	gvr := schema.GroupVersionResource{
		Group:    gv.Group,
		Version:  gv.Version,
		Resource: resourceName,
	}

	// Check only in MCR namespace (MCR API requires all targets to be in the same namespace)
	_, err = dynClient.Resource(gvr).Namespace(mcrNamespace).Get(ctx, targetName, metav1.GetOptions{})
	if err == nil {
		return mcrNamespace, nil
	}
	if errors.IsNotFound(err) {
		// Resource not found in MCR namespace
		return "", nil
	}

	// If it's not a NotFound error (e.g., API server temporary failure), log and treat as not found
	// We don't want to reject MCR creation due to temporary API issues
	klog.Warningf("Failed to check resource %s/%s in namespace %s (non-NotFound error): %v, treating as not found", resourceName, targetName, mcrNamespace, err)
	return "", nil
}
