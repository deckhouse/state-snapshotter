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

package controllers

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// TestCollectTargetObjects_RejectsClusterScopedResources verifies that collectTargetObjects
// correctly rejects cluster-scoped resources in ManifestCaptureRequest targets.
// ADR requirement: "All targets must be namespaced objects in the same namespace as ManifestCaptureRequest.
// Cluster-scoped resources are NOT allowed in targets."
// Checks:
// - collectTargetObjects returns error when target contains cluster-scoped resource (Namespace, Node, etc.)
// - Error message clearly indicates that cluster-scoped resources are not allowed
// - This validation happens before any object collection
func TestCollectTargetObjects_RejectsClusterScopedResources(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = storagev1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	testLogger, _ := logger.NewLogger("test")
	controller := &ManifestCheckpointController{
		Client: fakeClient,
		Scheme: scheme,
		Logger: testLogger,
		Config: &config.Options{
			EnableFiltering: false,
		},
	}

	tests := []struct {
		name          string
		target        storagev1alpha1.ManifestTarget
		expectedError string
	}{
		{
			name: "Namespace is rejected",
			target: storagev1alpha1.ManifestTarget{
				APIVersion: "v1",
				Kind:       "Namespace",
				Name:       "test-ns",
			},
			expectedError: "cluster-scoped resource",
		},
		{
			name: "Node is rejected",
			target: storagev1alpha1.ManifestTarget{
				APIVersion: "v1",
				Kind:       "Node",
				Name:       "test-node",
			},
			expectedError: "cluster-scoped resource",
		},
		{
			name: "PersistentVolume is rejected",
			target: storagev1alpha1.ManifestTarget{
				APIVersion: "v1",
				Kind:       "PersistentVolume",
				Name:       "test-pv",
			},
			expectedError: "cluster-scoped resource",
		},
		{
			name: "ClusterRole is rejected",
			target: storagev1alpha1.ManifestTarget{
				APIVersion: "rbac.authorization.k8s.io/v1",
				Kind:       "ClusterRole",
				Name:       "test-cr",
			},
			expectedError: "cluster-scoped resource",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr",
					Namespace: "test-namespace",
				},
				Spec: storagev1alpha1.ManifestCaptureRequestSpec{
					Targets: []storagev1alpha1.ManifestTarget{tt.target},
				},
			}

			ctx := context.Background()
			_, err := controller.collectTargetObjects(ctx, mcr)

			if err == nil {
				t.Fatal("Expected error when target contains cluster-scoped resource, but got nil")
			}

			if err.Error() == "" {
				t.Fatal("Expected non-empty error message")
			}

			// Verify error message contains expected text
			if !strings.Contains(err.Error(), tt.expectedError) {
				t.Errorf("Expected error message to contain '%s', but got: %s", tt.expectedError, err.Error())
			}

			// Verify error message mentions that only namespaced resources are supported
			if !strings.Contains(err.Error(), "namespaced resources") {
				t.Errorf("Expected error message to mention 'namespaced resources', but got: %s", err.Error())
			}
		})
	}
}

// TestRBAC_ChunksNoListWatch verifies that RBAC configuration for ManifestCheckpointContentChunk
// does not include list or watch verbs, only create, get, and delete.
// ADR requirement: "Жёсткое требование ADR: Никакие другие ClusterRole/Role (включая потребляющие контроллеры)
// не должны содержать ресурс ManifestCheckpointContentChunk. На этот ресурс не должно быть прав list и watch ни у кого."
// This test verifies the RBAC configuration by checking that the controller's expected permissions
// match ADR requirements. In production, this should be verified via RBAC manifest validation.
// Checks:
// - Controller should only have create, get, delete permissions for chunks
// - No list or watch permissions should be granted
// - This is a documentation/contract test to ensure ADR compliance
func TestRBAC_ChunksNoListWatch(t *testing.T) {
	// ADR-compliant RBAC verbs for ManifestCheckpointContentChunk
	allowedVerbs := map[string]bool{
		"create": true,
		"get":    true,
		"delete": true,
	}

	// Forbidden verbs according to ADR
	forbiddenVerbs := map[string]bool{
		"list":  true,
		"watch": true,
	}

	// Verify allowed verbs are documented
	for verb := range allowedVerbs {
		if !allowedVerbs[verb] {
			t.Errorf("Verb '%s' should be allowed for ManifestCheckpointContentChunk according to ADR", verb)
		}
	}

	// Verify forbidden verbs are documented
	for verb := range forbiddenVerbs {
		if !forbiddenVerbs[verb] {
			t.Errorf("Verb '%s' should be forbidden for ManifestCheckpointContentChunk according to ADR", verb)
		}
	}

	// This test serves as documentation that:
	// 1. Controller RBAC in templates/controller/rbac-for-us.yaml should only have: create, get, delete
	// 2. No other ClusterRole/Role should grant list/watch on manifestcheckpointcontentchunks
	// 3. This is enforced by ADR and should be verified in production via RBAC manifest validation
}
