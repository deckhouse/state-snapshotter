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
	"testing"

	"github.com/slok/kubewebhook/v2/pkg/model"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

func setDiscoveryResources(client *fake.Clientset, resources ...metav1.APIResource) {
	client.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{
		GroupVersion: "v1",
		APIResources: resources,
	}}
}

// newFakeDynamicClient creates a fake dynamic client with test resources
// Resources must be created with proper namespace and name already set
func newFakeDynamicClient(resources map[string]map[string]runtime.Object) dynamic.Interface {
	// Create a scheme with core v1 resources
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	// Build a list of runtime objects
	// Note: objects should already have namespace and name set correctly
	var objects []runtime.Object
	for _, nsResources := range resources {
		for _, obj := range nsResources {
			objects = append(objects, obj)
		}
	}

	return dynamicfake.NewSimpleDynamicClient(scheme, objects...)
}

func TestMCRValidate_ValidMCR(t *testing.T) {
	ctx := context.Background()

	// Setup mock Kubernetes client
	mockClient := fake.NewSimpleClientset()

	// Mock SubjectAccessReview to allow GET permission
	mockClient.PrependReactor("create", "subjectaccessreviews", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{
				Allowed: true,
			},
		}, nil
	})

	// Setup fake dynamic client with test ConfigMap in default namespace
	dynClient := newFakeDynamicClient(map[string]map[string]runtime.Object{
		"default": {
			"test-mcr-cm": &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr-cm",
					Namespace: "default",
				},
			},
		},
	})

	// Set the mock clients
	SetKubernetesClient(mockClient)
	SetDynamicClient(dynClient)

	// Create valid MCR
	mcr := &storagev1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mcr-test-ok",
			Namespace: "default",
		},
		Spec: storagev1alpha1.ManifestCaptureRequestSpec{
			Targets: []storagev1alpha1.ManifestTarget{
				{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "test-mcr-cm",
				},
			},
		},
	}

	arReview := &model.AdmissionReview{
		Operation: model.OperationCreate,
		UserInfo: authenticationv1.UserInfo{
			Username: "test-user",
			Groups:   []string{"system:authenticated"},
		},
	}

	result, err := MCRValidate(ctx, arReview, mcr)
	if err != nil {
		t.Fatalf("MCRValidate returned error: %v", err)
	}

	if !result.Valid {
		t.Errorf("Expected MCR to be valid, but got: %s", result.Message)
	}
}

func TestMCRValidate_RejectClusterScopedResource(t *testing.T) {
	ctx := context.Background()

	// Setup mock Kubernetes client
	mockClient := fake.NewSimpleClientset()
	setDiscoveryResources(mockClient, metav1.APIResource{Name: "nodes", Kind: "Node", Namespaced: false})
	dynClient := newFakeDynamicClient(map[string]map[string]runtime.Object{})
	SetKubernetesClient(mockClient)
	SetDynamicClient(dynClient)

	// Create MCR with cluster-scoped resource
	mcr := &storagev1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mcr-test-node",
			Namespace: "default",
		},
		Spec: storagev1alpha1.ManifestCaptureRequestSpec{
			Targets: []storagev1alpha1.ManifestTarget{
				{
					APIVersion: "v1",
					Kind:       "Node",
					Name:       "somenode",
				},
			},
		},
	}

	arReview := &model.AdmissionReview{
		Operation: model.OperationCreate,
		UserInfo: authenticationv1.UserInfo{
			Username: "test-user",
		},
	}

	// Mock SubjectAccessReview to allow (so we can test cluster-scoped rejection)
	mockClient.PrependReactor("create", "subjectaccessreviews", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{
				Allowed: true,
			},
		}, nil
	})

	result, err := MCRValidate(ctx, arReview, mcr)
	if err != nil {
		t.Fatalf("MCRValidate returned error: %v", err)
	}

	if result.Valid {
		t.Fatal("Expected cluster-scoped Node target to be rejected")
	}
	if !contains(result.Message, "cluster-scoped") {
		t.Fatalf("Expected cluster-scoped rejection, got: %s", result.Message)
	}
}

func TestMCRValidate_AllowsNamespaceTargetForNamespaceSnapshotBoundMCR(t *testing.T) {
	ctx := context.Background()

	mockClient := fake.NewSimpleClientset()
	setDiscoveryResources(mockClient, metav1.APIResource{Name: "namespaces", Kind: "Namespace", Namespaced: false})
	SetKubernetesClient(mockClient)
	SetDynamicClient(newFakeDynamicClient(map[string]map[string]runtime.Object{}))

	result, err := MCRValidate(ctx, testCreateAdmissionReview(), namespaceSnapshotBoundMCR("default", "default"))
	if err != nil {
		t.Fatalf("MCRValidate returned error: %v", err)
	}
	if !result.Valid {
		t.Fatalf("Expected NamespaceSnapshot-bound Namespace target to be valid, got: %s", result.Message)
	}
}

