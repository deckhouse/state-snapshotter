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

package handlers

import (
	"context"
	"errors"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

// newDynamicClientFailingGet returns a fake dynamic client whose every "get" fails with the given error, so
// a test can drive findResourceNamespace's error classification without needing a real API server.
func newDynamicClientFailingGet(getErr error) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	cl := dynamicfake.NewSimpleDynamicClient(scheme)
	cl.PrependReactor("get", "*", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, getErr
	})
	return cl
}

func forbiddenGetErr() error {
	return apierrors.NewForbidden(
		schema.GroupResource{Group: "acme.deckhouse.io", Resource: "widgets"},
		"w1",
		errors.New(`configmaps is forbidden: User "system:serviceaccount:d8-state-snapshotter:webhooks" cannot get resource`),
	)
}

// A 403 on the target Get means "this ServiceAccount may not read that resource", NOT "the target does not
// exist". Collapsing the two (the previous behaviour) rejected the MCR with a misleading
// "resource ... not found in namespace" while the real cause — a missing domain-read grant — was only
// visible in the webhook's own logs. The rejection must instead name the access denial.
func TestMCRValidate_ForbiddenTargetGetIsNotReportedAsNotFound(t *testing.T) {
	ctx := context.Background()

	mockClient := fake.NewSimpleClientset()
	mockClient.PrependReactor("create", "subjectaccessreviews", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true},
		}, nil
	})
	SetKubernetesClient(mockClient)
	SetDynamicClient(newDynamicClientFailingGet(forbiddenGetErr()))

	mcr := &storagev1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mcr-forbidden-target",
			Namespace: "default",
		},
		Spec: storagev1alpha1.ManifestCaptureRequestSpec{
			Targets: []storagev1alpha1.ManifestTarget{{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Name:       "some-cm",
			}},
		},
	}

	result, err := MCRValidate(ctx, testCreateAdmissionReview(), mcr)
	if err != nil {
		t.Fatalf("MCRValidate returned error: %v", err)
	}
	if result.Valid {
		t.Fatal("a target whose Get is denied must not be accepted")
	}
	if contains(result.Message, "not found") {
		t.Fatalf("an RBAC denial must not be reported as a missing target, got: %s", result.Message)
	}
	if !contains(result.Message, "access denied") {
		t.Fatalf("expected the rejection to name the access denial, got: %s", result.Message)
	}
}

// findResourceNamespace must classify the error itself: NotFound is an authoritative "absent" answer,
// Forbidden is a configuration error surfaced to the caller, and anything else stays non-fatal.
func TestFindResourceNamespaceErrorClassification(t *testing.T) {
	ctx := context.Background()
	gv := schema.GroupVersion{Group: "acme.deckhouse.io", Version: "v1"}

	t.Run("forbidden returns an error naming the resource", func(t *testing.T) {
		SetDynamicClient(newDynamicClientFailingGet(forbiddenGetErr()))

		ns, err := findResourceNamespace(ctx, gv, "widgets", "w1", "default")
		if err == nil {
			t.Fatal("expected an error for a denied Get")
		}
		if ns != "" {
			t.Errorf("namespace = %q, want empty on error", ns)
		}
		if !contains(err.Error(), "widgets") || !contains(err.Error(), "acme.deckhouse.io") {
			t.Errorf("error must name the resource and group, got: %v", err)
		}
	})

	t.Run("not found is an authoritative absent answer", func(t *testing.T) {
		SetDynamicClient(newFakeDynamicClient(map[string]map[string]runtime.Object{}))

		ns, err := findResourceNamespace(ctx, gv, "widgets", "missing", "default")
		if err != nil {
			t.Fatalf("NotFound must not be an error, got: %v", err)
		}
		if ns != "" {
			t.Errorf("namespace = %q, want empty for an absent target", ns)
		}
	})

	t.Run("other failures stay non-fatal", func(t *testing.T) {
		SetDynamicClient(newDynamicClientFailingGet(apierrors.NewServiceUnavailable("apiserver is having a moment")))

		ns, err := findResourceNamespace(ctx, gv, "widgets", "w1", "default")
		if err != nil {
			t.Fatalf("a transient failure must stay non-fatal, got: %v", err)
		}
		if ns != "" {
			t.Errorf("namespace = %q, want empty", ns)
		}
	})
}

// The classification keys off the typed API error rather than string matching, so a Forbidden is never
// mistaken for a NotFound.
func TestForbiddenClassificationUsesTypedError(t *testing.T) {
	forbidden := forbiddenGetErr()
	if !apierrors.IsForbidden(forbidden) {
		t.Fatal("expected IsForbidden to recognise a constructed Forbidden error")
	}
	if apierrors.IsNotFound(forbidden) {
		t.Fatal("a Forbidden error must not satisfy IsNotFound")
	}
}
