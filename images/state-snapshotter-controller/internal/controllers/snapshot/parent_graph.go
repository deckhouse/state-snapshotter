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
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/state-snapshotter/api/names"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snaphelpers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// snapshotChildSnapshotName is the name of a root-owned domain child snapshot CR, keyed by the parent
// Snapshot UID and the captured source object UID (unified scheme, see api/names). The child
// snapshot GVK is intentionally NOT part of the key (it is derived from the source GVK via the
// CustomSnapshotDefinition); connectivity is carried by ownerRefs/childRefs, not the name.
func snapshotChildSnapshotName(parentSnapshotUID, sourceUID types.UID) string {
	return names.ChildSnapshotName(parentSnapshotUID, sourceUID)
}

// sourceListForbiddenError signals that listing a mapped source kind was rejected with Forbidden.
// RBAC for domain/custom sources is granted externally (CSD AccessGranted), so the planner degrades the
// graph (Ready=False/SourceListForbidden) and requeues instead of treating Forbidden as an empty
// result (which would silently drop coverage) or as a hard reconcile error (noisy log spam while
// waiting for RBAC to be granted).
type sourceListForbiddenError struct {
	msg string
}

func (e *sourceListForbiddenError) Error() string { return e.msg }

type snapshotCoverageChecker interface {
	IsCovered(ctx context.Context, obj *unstructured.Unstructured) (bool, error)
	ObservePlannedSnapshot(ctx context.Context, source *unstructured.Unstructured, snapshotRef storagev1alpha1.SnapshotChildRef, contentRef *storagev1alpha1.SnapshotContentChildRef) error
}

type refBasedSnapshotCoverageChecker struct {
	reader    client.Reader
	namespace string
	seen      map[string]struct{}
	covered   map[string]struct{}
	roots     []storagev1alpha1.SnapshotChildRef
}

func newSnapshotCoverageChecker(reader client.Reader, namespace string, roots []storagev1alpha1.SnapshotChildRef) snapshotCoverageChecker {
	return &refBasedSnapshotCoverageChecker{
		reader:    reader,
		namespace: namespace,
		seen:      make(map[string]struct{}),
		covered:   make(map[string]struct{}),
		roots:     append([]storagev1alpha1.SnapshotChildRef(nil), roots...),
	}
}

func (c *refBasedSnapshotCoverageChecker) IsCovered(ctx context.Context, obj *unstructured.Unstructured) (bool, error) {
	if err := c.refresh(ctx); err != nil {
		return false, err
	}
	identity, err := controllercommon.SnapshotSourceIdentityFromObject(obj)
	if err != nil {
		return false, err
	}
	_, ok := c.covered[coverageObjectKey(identity)]
	return ok, nil
}

func (c *refBasedSnapshotCoverageChecker) ObservePlannedSnapshot(_ context.Context, source *unstructured.Unstructured, snapshotRef storagev1alpha1.SnapshotChildRef, _ *storagev1alpha1.SnapshotContentChildRef) error {
	identity, err := controllercommon.SnapshotSourceIdentityFromObject(source)
	if err != nil {
		return err
	}
	c.covered[coverageObjectKey(identity)] = struct{}{}
	c.roots = append(c.roots, snapshotRef)
	return nil
}

func (c *refBasedSnapshotCoverageChecker) refresh(ctx context.Context) error {
	for _, ref := range c.roots {
		if err := c.visitSnapshotRef(ctx, ref); err != nil {
			return err
		}
	}
	return nil
}