func TestMCRValidate_RejectsNamespaceTargetForDifferentNamespace(t *testing.T) {
	ctx := context.Background()

	mockClient := fake.NewSimpleClientset()
	setDiscoveryResources(mockClient, metav1.APIResource{Name: "namespaces", Kind: "Namespace", Namespaced: false})
	SetKubernetesClient(mockClient)
	SetDynamicClient(newFakeDynamicClient(map[string]map[string]runtime.Object{}))

	result, err := MCRValidate(ctx, testCreateAdmissionReview(), namespaceSnapshotBoundMCR("default", "other"))
	if err != nil {
		t.Fatalf("MCRValidate returned error: %v", err)
	}
	if result.Valid {
		t.Fatal("Expected Namespace target with name != MCR namespace to be rejected")
	}
	if !contains(result.Message, "cluster-scoped") {
		t.Fatalf("Expected cluster-scoped rejection, got: %s", result.Message)
	}
}

func TestMCRValidate_RejectsNamespaceTargetForOrdinaryMCR(t *testing.T) {
	ctx := context.Background()

	mockClient := fake.NewSimpleClientset()
	setDiscoveryResources(mockClient, metav1.APIResource{Name: "namespaces", Kind: "Namespace", Namespaced: false})
	SetKubernetesClient(mockClient)
	SetDynamicClient(newFakeDynamicClient(map[string]map[string]runtime.Object{}))

	mcr := &storagev1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mcr-ordinary",
			Namespace: "default",
		},
		Spec: storagev1alpha1.ManifestCaptureRequestSpec{
			Targets: []storagev1alpha1.ManifestTarget{{
				APIVersion: "v1",
				Kind:       "Namespace",
				Name:       "default",
			}},
		},
	}
	result, err := MCRValidate(ctx, testCreateAdmissionReview(), mcr)
	if err != nil {
		t.Fatalf("MCRValidate returned error: %v", err)
	}
	if result.Valid {
		t.Fatal("Expected ordinary MCR Namespace target to be rejected")
	}
	if !contains(result.Message, "cluster-scoped") {
		t.Fatalf("Expected cluster-scoped rejection, got: %s", result.Message)
	}
}

func TestMCRValidate_RejectsOtherClusterScopedTargetForNamespaceSnapshotBoundMCR(t *testing.T) {
	ctx := context.Background()

	mockClient := fake.NewSimpleClientset()
	setDiscoveryResources(mockClient, metav1.APIResource{Name: "nodes", Kind: "Node", Namespaced: false})
	SetKubernetesClient(mockClient)
	SetDynamicClient(newFakeDynamicClient(map[string]map[string]runtime.Object{}))

	mcr := namespaceSnapshotBoundMCR("default", "default")
	mcr.Spec.Targets[0] = storagev1alpha1.ManifestTarget{
		APIVersion: "v1",
		Kind:       "Node",
		Name:       "node-a",
	}
	result, err := MCRValidate(ctx, testCreateAdmissionReview(), mcr)
	if err != nil {
		t.Fatalf("MCRValidate returned error: %v", err)
	}
	if result.Valid {
		t.Fatal("Expected non-Namespace cluster-scoped target to be rejected")
	}
	if !contains(result.Message, "cluster-scoped") {
		t.Fatalf("Expected cluster-scoped rejection, got: %s", result.Message)
	}
}

