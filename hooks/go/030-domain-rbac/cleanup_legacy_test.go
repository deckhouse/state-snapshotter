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

package domain_rbac

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/state-snapshotter/hooks/go/consts"
)

func cleanupTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add rbacv1 scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	return scheme
}

// Upgrade path: the hook-managed leftovers of the removed demo domain-controller (ClusterRole +
// ClusterRoleBinding + domain TLS Secret) must be deleted, and re-running against an already-clean
// cluster must be a silent no-op (NotFound is success).
func TestCleanupLegacyDomainControllerArtifacts(t *testing.T) {
	ctx := context.Background()
	scheme := cleanupTestScheme(t)

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: legacyDomainClusterRoleName}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: legacyDomainClusterRoleName}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: legacyDomainTLSSecretName, Namespace: consts.ModuleNamespace}},
	).Build()

	if err := cleanupLegacyDomainControllerArtifacts(ctx, cl); err != nil {
		t.Fatalf("cleanup on a cluster with leftovers: %v", err)
	}

	if err := cl.Get(ctx, ctrlclient.ObjectKey{Name: legacyDomainClusterRoleName}, &rbacv1.ClusterRole{}); err == nil {
		t.Error("legacy ClusterRole must be deleted")
	}
	if err := cl.Get(ctx, ctrlclient.ObjectKey{Name: legacyDomainClusterRoleName}, &rbacv1.ClusterRoleBinding{}); err == nil {
		t.Error("legacy ClusterRoleBinding must be deleted")
	}
	if err := cl.Get(ctx, ctrlclient.ObjectKey{Name: legacyDomainTLSSecretName, Namespace: consts.ModuleNamespace}, &corev1.Secret{}); err == nil {
		t.Error("legacy domain TLS Secret must be deleted")
	}

	// Second run on the already-clean cluster: NotFound everywhere is success.
	if err := cleanupLegacyDomainControllerArtifacts(ctx, cl); err != nil {
		t.Fatalf("cleanup must be idempotent on a clean cluster: %v", err)
	}
}