func (c *refBasedSnapshotCoverageChecker) visitSnapshotRef(ctx context.Context, ref storagev1alpha1.SnapshotChildRef) error {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return err
	}
	gvk := gv.WithKind(ref.Kind)
	refKey := gvk.String() + "|" + c.namespace + "|" + ref.Name
	if _, ok := c.seen[refKey]; ok {
		return nil
	}
	c.seen[refKey] = struct{}{}
	child := &unstructured.Unstructured{}
	child.SetGroupVersionKind(gvk)
	if err := c.reader.Get(ctx, client.ObjectKey{Namespace: c.namespace, Name: ref.Name}, child); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	// spec.sourceRef is the source-of-truth for what an existing child snapshot captures. Namespace is
	// implicit (the run tree is namespace-local to the root Snapshot). A child without a valid
	// spec.sourceRef contributes no coverage and is skipped rather than failing the whole run
	// (one malformed sibling must not wedge planning); the skip is logged so the migration/corruption
	// edge is observable instead of silent. Framework-created children always carry a valid ref, so a
	// false "not covered" only re-plans to the same deterministic child name (idempotent).
	srcRef, found, err := unstructured.NestedStringMap(child.Object, "spec", "sourceRef")
	if err != nil {
		return err
	}
	switch {
	case !found:
		ctrllog.FromContext(ctx).V(1).Info("child snapshot has no spec.sourceRef; skipping for coverage",
			"gvk", gvk.String(), "namespace", c.namespace, "name", ref.Name)
	default:
		identity := controllercommon.SnapshotSourceIdentity{
			APIVersion: srcRef["apiVersion"],
			Kind:       srcRef["kind"],
			Namespace:  c.namespace,
			Name:       srcRef["name"],
		}
		if verr := identity.Validate(); verr != nil {
			ctrllog.FromContext(ctx).V(1).Info("child snapshot spec.sourceRef is invalid; skipping for coverage",
				"gvk", gvk.String(), "namespace", c.namespace, "name", ref.Name, "error", verr.Error())
		} else {
			c.covered[coverageObjectKey(identity)] = struct{}{}
		}
	}
	children, _, err := unstructured.NestedSlice(child.Object, "status", "childrenSnapshotRefs")
	if err != nil {
		return err
	}
	for _, raw := range children {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		childRef := storagev1alpha1.SnapshotChildRef{
			APIVersion: fmt.Sprint(m["apiVersion"]),
			Kind:       fmt.Sprint(m["kind"]),
			Name:       fmt.Sprint(m["name"]),
		}
		if childRef.APIVersion == "" || childRef.Kind == "" || childRef.Name == "" {
			continue
		}
		if err := c.visitSnapshotRef(ctx, childRef); err != nil {
			return err
		}
	}
	return nil
}

func coverageObjectKey(identity controllercommon.SnapshotSourceIdentity) string {
	return identity.APIVersion + "|" + identity.Kind + "|" + identity.Namespace + "|" + identity.Name
}

// weightLayerCaptureReady reports whether every child snapshot in a weight layer has reached the domain
// capture barrier 1 (status.captureState.domainSpecificController.phase >= Planned, i.e. Planned or
// Finished). The former per-child PlanningReady condition was replaced by this domain-owned phase enum;
// spec is immutable, so no observedGeneration gate is needed. Returns:
//   - ready: all children are Planned/Finished;
//   - terminalMessage: non-empty only when a child surfaced a terminal failure (phase=Failed, or a
//     terminal Ready reason); duration never does;
//   - pending: human-readable descriptors of the children not yet Planned (for the ChildrenPending
//     message). Waiting on these is unbounded by design.
func (r *SnapshotReconciler) weightLayerCaptureReady(ctx context.Context, namespace string, refs []storagev1alpha1.SnapshotChildRef) (ready bool, terminalMessage string, pending []string, err error) {
	for _, ref := range refs {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			return false, "", nil, err
		}
		gvk := gv.WithKind(ref.Kind)
		child := &unstructured.Unstructured{}
		child.SetGroupVersionKind(gvk)
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, child); err != nil {
			if errors.IsNotFound(err) {
				pending = append(pending, fmt.Sprintf("%s/%s/%s (not created yet)", gvk.String(), namespace, ref.Name))
				continue
			}
			return false, "", nil, err
		}
		if failed, message := snapshotChildTerminalFailure(child, gvk, namespace, ref.Name); failed {
			return false, message, nil, nil
		}
		if !childCaptureAtLeastPlanned(child) {
			pending = append(pending, describePendingChildPhase(child, gvk, namespace, ref.Name))
		}
	}
	if len(pending) > 0 {
		return false, "", pending, nil
	}
	return true, "", nil, nil
}

