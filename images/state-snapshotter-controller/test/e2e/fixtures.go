//go:build e2e
// +build e2e

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

package e2e

import (
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

// TestFixtures contains common test data
var TestFixtures = struct {
	// Namespaces
	TestNamespace1 string
	TestNamespace2 string

	// ConfigMaps
	TestConfigMapName string
	TestConfigMapData map[string]string

	// Services
	TestServiceName string

	// ManifestCaptureRequests
	TestMCRName string
}{
	TestNamespace1:    "test-ns-1",
	TestNamespace2:    "test-ns-2",
	TestConfigMapName: "test-cm",
	TestConfigMapData: map[string]string{
		"key1": "value1",
		"key2": "value2",
	},
	TestServiceName: "test-svc",
	TestMCRName:     "test-mcr",
}

// GetStandardTargets returns a standard set of targets for testing
func GetStandardTargets() []storagev1alpha1.ManifestTarget {
	return []storagev1alpha1.ManifestTarget{
		makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
		makeTarget("v1", "Service", TestFixtures.TestServiceName),
	}
}

// GetClusterScopedTarget returns a cluster-scoped target (should be rejected)
func GetClusterScopedTarget() storagev1alpha1.ManifestTarget {
	return makeTarget("v1", "Namespace", "test-namespace")
}

// GetNotFoundTarget returns a target that doesn't exist
func GetNotFoundTarget() storagev1alpha1.ManifestTarget {
	return makeTarget("v1", "ConfigMap", "non-existent-cm")
}