func TestMCRValidate_RejectWithoutGETPermission(t *testing.T) {
	ctx := context.Background()

	// Setup mock Kubernetes client
	mockClient := fake.NewSimpleClientset()

	// Mock SubjectAccessReview to deny GET permission
	mockClient.PrependReactor("create", "subjectaccessreviews", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{
				Allowed: false,
				Reason:  "forbidden by RBAC",
			},
		}, nil
	})

	// Setup fake dynamic client with Secret in default namespace
	dynClient := newFakeDynamicClient(map[string]map[string]runtime.Object{
		"default": {
			"forbidden-secret": &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "forbidden-secret",
					Namespace: "default",
				},
			},
		},
	})

	SetKubernetesClient(mockClient)
	SetDynamicClient(dynClient)

	// Create MCR with resource user doesn't have access to
	mcr := &storagev1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mcr-test-rbac",
			Namespace: "default",
		},
		Spec: storagev1alpha1.ManifestCaptureRequestSpec{
			Targets: []storagev1alpha1.ManifestTarget{
				{
					APIVersion: "v1",
					Kind:       "Secret",
					Name:       "forbidden-secret",
				},
			},
		},
	}

	arReview := &model.AdmissionReview{
		Operation: model.OperationCreate,
		UserInfo: authenticationv1.UserInfo{
			Username: "test-user",
			Groups:   []string{"system:authenticated"},
		},
	}

	result, err := MCRValidate(ctx, arReview, mcr)
	if err != nil {
		t.Fatalf("MCRValidate returned error: %v", err)
	}

	if result.Valid {
		t.Error("Expected MCR to be rejected (no GET permission), but it was accepted")
	}

	if result.Message == "" {
		t.Error("Expected error message, but got empty message")
	}

	expectedMsg := "cannot GET"
	if !contains(result.Message, expectedMsg) {
		t.Errorf("Expected error message to contain %q, but got: %s", expectedMsg, result.Message)
	}
}

func TestMCRValidate_RejectSecretInDifferentNamespace(t *testing.T) {
	ctx := context.Background()

	// Setup mock Kubernetes client
	mockClient := fake.NewSimpleClientset()

	// Mock SubjectAccessReview to allow GET permission (this shouldn't matter, as we reject when resource not found)
	mockClient.PrependReactor("create", "subjectaccessreviews", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{
				Allowed: true,
			},
		}, nil
	})

	// Setup fake dynamic client with Secret in kube-system namespace (different from MCR namespace)
	// Since we only check in MCR namespace (default), this Secret won't be found
	dynClient := newFakeDynamicClient(map[string]map[string]runtime.Object{
		"kube-system": {
			"test-mcr-forbidden": &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr-forbidden",
					Namespace: "kube-system",
				},
			},
		},
	})

	SetKubernetesClient(mockClient)
	SetDynamicClient(dynClient)

	// Create MCR in default namespace, but Secret is in kube-system
	mcr := &storagev1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mcr-test-cross-ns",
			Namespace: "default",
		},
		Spec: storagev1alpha1.ManifestCaptureRequestSpec{
			Targets: []storagev1alpha1.ManifestTarget{
				{
					APIVersion: "v1",
					Kind:       "Secret",
					Name:       "test-mcr-forbidden",
				},
			},
		},
	}

	arReview := &model.AdmissionReview{
		Operation: model.OperationCreate,
		UserInfo: authenticationv1.UserInfo{
			Username: "test-user",
			Groups:   []string{"system:authenticated"},
		},
	}

	result, err := MCRValidate(ctx, arReview, mcr)
	if err != nil {
		t.Fatalf("MCRValidate returned error: %v", err)
	}

	if result.Valid {
		t.Error("Expected MCR to be rejected (Secret not found in MCR namespace), but it was accepted")
	}

	expectedMsg := "not found in namespace"
	if !contains(result.Message, expectedMsg) {
		t.Errorf("Expected error message to contain %q, but got: %s", expectedMsg, result.Message)
	}
}

func TestMCRValidate_EmptyTargets(t *testing.T) {
	ctx := context.Background()

	mockClient := fake.NewSimpleClientset()
	dynClient := newFakeDynamicClient(map[string]map[string]runtime.Object{})
	SetKubernetesClient(mockClient)
	SetDynamicClient(dynClient)

	mcr := &storagev1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mcr-empty",
			Namespace: "default",
		},
		Spec: storagev1alpha1.ManifestCaptureRequestSpec{
			Targets: []storagev1alpha1.ManifestTarget{},
		},
	}

	arReview := &model.AdmissionReview{
		Operation: model.OperationCreate,
		UserInfo: authenticationv1.UserInfo{
			Username: "test-user",
		},
	}

	result, err := MCRValidate(ctx, arReview, mcr)
	if err != nil {
		t.Fatalf("MCRValidate returned error: %v", err)
	}

	if result.Valid {
		t.Error("Expected MCR to be rejected (empty targets), but it was accepted")
	}

	expectedMsg := "At least one target must be specified"
	if result.Message != expectedMsg {
		t.Errorf("Expected error message %q, but got: %s", expectedMsg, result.Message)
	}
}