// childCapturePhase reads status.captureState.domainSpecificController.phase off a child domain snapshot.
func childCapturePhase(child *unstructured.Unstructured) string {
	phase, _, _ := unstructured.NestedString(child.Object, "status", "captureState", "domainSpecificController", "phase")
	return phase
}

// childCaptureAtLeastPlanned reports whether the child reached barrier 1 (Planned or Finished): objects
// created and refs published, so the root may expand its graph and the binder may take over the content.
func childCaptureAtLeastPlanned(child *unstructured.Unstructured) bool {
	switch storagev1alpha1.SnapshotCapturePhase(childCapturePhase(child)) {
	case storagev1alpha1.SnapshotCapturePhasePlanned, storagev1alpha1.SnapshotCapturePhaseFinished:
		return true
	default:
		return false
	}
}

// childSubtreePlanned reports whether the child's main-computed recursive planning latch
// status.captureState.commonController.subtreePlanned is present and true — i.e. the child AND its whole
// subtree finished planning. The orphan/residual wave gates on this rather
// than the child's own phase so a PVC covered by a not-yet-planned GRANDCHILD is never momentarily seen
// as orphan. nil/false (absent latch) reads as "subtree not planned yet".
func childSubtreePlanned(child *unstructured.Unstructured) bool {
	v, found, _ := unstructured.NestedBool(child.Object, "status", "captureState", "commonController", "subtreePlanned")
	return found && v
}

// allDeclaredDomainChildSnapshotsReady reports whether every declared DOMAIN child snapshot of the root
// (Snapshot.status.childrenSnapshotRefs) has reached capture barrier 1 (status.captureState.
// domainSpecificController.phase >= Planned, i.e. Planned or Finished). It is the wave barrier for root
// orphan/residual PVC volume capture: orphan PVCs must be evaluated only once the already-declared domain
// subtree's coverage is COMPUTABLE, so a PVC that a domain child covers is never momentarily seen as
// orphan. The gate was relaxed from full Ready=True to phase>=Planned, and is now the
// main-computed recursive subtreePlanned latch: each DIRECT child must report
// commonController.subtreePlanned=true, which means the child AND all its descendants finished planning.
// Direct phase>=Planned was insufficient — a PVC covered by a not-yet-planned GRANDCHILD could momentarily
// be seen as orphan before that grandchild published its coverage. subtreePlanned needs only planning
// (not Ready/status.data), so this does not reintroduce the deadlock: coverage still reads the covered PVC
// UIDs straight from captureState (the in-flight VolumeCaptureRequest name for VCR domains, or
// status.sourceRef.uid for native-CSI VolumeSnapshots — both published by Planned). The root MANIFEST
// branch is not gated by this. A NotFound child is pending (not created yet); a child whose subtree is not
// yet planned (latch absent/false) is pending; a terminal child failure (phase=Failed) is NOT surfaced here
// (the content aggregation reports ChildrenFailed) — it stays pending, keeping the gate closed. No
// observedGeneration gate: the latch is monotonic and the spec is immutable. A namespace with no declared
// domain children passes the gate vacuously.
func (r *SnapshotReconciler) allDeclaredDomainChildSnapshotsReady(ctx context.Context, namespace string, refs []storagev1alpha1.SnapshotChildRef) (ready bool, pending []string, err error) {
	for _, ref := range refs {
		gv, perr := schema.ParseGroupVersion(ref.APIVersion)
		if perr != nil {
			return false, nil, perr
		}
		gvk := gv.WithKind(ref.Kind)
		child := &unstructured.Unstructured{}
		child.SetGroupVersionKind(gvk)
		if gerr := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, child); gerr != nil {
			if errors.IsNotFound(gerr) {
				pending = append(pending, fmt.Sprintf("%s/%s/%s (not created yet)", gvk.String(), namespace, ref.Name))
				continue
			}
			return false, nil, gerr
		}
		if !childSubtreePlanned(child) {
			pending = append(pending, describePendingChildSubtreePlanned(child, gvk, namespace, ref.Name))
		}
	}
	if len(pending) > 0 {
		return false, pending, nil
	}
	return true, nil, nil
}

