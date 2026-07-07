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

package genericbinder

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// Barrier 2 (ADR §6.2): the binder finalizes a domain snapshot's user-facing Ready ONLY after the domain
// reported captureState.domainSpecificController.phase=Finished. While the content is Ready=True but the
// domain is still at Planning/Planned (running consistency actions), Ready is held False/ChildrenPending;
// a non-domain owner (no phase) mirrors the content verbatim; a domain phase=Failed still bubbles first.
func TestCheckConsistencyAndSetReady_Barrier2FinishedGate(t *testing.T) {
	tests := []struct {
		name          string
		contentStatus metav1.ConditionStatus
		contentReason string
		phase         string // captureState.domainSpecificController.phase ("" = non-domain owner)
		failReason    string // captureState.domainSpecificController.reason (phase=Failed only)
		failMessage   string
		wantStatus    metav1.ConditionStatus
		wantReason    string
	}{
		{
			name:          "domain phase Planned holds Ready (barrier 2 not reached)",
			contentStatus: metav1.ConditionTrue,
			contentReason: snapshot.ReasonCompleted,
			phase:         string(storagev1alpha1.SnapshotCapturePhasePlanned),
			wantStatus:    metav1.ConditionFalse,
			wantReason:    snapshot.ReasonChildrenPending,
		},
		{
			name:          "domain phase Finished finalizes Ready",
			contentStatus: metav1.ConditionTrue,
			contentReason: snapshot.ReasonCompleted,
			phase:         string(storagev1alpha1.SnapshotCapturePhaseFinished),
			wantStatus:    metav1.ConditionTrue,
			wantReason:    snapshot.ReasonCompleted,
		},
		{
			name:          "non-domain owner (no phase) mirrors content verbatim",
			contentStatus: metav1.ConditionTrue,
			contentReason: snapshot.ReasonCompleted,
			phase:         "",
			wantStatus:    metav1.ConditionTrue,
			wantReason:    snapshot.ReasonCompleted,
		},
		{
			name:          "content not ready is unaffected by the finished gate",
			contentStatus: metav1.ConditionFalse,
			contentReason: snapshot.ReasonChildrenPending,
			phase:         string(storagev1alpha1.SnapshotCapturePhasePlanned),
			wantStatus:    metav1.ConditionFalse,
			wantReason:    snapshot.ReasonChildrenPending,
		},
		{
			name:          "domain phase Failed bubbles ahead of the finished gate",
			contentStatus: metav1.ConditionTrue,
			contentReason: snapshot.ReasonCompleted,
			phase:         string(storagev1alpha1.SnapshotCapturePhaseFailed),
			failReason:    "SourceNotFound",
			failMessage:   "source PVC gone",
			wantStatus:    metav1.ConditionFalse,
			wantReason:    "SourceNotFound",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			if err := storagev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatalf("add storage scheme: %v", err)
			}

			content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root-content"}}
			meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{
				Type:    snapshot.ConditionReady,
				Status:  tt.contentStatus,
				Reason:  tt.contentReason,
				Message: "content",
			})

			snapGVK := storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot")
			snapObj := &unstructured.Unstructured{}
			snapObj.SetGroupVersionKind(snapGVK)
			snapObj.SetName("root-snap")
			snapObj.SetNamespace("default")
			if err := unstructured.SetNestedField(snapObj.Object, "root-content", "status", "boundSnapshotContentName"); err != nil {
				t.Fatalf("set boundSnapshotContentName: %v", err)
			}
			if tt.phase != "" {
				if err := unstructured.SetNestedField(snapObj.Object, tt.phase, "status", "captureState", "domainSpecificController", "phase"); err != nil {
					t.Fatalf("set phase: %v", err)
				}
			}
			if tt.failReason != "" {
				if err := unstructured.SetNestedField(snapObj.Object, tt.failReason, "status", "captureState", "domainSpecificController", "reason"); err != nil {
					t.Fatalf("set reason: %v", err)
				}
			}
			if tt.failMessage != "" {
				if err := unstructured.SetNestedField(snapObj.Object, tt.failMessage, "status", "captureState", "domainSpecificController", "message"); err != nil {
					t.Fatalf("set message: %v", err)
				}
			}

			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(content, snapObj).
				WithStatusSubresource(snapObj).
				Build()

			reg := snapshot.NewGVKRegistry()
			if err := reg.RegisterSnapshotContentMapping(
				"Snapshot", storagev1alpha1.SchemeGroupVersion.String(),
				"SnapshotContent", storagev1alpha1.SchemeGroupVersion.String(),
			); err != nil {
				t.Fatalf("register snapshot/content mapping: %v", err)
			}
			r := &GenericSnapshotBinderController{Client: cl, APIReader: cl, Scheme: scheme, GVKRegistry: reg}

			snapLike, err := snapshot.ExtractSnapshotLike(snapObj)
			if err != nil {
				t.Fatalf("extract snapshot like: %v", err)
			}
			if err := r.checkConsistencyAndSetReady(ctx, snapLike, snapObj); err != nil {
				t.Fatalf("checkConsistencyAndSetReady: %v", err)
			}

			fresh := &unstructured.Unstructured{}
			fresh.SetGroupVersionKind(snapGVK)
			if err := cl.Get(ctx, client.ObjectKey{Namespace: "default", Name: "root-snap"}, fresh); err != nil {
				t.Fatalf("get snapshot: %v", err)
			}
			freshLike, err := snapshot.ExtractSnapshotLike(fresh)
			if err != nil {
				t.Fatalf("extract fresh snapshot like: %v", err)
			}
			got := snapshot.GetCondition(freshLike, snapshot.ConditionReady)
			if got == nil {
				t.Fatalf("snapshot has no Ready condition after derive")
			}
			if got.Status != tt.wantStatus || got.Reason != tt.wantReason {
				t.Fatalf("Ready = %s/%s, want %s/%s", got.Status, got.Reason, tt.wantStatus, tt.wantReason)
			}
		})
	}
}
