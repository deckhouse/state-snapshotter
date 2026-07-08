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
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

// plannedNamespaceRoot builds a namespace-root Snapshot already past barrier 1 (domain phase=Planned) —
// the state at which the manifest leg (where the unreadable-plan check lives) runs.
func plannedNamespaceRoot(t *testing.T) *storagev1alpha1.Snapshot {
	t.Helper()
	snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"}}
	snap.Status.CaptureState = &storagev1alpha1.CaptureStateStatus{
		DomainSpecificController: &storagev1alpha1.DomainSpecificControllerCaptureState{
			Phase: storagev1alpha1.SnapshotCapturePhasePlanned,
		},
	}
	return snap
}

func newUnreadablePlanReconciler(t *testing.T, snap *storagev1alpha1.Snapshot, rec record.EventRecorder) (*SnapshotReconciler, snapshotsdk.CaptureSDK) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap).WithStatusSubresource(snap).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl, Recorder: rec}
	sdk := snapshotsdk.New(cl, cl, snapshotsdk.NewStorageFoundationProvider(cl))
	return r, sdk
}

// TestReportUnreadableNamespacePlan_PublishesDomainMessageAndEvent asserts the fail-closed unreadable plan
// is surfaced through the domain-owned message channel + a Warning Event, WITHOUT the domain writing Ready
// and WITHOUT regressing the lifecycle phase.
func TestReportUnreadableNamespacePlan_PublishesDomainMessageAndEvent(t *testing.T) {
	ctx := context.Background()
	snap := plannedNamespaceRoot(t)
	rec := record.NewFakeRecorder(10)
	r, sdk := newUnreadablePlanReconciler(t, snap, rec)
	adapter := NewNamespaceSnapshotAdapter(snap)

	unreadable := []schema.GroupVersionResource{
		{Group: "apps", Version: "v1", Resource: "deployments"},
		{Group: "", Version: "v1", Resource: "secrets"},
	}
	if err := r.reportUnreadableNamespacePlan(ctx, snap, adapter, sdk, unreadable); err != nil {
		t.Fatalf("reportUnreadableNamespacePlan: %v", err)
	}

	fresh := &storagev1alpha1.Snapshot{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "ns1", Name: "root"}, fresh); err != nil {
		t.Fatalf("get root: %v", err)
	}
	cs := fresh.Status.CaptureState
	if cs == nil || cs.DomainSpecificController == nil {
		t.Fatalf("domain capture state missing after report")
	}
	msg := cs.DomainSpecificController.Message
	for _, want := range []string{"deployments", "secrets", "not readable"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("domain message %q does not contain %q", msg, want)
		}
	}
	// Phase must be preserved (never regressed) and no terminal reason written by an observability-only report.
	if cs.DomainSpecificController.Phase != storagev1alpha1.SnapshotCapturePhasePlanned {
		t.Fatalf("phase = %q, want Planned (preserved)", cs.DomainSpecificController.Phase)
	}
	if cs.DomainSpecificController.Reason != "" {
		t.Fatalf("domain reason = %q, want empty (non-terminal diagnostic)", cs.DomainSpecificController.Reason)
	}
	// The ns domain must NOT co-write the core-owned Ready condition.
	if got := meta.FindStatusCondition(fresh.Status.Conditions, snapshotpkg.ConditionReady); got != nil {
		t.Fatalf("Ready condition was written by the domain (%s/%s); it must be left to the core", got.Status, got.Reason)
	}

	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, eventReasonNamespacePlanUnreadable) {
			t.Fatalf("event %q missing reason %q", ev, eventReasonNamespacePlanUnreadable)
		}
		if !strings.Contains(ev, "Warning") || !strings.Contains(ev, "deployments") {
			t.Fatalf("event %q missing Warning type or the GVR list", ev)
		}
	default:
		t.Fatalf("expected a Warning Event, got none")
	}
}