// maxPendingChildrenInMessage caps how many pending child descriptors are embedded in the
// ChildrenPending condition message. A namespace may map a large number of source objects; without
// a cap the condition message (and its status patch) could grow unboundedly and the apiserver may
// reject an oversized status update. The full count is always reported.
const maxPendingChildrenInMessage = 20

// summarizePendingChildren renders the pending-children part of the ChildrenPending message,
// truncating to maxPendingChildrenInMessage while still reporting the total count.
func summarizePendingChildren(pending []string) string {
	if len(pending) <= maxPendingChildrenInMessage {
		return "pending children: " + strings.Join(pending, ", ")
	}
	return fmt.Sprintf("pending children (first %d of %d): %s", maxPendingChildrenInMessage, len(pending), strings.Join(pending[:maxPendingChildrenInMessage], ", "))
}

// describePendingChildPhase renders a compact, diagnosable descriptor for a child that has not yet
// reached capture barrier 1, reporting its observed domain phase (empty = not started), so the parent's
// ChildrenPending message makes an hours-long wait explainable rather than silent.
func describePendingChildPhase(child *unstructured.Unstructured, gvk schema.GroupVersionKind, namespace, name string) string {
	phase := childCapturePhase(child)
	if phase == "" {
		return fmt.Sprintf("%s/%s/%s (no capture phase yet)", gvk.String(), namespace, name)
	}
	return fmt.Sprintf("%s/%s/%s (capture phase=%s)", gvk.String(), namespace, name, phase)
}

// describePendingChildSubtreePlanned renders a compact descriptor for a child whose recursive planning
// latch (commonController.subtreePlanned) is not yet set, reporting its own domain phase so a wait that is
// blocked on a not-yet-planned DESCENDANT (child phase already Planned, subtree latch still absent) is
// distinguishable from a child that has not started planning itself.
func describePendingChildSubtreePlanned(child *unstructured.Unstructured, gvk schema.GroupVersionKind, namespace, name string) string {
	phase := childCapturePhase(child)
	if phase == "" {
		return fmt.Sprintf("%s/%s/%s (subtree not planned yet: no capture phase)", gvk.String(), namespace, name)
	}
	return fmt.Sprintf("%s/%s/%s (subtree not planned yet: child phase=%s)", gvk.String(), namespace, name, phase)
}

func snapshotChildTerminalFailure(child *unstructured.Unstructured, gvk schema.GroupVersionKind, namespace, name string) (bool, string) {
	// Domain-owned terminal: phase=Failed carries the domain reason/message.
	if storagev1alpha1.SnapshotCapturePhase(childCapturePhase(child)) == storagev1alpha1.SnapshotCapturePhaseFailed {
		reason, _, _ := unstructured.NestedString(child.Object, "status", "captureState", "domainSpecificController", "reason")
		msg, _, _ := unstructured.NestedString(child.Object, "status", "captureState", "domainSpecificController", "message")
		detail := reason
		if msg != "" {
			detail = fmt.Sprintf("%s: %s", reason, msg)
		}
		return true, fmt.Sprintf("child snapshot %s/%s/%s failed capture (phase=Failed): %s", gvk.String(), namespace, name, detail)
	}
	// Core-derived terminal: a terminal Ready reason on the child. This is the fail-closed path for a domain
	// child's data-leg failure caught DURING root planning (vcr-watch-core-terminal): the core makes the
	// child content terminal (VolumeCaptureFailed) and mirrors it onto the child's Ready, and this gate
	// catches it (e.g. VolumeCaptureFailed, GraphPlanningFailed, ChildrenFailed). The consumer
	// (planNamespaceChildren) surfaces it on the root as the CANONICAL ChildrenFailed — the same reason the
	// content aggregation + Ready mirror produce when the failure lands post-Planned — so the root's
	// terminal reason does not depend on the race between the child failure and MarkPlanned.
	class, message := usecase.ClassifyGenericChildSnapshotReady(child, gvk, namespace, name)
	if class == usecase.SnapshotChildReadyClassFailed && readyConditionIsCurrentTerminal(child) {
		return true, message
	}
	return false, ""
}

