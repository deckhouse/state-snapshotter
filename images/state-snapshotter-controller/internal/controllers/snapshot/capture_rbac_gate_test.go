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

package snapshot

import (
	"context"
	"errors"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeSelfSubjectAccessReviewer records the last request and returns a canned allowed/err result.
type fakeSelfSubjectAccessReviewer struct {
	allowed bool
	err     error
	last    *authorizationv1.SelfSubjectAccessReview
}

func (f *fakeSelfSubjectAccessReviewer) Create(_ context.Context, sar *authorizationv1.SelfSubjectAccessReview, _ metav1.CreateOptions) (*authorizationv1.SelfSubjectAccessReview, error) {
	f.last = sar
	if f.err != nil {
		return nil, f.err
	}
	out := sar.DeepCopy()
	out.Status.Allowed = f.allowed
	return out, nil
}

func TestNamespaceCaptureRBACReady(t *testing.T) {
	t.Run("nil client skips the gate (returns true)", func(t *testing.T) {
		r := &SnapshotReconciler{}
		ready, err := r.namespaceCaptureRBACReady(context.Background(), "ns")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ready {
			t.Fatalf("expected ready=true when SARClient is nil")
		}
	})

	t.Run("allowed -> ready and reviews wildcard list in the namespace", func(t *testing.T) {
		fake := &fakeSelfSubjectAccessReviewer{allowed: true}
		r := &SnapshotReconciler{SARClient: fake}
		ready, err := r.namespaceCaptureRBACReady(context.Background(), "team-a")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ready {
			t.Fatalf("expected ready=true when SAR allows")
		}
		ra := fake.last.Spec.ResourceAttributes
		if ra == nil {
			t.Fatalf("expected ResourceAttributes to be set")
		}
		if ra.Namespace != "team-a" || ra.Verb != "list" || ra.Group != "*" || ra.Resource != "*" {
			t.Fatalf("unexpected ResourceAttributes: %+v", ra)
		}
	})

	t.Run("denied -> not ready (gate blocks the list)", func(t *testing.T) {
		r := &SnapshotReconciler{SARClient: &fakeSelfSubjectAccessReviewer{allowed: false}}
		ready, err := r.namespaceCaptureRBACReady(context.Background(), "ns")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ready {
			t.Fatalf("expected ready=false when SAR denies")
		}
	})

	t.Run("error is propagated", func(t *testing.T) {
		r := &SnapshotReconciler{SARClient: &fakeSelfSubjectAccessReviewer{err: errors.New("boom")}}
		if _, err := r.namespaceCaptureRBACReady(context.Background(), "ns"); err == nil {
			t.Fatalf("expected error to be propagated")
		}
	})
}