func TestMCRValidate_NoNamespace(t *testing.T) {
	ctx := context.Background()

	mockClient := fake.NewSimpleClientset()
	dynClient := newFakeDynamicClient(map[string]map[string]runtime.Object{})
	SetKubernetesClient(mockClient)
	SetDynamicClient(dynClient)

	mcr := &storagev1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mcr-no-namespace",
			// Namespace is empty
		},
		Spec: storagev1alpha1.ManifestCaptureRequestSpec{
			Targets: []storagev1alpha1.ManifestTarget{
				{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "test",
				},
			},
		},
	}

	arReview := &model.AdmissionReview{
		Operation: model.OperationCreate,
		UserInfo: authenticationv1.UserInfo{
			Username: "test-user",
		},
	}

	result, err := MCRValidate(ctx, arReview, mcr)
	if err != nil {
		t.Fatalf("MCRValidate returned error: %v", err)
	}

	if result.Valid {
		t.Error("Expected MCR to be rejected (no namespace), but it was accepted")
	}

	expectedMsg := "must be created in a namespace"
	if !contains(result.Message, expectedMsg) {
		t.Errorf("Expected error message to contain %q, but got: %s", expectedMsg, result.Message)
	}
}

func TestMCRValidate_DeleteOperation(t *testing.T) {
	ctx := context.Background()

	mockClient := fake.NewSimpleClientset()
	dynClient := newFakeDynamicClient(map[string]map[string]runtime.Object{})
	SetKubernetesClient(mockClient)
	SetDynamicClient(dynClient)

	now := metav1.Now()
	mcr := &storagev1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "mcr-delete",
			Namespace:         "default",
			DeletionTimestamp: &now,
		},
		Spec: storagev1alpha1.ManifestCaptureRequestSpec{
			Targets: []storagev1alpha1.ManifestTarget{
				{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "test",
				},
			},
		},
	}

	arReview := &model.AdmissionReview{
		Operation: model.OperationDelete,
		UserInfo: authenticationv1.UserInfo{
			Username: "test-user",
		},
	}

	result, err := MCRValidate(ctx, arReview, mcr)
	if err != nil {
		t.Fatalf("MCRValidate returned error: %v", err)
	}

	if !result.Valid {
		t.Errorf("Expected MCR delete to be valid (skip validation), but got: %s", result.Message)
	}
}

func TestMCRValidate_MultipleTargets(t *testing.T) {
	ctx := context.Background()

	mockClient := fake.NewSimpleClientset()

	// Mock SubjectAccessReview to allow GET permission for all
	mockClient.PrependReactor("create", "subjectaccessreviews", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{
				Allowed: true,
			},
		}, nil
	})

	// Setup fake dynamic client with test resources in default namespace
	dynClient := newFakeDynamicClient(map[string]map[string]runtime.Object{
		"default": {
			"cm1": &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cm1",
					Namespace: "default",
				},
			},
			"secret1": &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "secret1",
					Namespace: "default",
				},
			},
		},
	})

	SetKubernetesClient(mockClient)
	SetDynamicClient(dynClient)

	mcr := &storagev1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mcr-multiple",
			Namespace: "default",
		},
		Spec: storagev1alpha1.ManifestCaptureRequestSpec{
			Targets: []storagev1alpha1.ManifestTarget{
				{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "cm1",
				},
				{
					APIVersion: "v1",
					Kind:       "Secret",
					Name:       "secret1",
				},
			},
		},
	}

	arReview := &model.AdmissionReview{
		Operation: model.OperationCreate,
		UserInfo: authenticationv1.UserInfo{
			Username: "test-user",
		},
	}

	result, err := MCRValidate(ctx, arReview, mcr)
	if err != nil {
		t.Fatalf("MCRValidate returned error: %v", err)
	}

	if !result.Valid {
		t.Errorf("Expected MCR with multiple valid targets to be valid, but got: %s", result.Message)
	}
}

func testCreateAdmissionReview() *model.AdmissionReview {
	return &model.AdmissionReview{
		Operation: model.OperationCreate,
		UserInfo: authenticationv1.UserInfo{
			Username: "test-user",
			Groups:   []string{"system:authenticated"},
		},
	}
}

func namespaceSnapshotBoundMCR(namespace, targetName string) *storagev1alpha1.ManifestCaptureRequest {
	return &storagev1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mcr-ns-bound",
			Namespace: namespace,
			Annotations: map[string]string{
				boundNamespaceSnapshotContentAnnotation: "ns-content",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: namespaceSnapshotAPIVersion,
				Kind:       namespaceSnapshotKind,
				Name:       "root",
			}},
		},
		Spec: storagev1alpha1.ManifestCaptureRequestSpec{
			Targets: []storagev1alpha1.ManifestTarget{{
				APIVersion: "v1",
				Kind:       "Namespace",
				Name:       targetName,
			}},
		},
	}
}

// Helper function to check if string contains substring
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
