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

package namespacemanifest

import (
	"context"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// n2aNamespacedGVR is the built-in allowlist for N2a (design §4.5), without optional networkpolicies.
var n2aNamespacedGVR = []schema.GroupVersionResource{
	{Group: "apps", Version: "v1", Resource: "deployments"},
	{Group: "apps", Version: "v1", Resource: "statefulsets"},
	{Group: "apps", Version: "v1", Resource: "daemonsets"},
	{Group: "batch", Version: "v1", Resource: "jobs"},
	{Group: "batch", Version: "v1", Resource: "cronjobs"},
	{Group: "", Version: "v1", Resource: "pods"},
	{Group: "", Version: "v1", Resource: "services"},
	{Group: "", Version: "v1", Resource: "configmaps"},
	{Group: "", Version: "v1", Resource: "secrets"},
	{Group: "", Version: "v1", Resource: "serviceaccounts"},
	{Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
	{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
}

// BuildManifestCaptureTargets is N2a-only: it lists the whole namespace per allowlist without List pagination.
// Large namespaces are intentionally unsupported until hardening; do not treat this as production-complete
// capture enumeration (see design namespace-snapshot-controller.md §4.5 / §5.1 and implementation-plan).
//
// The returned slice is sorted by (APIVersion, Kind, Name) for stable MCR spec and drift checks.
func BuildManifestCaptureTargets(ctx context.Context, dyn dynamic.Interface, namespace string) ([]ManifestTarget, error) {
	var targets []ManifestTarget
	seen := make(map[string]struct{})

	for _, gvr := range n2aNamespacedGVR {
		lst, err := dyn.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			if isDiscoveryNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("list %s in namespace %s: %w", gvr.String(), namespace, err)
		}
		for _, item := range lst.Items {
			key := objectKey(&item)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			apiVersion := item.GetAPIVersion()
			if apiVersion == "" {
				apiVersion = gvr.GroupVersion().String()
			}
			kind := item.GetKind()
			if kind == "" {
				continue
			}
			targets = append(targets, ManifestTarget{
				APIVersion: apiVersion,
				Kind:       kind,
				Name:       item.GetName(),
			})
		}
	}

	sort.Slice(targets, func(i, j int) bool {
		a, b := targets[i], targets[j]
		if a.APIVersion != b.APIVersion {
			return a.APIVersion < b.APIVersion
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
	return targets, nil
}

// ManifestTarget is a minimal capture target (mirrors api/v1alpha1.ManifestTarget for the controller layer).
type ManifestTarget struct {
	APIVersion string
	Kind       string
	Name       string
}

func objectKey(u *unstructured.Unstructured) string {
	return fmt.Sprintf("%s|%s|%s|%s", u.GetAPIVersion(), u.GetKind(), u.GetNamespace(), u.GetName())
}

func isDiscoveryNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "could not find the requested resource") ||
		strings.Contains(s, "the server could not find the requested resource")
}