// readyConditionIsCurrentTerminal reports whether the child's Ready condition is authoritative for its
// current generation (observedGeneration == metadata.generation). The Ready-based terminal classifier
// (usecase.ClassifyGenericChildSnapshotReady) does not check observedGeneration, so without this guard
// a stale Ready=False/<terminal reason> from an older spec generation could trip a false terminal
// failure in the wave gate: a terminal state counts only when the child confirmed it for the current
// generation.
func readyConditionIsCurrentTerminal(child *unstructured.Unstructured) bool {
	rc := usecase.CurrentReadyCondition(child)
	return rc != nil && rc.ObservedGeneration == child.GetGeneration()
}

// patchSnapshotNotReadyLocal writes a local (non-mirror) Ready=False condition on the root Snapshot. It is
// used for planning-time failures that cannot yet be represented by the bound SnapshotContent (e.g. a graph
// planning error before any content Ready exists); the Ready=True path is always driven by the mirror from
// SnapshotContent, so this local writer only ever reports the failure state.
func (r *SnapshotReconciler) patchSnapshotNotReadyLocal(
	ctx context.Context,
	key types.NamespacedName,
	reason string,
	message string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, key, cur); err != nil {
			return err
		}
		existing := meta.FindStatusCondition(cur.Status.Conditions, snapshotpkg.ConditionReady)
		if existing != nil &&
			existing.Status == metav1.ConditionFalse &&
			existing.Reason == reason &&
			existing.Message == message &&
			existing.ObservedGeneration == cur.Generation {
			return nil
		}
		base := cur.DeepCopy()
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:               snapshotpkg.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: cur.Generation,
		})
		return r.Client.Status().Patch(ctx, cur, client.MergeFrom(base))
	})
}

// coverageRootsForNextWave returns the snapshot refs used to seed the coverage checker for the next
// (higher-numeric) weight wave during child-graph recompute. It intentionally returns ONLY the refs
// planned/confirmed in the current recompute pass and NEVER the parent's own
// status.childrenSnapshotRefs.
//
// Seeding from parent status is the self-coverage idempotency bug: a generated child from another
// wave carried in status would be visited by the coverage checker, which reads its own spec.sourceRef
// and marks that source covered. The same source is then skipped this pass and omitted from the
// declared set, so the root stops re-declaring a child it still owns — and a subsequent pass reads the
// gap as a lost child. Planning must be a full recompute: coverage between waves flows only from
// earlier (lower-numeric-weight) subtrees planned in this pass.
func coverageRootsForNextWave(plannedThisPass []storagev1alpha1.SnapshotChildRef) []storagev1alpha1.SnapshotChildRef {
	return append([]storagev1alpha1.SnapshotChildRef{}, plannedThisPass...)
}

func sortSnapshotChildRefs(refs []storagev1alpha1.SnapshotChildRef) {
	sort.Slice(refs, func(i, j int) bool {
		return fmt.Sprintf("%s/%s/%s", refs[i].APIVersion, refs[i].Kind, refs[i].Name) <
			fmt.Sprintf("%s/%s/%s", refs[j].APIVersion, refs[j].Kind, refs[j].Name)
	})
}

func sortSnapshotContentChildRefs(refs []storagev1alpha1.SnapshotContentChildRef) {
	sort.Slice(refs, func(i, j int) bool {
		return refs[i].Name < refs[j].Name
	})
}
