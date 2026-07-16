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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// The point-in-time freeze gate of reconcileNamespaceManifestLeg: once the manifest leg is planned (the
// MCR name is published) the reconciler must NOT re-list the live namespace or recompute spec.targets —
// it short-circuits to a plain requeue until main latches manifestCaptured. The end-to-end frozen-plan
// invariant is pinned by the integration spec in test/integration/snapshot_capture_plan_drift_test.go;
// this unit test pins the short-circuit itself: the gate returns BEFORE any planning dependency is
// touched, which is proven by passing a reconciler with nil Dynamic/Discovery clients.
func TestReconcileNamespaceManifestLegFreezeGate(t *testing.T) {
	ctx := context.Background()
	boolPtrV := func(v bool) *bool { return &v }

	newRoot := func() *storagev1alpha1.Snapshot {
		return &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "root", UID: "root-uid"},
		}
	}

	t.Run("captured latch: non-requeuing done", func(t *testing.T) {
		nsSnap := newRoot()
		nsSnap.Status.CaptureState = &storagev1alpha1.CaptureStateStatus{
			CommonController: &storagev1alpha1.CommonControllerCaptureState{ManifestCaptured: boolPtrV(true)},
		}
		r := &SnapshotReconciler{}
		res, err := r.reconcileNamespaceManifestLeg(ctx, nsSnap, nil, NewNamespaceSnapshotAdapter(nsSnap), nil)
		if err != nil {
			t.Fatalf("captured leg must return nil error, got %v", err)
		}
		if res.RequeueAfter != 0 || res.Requeue {
			t.Fatalf("captured leg must not requeue, got %+v", res)
		}
	})

	t.Run("planned leg (MCR name published): requeues without touching planning clients", func(t *testing.T) {
		nsSnap := newRoot()
		nsSnap.Status.CaptureState = &storagev1alpha1.CaptureStateStatus{
			CommonController: &storagev1alpha1.CommonControllerCaptureState{ManifestCaptured: boolPtrV(false)},
			DomainSpecificController: &storagev1alpha1.DomainSpecificControllerCaptureState{
				ManifestCaptureRequestName: "nss-mcr-frozen",
			},
		}
		// Dynamic/Discovery are nil: if the gate did NOT short-circuit, the reconciler would error on the
		// nil-client guard (or panic later) instead of returning the plain requeue asserted here.
		r := &SnapshotReconciler{}
		res, err := r.reconcileNamespaceManifestLeg(ctx, nsSnap, nil, NewNamespaceSnapshotAdapter(nsSnap), nil)
		if err != nil {
			t.Fatalf("planned leg must short-circuit with nil error, got %v", err)
		}
		if res.RequeueAfter != 500*time.Millisecond {
			t.Fatalf("planned leg must requeue (500ms) until the captured latch flips, got %+v", res)
		}
	})

	t.Run("unplanned leg proceeds to planning (hits the nil-client guard)", func(t *testing.T) {
		nsSnap := newRoot() // no MCR name, not captured -> ManifestCaptureNeeded=true
		r := &SnapshotReconciler{}
		_, err := r.reconcileNamespaceManifestLeg(ctx, nsSnap, nil, NewNamespaceSnapshotAdapter(nsSnap), nil)
		if err == nil {
			t.Fatal("unplanned leg with nil Dynamic/Discovery must fail on the client guard (the gate must NOT swallow it)")
		}
	})
}
