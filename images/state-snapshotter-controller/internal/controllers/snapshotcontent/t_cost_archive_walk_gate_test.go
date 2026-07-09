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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// snapshotOwnerGVK is the GVK of the owning Snapshot read by declaredNonLeafChildContentNames via the
// uncached APIReader. Counting Gets of this GVK on the APIReader role is a clean proxy for "the expensive
// declared-vs-linked walk ran on this pass" (nothing else in the plan reads a Snapshot uncached).
func snapshotOwnerGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   storagev1alpha1.SchemeGroupVersion.Group,
		Version: storagev1alpha1.SchemeGroupVersion.Version,
		Kind:    "Snapshot",
	}
}

// T-cost: when this node's own manifest leg is NOT yet Ready, the archive latch cannot become True on this
// pass regardless of the declared set, so the expensive uncached declared-vs-linked walk must be skipped.
func TestArchiveWalkSkippedWhenOwnManifestNotReady(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := aggScheme(t)

	// mcpName empty -> own manifest leg pending (ownManifestReady == false).
	parent := contentWithSnapshotRef("root", "", "ns1", "snap-root")
	owner := ownerSnapshotWithChildren("ns1", "snap-root", "child-snap") // declares a non-leaf child
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner).Build()

	counters := newSplitCounters()
	cache, apiReader := counters.splitClients(base, nil)
	r := &SnapshotContentController{Client: cache, APIReader: apiReader, Scheme: scheme, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, parent)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsArchivedStatus != metav1.ConditionFalse {
		t.Fatalf("manifestsArchived = %s, want False (own manifest pending)", plan.manifestsArchivedStatus)
	}
	if n := counters.getCount(roleAPIReader, snapshotOwnerGVK()); n != 0 {
		t.Fatalf("owner Snapshot APIReader GET = %d, want 0 (declared walk must be skipped while own manifest not ready)", n)
	}
}

// T-cost: when a linked child is still pending (not archived), the subtree is not archived regardless of
// the declared set, so the expensive uncached declared-vs-linked walk must be skipped.
func TestArchiveWalkSkippedWhenLinkedChildPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := aggScheme(t)

	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	pendingChild := contentWithManifestsArchived("cc1", metav1.ConditionFalse, snapshot.ReasonManifestsCapturing)
	parent := contentWithSnapshotRef("root", "mcp-ok", "ns1", "snap-root", "cc1")
	owner := ownerSnapshotWithChildren("ns1", "snap-root", "child-snap")
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, mcp, pendingChild).Build()

	counters := newSplitCounters()
	cache, apiReader := counters.splitClients(base, nil)
	r := &SnapshotContentController{Client: cache, APIReader: apiReader, Scheme: scheme, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, parent)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsArchivedStatus != metav1.ConditionFalse {
		t.Fatalf("manifestsArchived = %s, want False (linked child pending)", plan.manifestsArchivedStatus)
	}
	if n := counters.getCount(roleAPIReader, snapshotOwnerGVK()); n != 0 {
		t.Fatalf("owner Snapshot APIReader GET = %d, want 0 (declared walk must be skipped while a linked child is pending)", n)
	}
}

// T-cost: on the single pass that CAN latch True (own manifest Ready AND every linked child archived), the
// authoritative uncached declared-vs-linked fail-close MUST run so an unlinked declared child cannot be
// missed. Here the declared child resolves to the linked+archived content, so the latch closes True.
func TestArchiveWalkRunsWhenAboutToLatchTrue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := aggScheme(t)

	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	archivedChild := contentWithManifestsArchived("cc1", metav1.ConditionTrue, snapshot.ReasonManifestsArchived)
	owner := ownerSnapshotWithChildren("ns1", "snap-root", "child-snap")
	boundChild := boundChildSnapshot("ns1", "child-snap", "cc1") // declared child resolves to linked content cc1
	parent := contentWithSnapshotRef("root", "mcp-ok", "ns1", "snap-root", "cc1")
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, boundChild, mcp, archivedChild).Build()

	counters := newSplitCounters()
	cache, apiReader := counters.splitClients(base, nil)
	r := &SnapshotContentController{Client: cache, APIReader: apiReader, Scheme: scheme, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, parent)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsArchivedStatus != metav1.ConditionTrue || plan.manifestsArchivedReason != snapshot.ReasonManifestsArchived {
		t.Fatalf("manifestsArchived = %s/%s, want True/%s (about to latch: declared child linked+archived)", plan.manifestsArchivedStatus, plan.manifestsArchivedReason, snapshot.ReasonManifestsArchived)
	}
	if n := counters.getCount(roleAPIReader, snapshotOwnerGVK()); n < 1 {
		t.Fatalf("owner Snapshot APIReader GET = %d, want >=1 (declared fail-close must run on the latch-True pass)", n)
	}
}