// TestReportUnreadableNamespacePlan_IdempotentOnSameSet asserts the 500ms fail-closed requeue does not
// flood: an unchanged unreadable set neither re-emits the Event nor re-writes the status.
func TestReportUnreadableNamespacePlan_IdempotentOnSameSet(t *testing.T) {
	ctx := context.Background()
	snap := plannedNamespaceRoot(t)
	rec := record.NewFakeRecorder(10)
	r, sdk := newUnreadablePlanReconciler(t, snap, rec)
	adapter := NewNamespaceSnapshotAdapter(snap)

	unreadable := []schema.GroupVersionResource{{Group: "apps", Version: "v1", Resource: "deployments"}}
	if err := r.reportUnreadableNamespacePlan(ctx, snap, adapter, sdk, unreadable); err != nil {
		t.Fatalf("first report: %v", err)
	}
	select {
	case <-rec.Events:
	default:
		t.Fatalf("expected the first Event")
	}

	// Re-read into the same object the reconciler holds (mirrors the per-reconcile re-read), then report the
	// same set again: the published message is unchanged, so nothing fires.
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "ns1", Name: "root"}, snap); err != nil {
		t.Fatalf("re-get root: %v", err)
	}
	if err := r.reportUnreadableNamespacePlan(ctx, snap, adapter, sdk, unreadable); err != nil {
		t.Fatalf("second report: %v", err)
	}
	select {
	case ev := <-rec.Events:
		t.Fatalf("unexpected second Event on unchanged set: %q", ev)
	default:
	}
}

// TestClearUnreadableNamespacePlanDiagnostic asserts a recovered plan clears ONLY this leg's own stale
// diagnostic and leaves a foreign domain message untouched.
func TestClearUnreadableNamespacePlanDiagnostic(t *testing.T) {
	ctx := context.Background()

	t.Run("clears own stale diagnostic", func(t *testing.T) {
		snap := plannedNamespaceRoot(t)
		snap.Status.CaptureState.DomainSpecificController.Message = formatUnreadableNamespacePlan(
			[]schema.GroupVersionResource{{Group: "apps", Version: "v1", Resource: "deployments"}})
		r, sdk := newUnreadablePlanReconciler(t, snap, nil)
		adapter := NewNamespaceSnapshotAdapter(snap)
		if err := r.clearUnreadableNamespacePlanDiagnostic(ctx, snap, adapter, sdk); err != nil {
			t.Fatalf("clear: %v", err)
		}
		fresh := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "ns1", Name: "root"}, fresh); err != nil {
			t.Fatalf("get: %v", err)
		}
		if got := fresh.Status.CaptureState.DomainSpecificController.Message; got != "" {
			t.Fatalf("message = %q, want cleared", got)
		}
	})

	t.Run("leaves a foreign message untouched", func(t *testing.T) {
		snap := plannedNamespaceRoot(t)
		const foreign = "some other domain message"
		snap.Status.CaptureState.DomainSpecificController.Message = foreign
		r, sdk := newUnreadablePlanReconciler(t, snap, nil)
		adapter := NewNamespaceSnapshotAdapter(snap)
		if err := r.clearUnreadableNamespacePlanDiagnostic(ctx, snap, adapter, sdk); err != nil {
			t.Fatalf("clear: %v", err)
		}
		fresh := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "ns1", Name: "root"}, fresh); err != nil {
			t.Fatalf("get: %v", err)
		}
		if got := fresh.Status.CaptureState.DomainSpecificController.Message; got != foreign {
			t.Fatalf("message = %q, want %q (untouched)", got, foreign)
		}
	})
}

// TestFormatUnreadableNamespacePlan_CapsAndSorts asserts the message lists at most maxReportedUnreadableGVRs
// verbatim (sorted), then a "(+N more)" tail, and always reports the full count.
func TestFormatUnreadableNamespacePlan_CapsAndSorts(t *testing.T) {
	total := maxReportedUnreadableGVRs + 5
	unreadable := make([]schema.GroupVersionResource, 0, total)
	// Insert in reverse-sorted order to prove the output is sorted.
	for i := total - 1; i >= 0; i-- {
		unreadable = append(unreadable, schema.GroupVersionResource{Group: "g", Version: "v1", Resource: fmt.Sprintf("res%02d", i)})
	}
	msg := formatUnreadableNamespacePlan(unreadable)

	if !strings.Contains(msg, fmt.Sprintf("%d resource type(s)", total)) {
		t.Fatalf("message %q missing full count %d", msg, total)
	}
	if !strings.Contains(msg, fmt.Sprintf("(+%d more)", total-maxReportedUnreadableGVRs)) {
		t.Fatalf("message %q missing (+N more) tail", msg)
	}
	// The first (lowest-sorted) resource must be shown; a resource beyond the cap must not.
	if !strings.Contains(msg, "res00") {
		t.Fatalf("message %q missing first sorted GVR res00", msg)
	}
	if strings.Contains(msg, fmt.Sprintf("res%02d", total-1)) {
		t.Fatalf("message %q must not list the last GVR beyond the cap", msg)
	}
}
