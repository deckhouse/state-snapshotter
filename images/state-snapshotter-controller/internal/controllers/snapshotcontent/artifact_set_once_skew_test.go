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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

// The set-once artifact spans two modules, so the two deploy orders have to be survivable. Neither is a
// supported configuration to run in — the module gate is what keeps a new storage-foundation off an old
// state-snapshotter — but a cluster passes through them during an upgrade, and what must not happen in
// either is a moved artifact or a wedged reconcile.

// New core, old two-valued domain SDK: the domain reads a wiped latch as "not captured" and creates a
// fresh ManifestCaptureRequest. The core neither follows it nor jams: the published pointer stays on the
// original checkpoint, the leg latch is restored from that pointer, and the stray request is reaped
// without a requeue. The visible cost is exactly what the design accepts — one wasted request.
func TestVersionSkew_OldDomainRecreatesRequest_PointerStaysAndRequestIsReaped(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)

	// The state an old SDK leaves behind: captureState wiped by a typed writer, then a NEW MCR created and
	// its name published, while the content still names the original Ready checkpoint.
	owner := captureLegsOwner(clSnapGVK, projTestContent, "mcr-recreated-by-old-sdk", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))
	strayMCR := &ssv1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: projTestNS, Name: "mcr-recreated-by-old-sdk"},
	}
	strayMCR.Status.CheckpointName = "mcp-from-the-second-capture"

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped(clMCPName, nil), captureLegsReadyOwnedMCP(), strayMCR).
		Build()
	r := captureLegsController(cl, clSnapGVK, false)

	requeue, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil))
	if err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}
	if requeue {
		t.Fatal("a stray request from an old SDK must not leave the leg requeuing forever")
	}
	if got, found := captureLegsOwnerLatch(t, cl, clSnapGVK, "manifestCaptured"); !found || !got {
		t.Fatalf("the latch must be restored from the published pointer, got found=%v value=%v", found, got)
	}
	content := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: projTestContent}, content); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if content.Status.ManifestCheckpointName != clMCPName {
		t.Fatalf("the published pointer must not follow the second capture, got %q want %q",
			content.Status.ManifestCheckpointName, clMCPName)
	}
	err = cl.Get(ctx, client.ObjectKey{Namespace: projTestNS, Name: "mcr-recreated-by-old-sdk"}, &ssv1alpha1.ManifestCaptureRequest{})
	if err == nil {
		t.Fatal("the stray request must be reaped once the content durably owns a Ready checkpoint")
	}
	if client.IgnoreNotFound(err) != nil {
		t.Fatalf("get stray MCR: %v", err)
	}
}

// New domain SDK, old core: an old core writes the leg latch as an explicit false (its eager-init did
// exactly that), which the three-valued reading takes as CaptureLegPlan. So the new domain acts rather
// than waits — the deploy order that would be dangerous is the one where a domain waits for a declaration
// that never comes, and an explicit false is a declaration.
func TestVersionSkew_OldCoreDeclaresExplicitFalse_NewDomainActs(t *testing.T) {
	ctx := context.Background()
	scheme := captureLegsScheme(t)

	owner := captureLegsOwner(clSnapGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))
	if err := unstructured.SetNestedField(owner.Object, false, "status", "captureState", "commonController", "manifestCaptured"); err != nil {
		t.Fatalf("seed the old core's eager false: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, captureLegsContentTyped("", nil)).
		Build()
	r := captureLegsController(cl, clSnapGVK, false)

	if _, err := r.reconcileOwnerCaptureLegs(ctx, captureLegsContentObj(clSnapGVK, nil)); err != nil {
		t.Fatalf("reconcileOwnerCaptureLegs: %v", err)
	}
	// Nothing to restore (no pointer yet) and nothing to reap: the declaration the domain needs is simply
	// left in place, false.
	got, found := captureLegsOwnerLatch(t, cl, clSnapGVK, "manifestCaptured")
	if !found {
		t.Fatal("an explicit false declaration must survive the core pass (a domain would otherwise wait forever)")
	}
	if got {
		t.Fatal("nothing was captured, so the latch must stay false")
	}
}

// While ManifestsReady is pending, the reason stays ManifestCapturePending — that set is a contract — but
// the message must tell the two waits apart: a capture that was asked for and has not landed, versus a leg
// the core has not declared, in which no request exists to wait for at all.
func TestManifestPendingMessageDistinguishesUndeclaredLegFromPendingCapture(t *testing.T) {
	r := captureLegsController(fake.NewClientBuilder().WithScheme(captureLegsScheme(t)).Build(), clSnapGVK, false)

	declared := captureLegsOwner(clSnapGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))
	if err := unstructured.SetNestedField(declared.Object, false, "status", "captureState", "commonController", "manifestCaptured"); err != nil {
		t.Fatalf("declare the leg: %v", err)
	}
	undeclared := captureLegsOwner(clSnapGVK, projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))

	genericOwner := captureLegsOwner(
		storagev1alpha1.SchemeGroupVersion.WithKind("SomeGenericSnapshot"),
		projTestContent, "", "", string(storagev1alpha1.SnapshotCapturePhasePlanned))

	for _, tc := range []struct {
		name  string
		owner *unstructured.Unstructured
		want  string
	}{
		{name: "declared leg: a capture is pending", owner: declared, want: "waiting for manifest capture checkpoint to be published"},
		{name: "no owner resolved this pass", owner: nil, want: "waiting for manifest capture checkpoint to be published"},
		{name: "non-domain-capture owner never declares the latch", owner: genericOwner, want: "waiting for manifest capture checkpoint to be published"},
		{name: "undeclared leg: nothing has been asked for yet", owner: undeclared, want: "waiting for the manifest capture leg to be declared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := r.manifestPendingMessage(tc.owner)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("message %q must contain %q", got, tc.want)
			}
			if tc.owner == undeclared && !strings.Contains(got, clOwnerName) {
				t.Fatalf("the undeclared-leg message must name the owner, got %q", got)
			}
		})
	}
}
