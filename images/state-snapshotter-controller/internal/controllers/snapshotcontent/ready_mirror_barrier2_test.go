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

package snapshotcontent

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// Barrier 2 in the content-controller post-bind writer: mirrorReadyToOwnerSnapshot finalizes the owning
// Snapshot's Ready=True ONLY after the domain reported phase=Finished. While the content is Ready=True but
// the domain is still Planning/Planned, the mirror holds Ready=False/ChildrenPending; a non-domain owner
// (no phase) mirrors verbatim; a domain phase=Failed bubbles ahead of the finished gate. This is the
// content-side twin of the binder gate so both post-bind writers agree during the staged split.
func TestMirrorReadyToOwnerSnapshot_Barrier2FinishedGate(t *testing.T) {
	tests := []struct {
		name        string
		phase       string
		failReason  string
		failMessage string
		wantStatus  metav1.ConditionStatus
		wantReason  string
	}{
		{
			name:       "domain phase Planned holds Ready",
			phase:      string(storagev1alpha1.SnapshotCapturePhasePlanned),
			wantStatus: metav1.ConditionFalse,
			wantReason: snapshot.ReasonChildrenPending,
		},
		{
			name:       "domain phase Finished finalizes Ready",
			phase:      string(storagev1alpha1.SnapshotCapturePhaseFinished),
			wantStatus: metav1.ConditionTrue,
			wantReason: snapshot.ReasonCompleted,
		},
		{
			name:       "non-domain owner (no phase) mirrors verbatim",
			phase:      "",
			wantStatus: metav1.ConditionTrue,
			wantReason: snapshot.ReasonCompleted,
		},
		{
			name:        "domain phase Failed bubbles ahead of the finished gate",
			phase:       string(storagev1alpha1.SnapshotCapturePhaseFailed),
			failReason:  "SourceNotFound",
			failMessage: "source PVC gone",
			wantStatus:  metav1.ConditionFalse,
			wantReason:  "SourceNotFound",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			if err := storagev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatalf("add storage scheme: %v", err)
			}

			snapGVK := storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot")
			owner := &unstructured.Unstructured{}
			owner.SetGroupVersionKind(snapGVK)
			owner.SetNamespace("ns1")
			owner.SetName("owner")
			if err := unstructured.SetNestedField(owner.Object, "root", "status", "boundSnapshotContentName"); err != nil {
				t.Fatalf("set boundSnapshotContentName: %v", err)
			}
			if tt.phase != "" {
				if err := unstructured.SetNestedField(owner.Object, tt.phase, "status", "captureState", "domainSpecificController", "phase"); err != nil {
					t.Fatalf("set phase: %v", err)
				}
			}
			if tt.failReason != "" {
				if err := unstructured.SetNestedField(owner.Object, tt.failReason, "status", "captureState", "domainSpecificController", "reason"); err != nil {
					t.Fatalf("set reason: %v", err)
				}
			}
			if tt.failMessage != "" {
				if err := unstructured.SetNestedField(owner.Object, tt.failMessage, "status", "captureState", "domainSpecificController", "message"); err != nil {
					t.Fatalf("set message: %v", err)
				}
			}

			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(owner).
				WithStatusSubresource(owner).
				Build()
			r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

			contentObj := orphanGateContent(t, orphanGateOpts{name: "root", ownerNS: "ns1", ownerName: "owner", readyTrue: true})
			if err := r.mirrorReadyToOwnerSnapshot(ctx, contentObj); err != nil {
				t.Fatalf("mirrorReadyToOwnerSnapshot: %v", err)
			}

			fresh := &unstructured.Unstructured{}
			fresh.SetGroupVersionKind(snapGVK)
			if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns1", Name: "owner"}, fresh); err != nil {
				t.Fatalf("get owner: %v", err)
			}
			freshLike, err := snapshot.ExtractSnapshotLike(fresh)
			if err != nil {
				t.Fatalf("extract snapshot like: %v", err)
			}
			got := snapshot.GetCondition(freshLike, snapshot.ConditionReady)
			if got == nil {
				t.Fatalf("owner has no Ready condition after mirror")
			}
			if got.Status != tt.wantStatus || got.Reason != tt.wantReason {
				t.Fatalf("Ready = %s/%s, want %s/%s", got.Status, got.Reason, tt.wantStatus, tt.wantReason)
			}
		})
	}
}
