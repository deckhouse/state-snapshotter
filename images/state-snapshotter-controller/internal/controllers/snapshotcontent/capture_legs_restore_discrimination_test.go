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
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// captureLegsOwnerWithDeclaredLegs is the owner as the ordinary path leaves it: the core has declared both
// legs open (false) and the domain has published its plan. It is the state every healthy capture passes
// through, and the one a restore must not be confused with.
func captureLegsOwnerWithDeclaredLegs(declared ...string) *unstructured.Unstructured {
	o := captureLegsOwner(clSnapGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))
	for _, leg := range declared {
		_ = unstructured.SetNestedField(o.Object, false, "status", "captureState", "commonController", leg)
	}
	return o
}

// A capture that finishes normally is not a restore. The legs still latch true — that is the ordinary
// path and it must keep working — but neither the counter nor the event may fire, because both name a
// recovery from a status replace and an operator reads them as one. Counting the ordinary latch would
// give the metric a floor that rises with plain capture traffic, which is exactly what makes it useless
// as a detector of the thing it is named after.
//
// The metric is process-global, so these subtests read it under one sequential parent and do not call
// t.Parallel().
func TestReconcileOwnerCaptureLegs_OrdinaryLatchIsNotARestore(t *testing.T) {
	for _, tc := range []struct {
		name          string
		owner         *unstructured.Unstructured
		wantRestores  float64
		wantEventLegs []string
	}{
		{
			name:         "ordinary: both legs declared open, capture completes",
			owner:        captureLegsOwnerWithDeclaredLegs("manifestCaptured", "dataCaptured"),
			wantRestores: 0,
		},
		{
			name:         "ordinary: manifest leg declared open, data leg declared open",
			owner:        captureLegsOwnerWithDeclaredLegs("dataCaptured", "manifestCaptured"),
			wantRestores: 0,
		},
		{
			name:          "restore: both legs absent beside a durable artifact",
			owner:         captureLegsOwnerWithDeclaredLegs(),
			wantRestores:  1,
			wantEventLegs: []string{"manifestCaptured", "dataCaptured"},
		},
		{
			name:          "restore: one leg survived the replace, the other did not",
			owner:         captureLegsOwnerWithDeclaredLegs("manifestCaptured"),
			wantRestores:  1,
			wantEventLegs: []string{"dataCaptured"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := captureLegsScheme(t)
			rec := record.NewFakeRecorder(8)
			registerSnapshotContentMetrics()
			before := testutil.ToFloat64(captureStateRestoredTotal)

			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(tc.owner).
				WithObjects(tc.owner, captureLegsContentTyped(clMCPName, captureLegsData()), captureLegsReadyOwnedMCP()).
				Build()
			r := captureLegsController(cl, clSnapGVK, true)
			r.Recorder = rec

			if _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil)); err != nil {
				t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
			}

			// Whatever the accounting says, the object must end up correct: a captured leg is latched.
			for _, leg := range []string{"manifestCaptured", "dataCaptured"} {
				if got, found := captureLegsOwnerLatch(t, cl, clSnapGVK, leg); !found || !got {
					t.Fatalf("%s must be latched true after the capture, got found=%v value=%v", leg, found, got)
				}
			}

			if delta := testutil.ToFloat64(captureStateRestoredTotal) - before; delta != tc.wantRestores {
				t.Fatalf("restore counter delta = %v, want %v", delta, tc.wantRestores)
			}
			assertRestoreEvent(t, rec, tc.wantEventLegs)
		})
	}
}

// assertRestoreEvent requires exactly one CaptureStateRestored event naming every leg in wantLegs when
// wantLegs is non-empty, and no event at all when it is empty. A leg NOT in wantLegs must not be named:
// an event that lists a leg the core merely latched would mislead the same way the counter would.
func assertRestoreEvent(t *testing.T, rec *record.FakeRecorder, wantLegs []string) {
	t.Helper()
	var events []string
	for {
		select {
		case ev := <-rec.Events:
			events = append(events, ev)
			continue
		default:
		}
		break
	}
	if len(wantLegs) == 0 {
		for _, ev := range events {
			if strings.Contains(ev, EventReasonCaptureStateRestored) {
				t.Fatalf("an ordinary capture must emit no %s event, got %q", EventReasonCaptureStateRestored, ev)
			}
		}
		return
	}
	var restoreEvents []string
	for _, ev := range events {
		if strings.Contains(ev, EventReasonCaptureStateRestored) {
			restoreEvents = append(restoreEvents, ev)
		}
	}
	if len(restoreEvents) != 1 {
		t.Fatalf("want exactly one %s event, got %d: %v", EventReasonCaptureStateRestored, len(restoreEvents), restoreEvents)
	}
	ev := restoreEvents[0]
	for _, leg := range wantLegs {
		if !strings.Contains(ev, leg) {
			t.Fatalf("event %q must name the restored leg %s", ev, leg)
		}
	}
	for _, leg := range []string{"manifestCaptured", "dataCaptured"} {
		if slices.Contains(wantLegs, leg) {
			continue
		}
		if strings.Contains(ev, leg) {
			t.Fatalf("event %q names %s, which was declared and merely latched, not restored", ev, leg)
		}
	}
}
