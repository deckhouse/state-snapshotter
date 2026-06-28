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
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/csdregistry"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func (r *SnapshotReconciler) reconcileParentOwnedChildGraph(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
) (bool, bool, error) {
	mappings, err := csdregistry.EligibleResourceSnapshotMappings(ctx, r.snapshotReader(), r.Mgr.GetRESTMapper())
	if err != nil {
		return false, false, err
	}
	if len(mappings) == 0 {
		changed, err := r.patchSnapshotChildrenRefs(ctx, types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, nil)
		return changed, err == nil, err
	}

	// resourceSelector narrows which top-level/standalone domain source objects the root expands into child
	// snapshots (nil = expand all). Resolved once here and threaded into every layer. Excluded objects are
	// consistently dropped from the root manifest leg by the same selector. Nested domain children created by
	// domain controllers are out of scope (see plan section 5a). A resolve error is surfaced as a graph
	// planning failure by the caller (ChildrenSnapshotReady=False).
	selector, err := nsSnap.ResolveResourceSelector()
	if err != nil {
		return false, false, fmt.Errorf("resolve spec.resourceSelector: %w", err)
	}

	var desiredRefs []storagev1alpha1.SnapshotChildRef
	coverage := newSnapshotCoverageChecker(r.Client, nsSnap.Namespace, nil)
	for layerStart := 0; layerStart < len(mappings); {
		priority := mappings[layerStart].Priority
		layerEnd := layerStart + 1
		for layerEnd < len(mappings) && mappings[layerEnd].Priority == priority {
			layerEnd++
		}
		var layerRefs []storagev1alpha1.SnapshotChildRef
		for _, mapping := range mappings[layerStart:layerEnd] {
			refs, err := r.ensureParentOwnedChildGraphLayer(ctx, nsSnap, mapping, coverage, selector)
			if err != nil {
				var forbidden *sourceListForbiddenError
				if stderrors.As(err, &forbidden) {
					sortSnapshotChildRefs(desiredRefs)
					changed, perr := r.patchSnapshotChildrenRefsCondition(ctx, types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, desiredRefs, metav1.ConditionFalse, snapshotpkg.ReasonSourceListForbidden, forbidden.Error())
					return changed, false, perr
				}
				return false, false, err
			}
			layerRefs = append(layerRefs, refs...)
		}
		desiredRefs = append(desiredRefs, layerRefs...)
		ready, terminalMessage, pending, err := r.priorityLayerChildrenSnapshotReady(ctx, nsSnap.Namespace, layerRefs)
		if err != nil {
			return false, false, err
		}
		if terminalMessage != "" {
			sortSnapshotChildRefs(desiredRefs)
			changed, err := r.patchSnapshotChildrenRefsCondition(ctx, types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, desiredRefs, metav1.ConditionFalse, snapshotpkg.ReasonGraphPlanningFailed, terminalMessage)
			return changed, false, err
		}
		if !ready {
			// Unbounded by design: a child snapshot (e.g. large-storage capture) may stay pending for
			// hours. Hold ChildrenSnapshotReady=False/PriorityLayerPending listing the pending children for
			// diagnosability; never fail by duration. Capture stays gated until the layer is ready.
			sortSnapshotChildRefs(desiredRefs)
			message := fmt.Sprintf("waiting for priority %d child snapshots to publish current ChildrenSnapshotReady=True; %s", priority, summarizePendingChildren(pending))
			changed, err := r.patchSnapshotChildrenRefsCondition(ctx, types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, desiredRefs, metav1.ConditionFalse, snapshotpkg.ReasonPriorityLayerPending, message)
			return changed, false, err
		}
		coverage = newSnapshotCoverageChecker(r.Client, nsSnap.Namespace, coverageRootsForNextWave(desiredRefs))
		layerStart = layerEnd
	}
	sortSnapshotChildRefs(desiredRefs)

	statusChanged, err := r.patchSnapshotChildrenRefs(ctx, types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, desiredRefs)
	if err != nil {
		return false, false, err
	}

	_ = content
	return statusChanged, true, nil
}

func (r *SnapshotReconciler) ensureParentOwnedChildGraphLayer(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	mapping csdregistry.EligibleResourceSnapshotMapping,
	coverage snapshotCoverageChecker,
	selector labels.Selector,
) ([]storagev1alpha1.SnapshotChildRef, error) {
	var refs []storagev1alpha1.SnapshotChildRef
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(mapping.SourceGVK)
	list.SetKind(mapping.SourceGVK.Kind + "List")
	resources, err := r.Dynamic.Resource(mapping.SourceGVR).Namespace(nsSnap.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		// NotFound: the mapped source kind is not (yet) served by the API; legitimately empty for now.
		if errors.IsNotFound(err) {
			return nil, nil
		}
		// Forbidden is RBAC-driven (granted externally via DSC RBACReady). Treating it as "no objects"
		// would silently drop coverage, so degrade the graph instead of returning empty (fail-closed).
		if errors.IsForbidden(err) {
			return nil, &sourceListForbiddenError{msg: fmt.Sprintf("list source %s: %v", mapping.SourceGVK.String(), err)}
		}
		return nil, err
	}
	list.Items = resources.Items
	sort.Slice(list.Items, func(i, j int) bool {
		a, b := list.Items[i], list.Items[j]
		if a.GetNamespace() != b.GetNamespace() {
			return a.GetNamespace() < b.GetNamespace()
		}
		if a.GetName() != b.GetName() {
			return a.GetName() < b.GetName()
		}
		return string(a.GetUID()) < string(b.GetUID())
	})
	for i := range list.Items {
		resource := &list.Items[i]
		// User-provided resourceSelector narrows expansion: a domain source object whose labels do not match
		// is not expanded into a child snapshot (nil selector = expand all). The same object is then dropped
		// from the root manifest leg by the same selector, keeping the two legs consistent.
		if selector != nil && !selector.Matches(labels.Set(resource.GetLabels())) {
			continue
		}
		covered, err := coverage.IsCovered(ctx, resource)
		if err != nil {
			return nil, err
		}
		if covered {
			continue
		}
		childName := snapshotChildSnapshotName(nsSnap.Name, mapping.SourceGVK.String(), mapping.SnapshotGVK.String(), resource.GetName(), string(resource.GetUID()))
		if err := r.ensureParentOwnedChildSnapshot(ctx, nsSnap, childName, mapping.SnapshotGVK, resource); err != nil {
			return nil, err
		}
		ref := storagev1alpha1.SnapshotChildRef{
			APIVersion: mapping.SnapshotGVK.GroupVersion().String(),
			Kind:       mapping.SnapshotGVK.Kind,
			Name:       childName,
		}
		refs = append(refs, ref)
		if err := coverage.ObservePlannedSnapshot(ctx, resource, ref, nil); err != nil {
			return nil, err
		}
	}
	sortSnapshotChildRefs(refs)
	return refs, nil
}

func snapshotChildSnapshotName(parentName, resourceGVK, snapshotGVK, resourceName, resourceUID string) string {
	sum := sha256.Sum256([]byte(parentName + "|" + resourceGVK + "|" + snapshotGVK + "|" + resourceName + "|" + resourceUID))
	return "nss-child-" + hex.EncodeToString(sum[:10])
}

func (r *SnapshotReconciler) ensureParentOwnedChildSnapshot(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	name string,
	gvk schema.GroupVersionKind,
	source *unstructured.Unstructured,
) error {
	sourceIdentity, err := controllercommon.SnapshotSourceIdentityFromObject(source)
	if err != nil {
		return fmt.Errorf("source identity for %s/%s: %w", source.GroupVersionKind().String(), source.GetName(), err)
	}
	key := client.ObjectKey{Namespace: nsSnap.Namespace, Name: name}
	child := &unstructured.Unstructured{}
	child.SetGroupVersionKind(gvk)
	if err := r.Client.Get(ctx, key, child); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		// spec.sourceRef is the single source-of-truth for what this child snapshot captures; the CRD
		// enforces its immutability (CEL self == oldSelf), so the planner sets it once at creation and
		// never rewrites it.
		child = &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": gvk.GroupVersion().String(),
				"kind":       gvk.Kind,
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": nsSnap.Namespace,
				},
				"spec": map[string]interface{}{
					"sourceRef": map[string]interface{}{
						"apiVersion": sourceIdentity.APIVersion,
						"kind":       sourceIdentity.Kind,
						"name":       sourceIdentity.Name,
					},
				},
			},
		}
		child.SetGroupVersionKind(gvk)
		child.SetOwnerReferences([]metav1.OwnerReference{controllercommon.SnapshotOwnerReference(storagev1alpha1.SchemeGroupVersion.String(), "Snapshot", nsSnap.Name, nsSnap.UID)})
		return r.Client.Create(ctx, child)
	}
	base := child.DeepCopy()
	if err := controllercommon.EnsureSnapshotOwnerRef(child, controllercommon.SnapshotOwnerReference(storagev1alpha1.SchemeGroupVersion.String(), "Snapshot", nsSnap.Name, nsSnap.UID)); err != nil {
		return err
	}
	if controllercommon.OwnerReferencesEqual(base.GetOwnerReferences(), child.GetOwnerReferences()) {
		return nil
	}
	return r.Client.Patch(ctx, child, client.MergeFrom(base))
}

// sourceListForbiddenError signals that listing a mapped source kind was rejected with Forbidden.
// RBAC for domain/custom sources is granted externally (DSC RBACReady), so the planner degrades the
// graph (ChildrenSnapshotReady=False/SourceListForbidden) and requeues instead of treating Forbidden as an empty
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

// priorityLayerChildrenSnapshotReady reports whether every child snapshot in a priority layer has published a
// current ChildrenSnapshotReady=True (observedGeneration == metadata.generation; Ready=True does NOT substitute
// ChildrenSnapshotReady=True). Returns:
//   - ready: all children are ChildrenSnapshotReady=True for their current generation;
//   - terminalMessage: non-empty only when a child surfaced a terminal failure condition (the only
//     thing that turns the layer into ChildrenSnapshotReady=False/GraphPlanningFailed); duration never does;
//   - pending: human-readable descriptors of the children not yet ready (for the PriorityLayerPending
//     message). Waiting on these is unbounded by design.
func (r *SnapshotReconciler) priorityLayerChildrenSnapshotReady(ctx context.Context, namespace string, refs []storagev1alpha1.SnapshotChildRef) (ready bool, terminalMessage string, pending []string, err error) {
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
		conditions, _, err := unstructured.NestedSlice(child.Object, "status", "conditions")
		if err != nil {
			return false, "", nil, err
		}
		if !conditionSliceHasCurrentTrue(conditions, snapshotpkg.ConditionChildrenSnapshotReady, child.GetGeneration()) {
			pending = append(pending, describePendingChildChildrenSnapshotReady(conditions, gvk, namespace, ref.Name, child.GetGeneration()))
		}
	}
	if len(pending) > 0 {
		return false, "", pending, nil
	}
	return true, "", nil, nil
}

// allDeclaredDomainChildSnapshotsReady reports whether every declared DOMAIN child snapshot of the root
// (Snapshot.status.childrenSnapshotRefs, excluding CSI VolumeSnapshot visibility leaves) has reached full
// Ready=True. It is the wave barrier for root orphan/residual PVC volume capture: orphan PVCs must be
// evaluated only after the domain subtree has finished capturing, so a PVC that a domain child covers is
// never momentarily seen as orphan (which previously created an nss-vs-* VolumeSnapshot + child volume
// node that then got pruned, leaving a dangling, never-archiving node). The root MANIFEST branch is not
// gated by this. A NotFound or not-yet-Ready child is pending (not a failure); a terminal child failure is
// surfaced separately by the content aggregation (ChildrenFailed), so here it simply keeps the gate closed.
// Readiness reuses ClassifyGenericChildSnapshotReady (Ready=True == Completed) for the pending/failed
// descriptors AND enforces the strict generation contract: a Ready=True is honored only when its
// observedGeneration == metadata.generation, so a stale Ready=True from a previous spec generation cannot
// open the orphan wave while the child re-reconciles (mirrors readyConditionIsCurrentTerminal /
// conditionSliceHasCurrentTrue; domain child controllers stamp Ready.observedGeneration on every write).
// A namespace with no declared domain children passes the gate vacuously.
func (r *SnapshotReconciler) allDeclaredDomainChildSnapshotsReady(ctx context.Context, namespace string, refs []storagev1alpha1.SnapshotChildRef) (ready bool, pending []string, err error) {
	for _, ref := range refs {
		if snapshotpkg.IsVolumeSnapshotVisibilityLeaf(ref) {
			continue
		}
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
		if class, msg := usecase.ClassifyGenericChildSnapshotReady(child, gvk, namespace, ref.Name); class != usecase.SnapshotChildReadyClassCompleted {
			pending = append(pending, msg)
			continue
		}
		// Completed (Ready=True): require the current generation so a stale Ready does not open the gate.
		if rc := usecase.CurrentReadyCondition(child); rc == nil || rc.ObservedGeneration != child.GetGeneration() {
			pending = append(pending, fmt.Sprintf("%s/%s/%s (Ready=True but observedGeneration stale/missing; want %d)", gvk.String(), namespace, ref.Name, child.GetGeneration()))
		}
	}
	if len(pending) > 0 {
		return false, pending, nil
	}
	return true, nil, nil
}

// maxPendingChildrenInMessage caps how many pending child descriptors are embedded in the
// PriorityLayerPending condition message. A namespace may map a large number of source objects; without
// a cap the condition message (and its status patch) could grow unboundedly and the apiserver may
// reject an oversized status update. The full count is always reported.
const maxPendingChildrenInMessage = 20

// summarizePendingChildren renders the pending-children part of the PriorityLayerPending message,
// truncating to maxPendingChildrenInMessage while still reporting the total count.
func summarizePendingChildren(pending []string) string {
	if len(pending) <= maxPendingChildrenInMessage {
		return "pending children: " + strings.Join(pending, ", ")
	}
	return fmt.Sprintf("pending children (first %d of %d): %s", maxPendingChildrenInMessage, len(pending), strings.Join(pending[:maxPendingChildrenInMessage], ", "))
}

// describePendingChildChildrenSnapshotReady renders a compact, diagnosable descriptor for a child whose
// ChildrenSnapshotReady is not yet current: it reports the observed ChildrenSnapshotReady status/reason, distinguishes a
// missing condition from a stale observedGeneration, so the parent's PriorityLayerPending message
// makes an hours-long wait explainable rather than silent.
func describePendingChildChildrenSnapshotReady(conditions []interface{}, gvk schema.GroupVersionKind, namespace, name string, generation int64) string {
	for _, raw := range conditions {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] != snapshotpkg.ConditionChildrenSnapshotReady {
			continue
		}
		status, _ := m["status"].(string)
		reason, _ := m["reason"].(string)
		observed, hasObserved := conditionObservedGeneration(m)
		switch {
		case status == string(metav1.ConditionTrue) && !hasObserved:
			return fmt.Sprintf("%s/%s/%s (ChildrenSnapshotReady=True without observedGeneration; want %d)", gvk.String(), namespace, name, generation)
		case status == string(metav1.ConditionTrue) && observed != generation:
			return fmt.Sprintf("%s/%s/%s (ChildrenSnapshotReady=True observedGeneration=%d, stale; want %d)", gvk.String(), namespace, name, observed, generation)
		default:
			return fmt.Sprintf("%s/%s/%s (ChildrenSnapshotReady=%s/%s)", gvk.String(), namespace, name, status, reason)
		}
	}
	return fmt.Sprintf("%s/%s/%s (no ChildrenSnapshotReady condition yet)", gvk.String(), namespace, name)
}

// conditionObservedGeneration extracts a condition's observedGeneration (int64/float64) and whether
// the field was present. A missing observedGeneration is treated as "not current" by the strict
// ChildrenSnapshotReady contract.
func conditionObservedGeneration(m map[string]interface{}) (int64, bool) {
	switch observed := m["observedGeneration"].(type) {
	case int64:
		return observed, true
	case float64:
		return int64(observed), true
	default:
		return 0, false
	}
}

func snapshotChildTerminalFailure(child *unstructured.Unstructured, gvk schema.GroupVersionKind, namespace, name string) (bool, string) {
	conditions, _, err := unstructured.NestedSlice(child.Object, "status", "conditions")
	if err == nil && conditionSliceHasCurrentFalseReason(conditions, snapshotpkg.ConditionChildrenSnapshotReady, snapshotpkg.ReasonGraphPlanningFailed, child.GetGeneration()) {
		return true, fmt.Sprintf("child snapshot %s/%s/%s failed graph planning", gvk.String(), namespace, name)
	}
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
// failure in the wave gate. Mirrors the strict ChildrenSnapshotReady contract: a terminal state counts only when
// the child has confirmed it for the current generation.
func readyConditionIsCurrentTerminal(child *unstructured.Unstructured) bool {
	rc := usecase.CurrentReadyCondition(child)
	return rc != nil && rc.ObservedGeneration == child.GetGeneration()
}

func conditionSliceHasCurrentTrue(conditions []interface{}, typ string, generation int64) bool {
	for _, raw := range conditions {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] != typ || m["status"] != string(metav1.ConditionTrue) {
			continue
		}
		// Strict contract: ChildrenSnapshotReady=True counts only with observedGeneration == metadata.generation.
		// A missing or stale observedGeneration means the child has not confirmed the current spec, so
		// the layer stays pending (never silently treated as ready).
		observed, ok := conditionObservedGeneration(m)
		return ok && observed == generation
	}
	return false
}

func conditionSliceHasCurrentFalseReason(conditions []interface{}, typ, reason string, generation int64) bool {
	for _, raw := range conditions {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] != typ || m["status"] != string(metav1.ConditionFalse) || m["reason"] != reason {
			continue
		}
		// Strict contract: a terminal ChildrenSnapshotReady=False is only current with observedGeneration ==
		// metadata.generation. A stale/missing observedGeneration is treated as not-yet-current
		// (pending), so a child must re-confirm failure for the current spec generation.
		observed, ok := conditionObservedGeneration(m)
		return ok && observed == generation
	}
	return false
}

func (r *SnapshotReconciler) patchSnapshotChildrenRefs(
	ctx context.Context,
	parent types.NamespacedName,
	desired []storagev1alpha1.SnapshotChildRef,
) (bool, error) {
	return r.patchSnapshotChildrenRefsCondition(ctx, parent, desired, metav1.ConditionTrue, snapshotpkg.ReasonCompleted, "child planning complete")
}

func (r *SnapshotReconciler) patchSnapshotChildrenRefsCondition(
	ctx context.Context,
	parent types.NamespacedName,
	desired []storagev1alpha1.SnapshotChildRef,
	status metav1.ConditionStatus,
	reason string,
	message string,
) (bool, error) {
	changed := false
	var effective []storagev1alpha1.SnapshotChildRef
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, parent, cur); err != nil {
			return err
		}
		effective = mergeSnapshotManagedChildRefs(cur.Status.ChildrenSnapshotRefs, desired)
		domainReady := meta.FindStatusCondition(cur.Status.Conditions, snapshotpkg.ConditionChildrenSnapshotReady)
		domainReadyCurrent := domainReady != nil &&
			domainReady.Status == status &&
			domainReady.Reason == reason &&
			domainReady.Message == message &&
			domainReady.ObservedGeneration == cur.Generation
		if snapshotChildRefsEqualIgnoreOrder(cur.Status.ChildrenSnapshotRefs, effective) && domainReadyCurrent {
			return nil
		}
		cur.Status.ChildrenSnapshotRefs = append([]storagev1alpha1.SnapshotChildRef(nil), effective...)
		cur.Status.ObservedGeneration = cur.Generation
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:               snapshotpkg.ConditionChildrenSnapshotReady,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: cur.Generation,
		})
		changed = true
		return r.Client.Status().Update(ctx, cur)
	})
	return changed, err
}

func (r *SnapshotReconciler) patchSnapshotChildrenSnapshotReady(
	ctx context.Context,
	key types.NamespacedName,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, key, cur); err != nil {
			return err
		}
		existing := meta.FindStatusCondition(cur.Status.Conditions, snapshotpkg.ConditionChildrenSnapshotReady)
		if existing != nil &&
			existing.Status == status &&
			existing.Reason == reason &&
			existing.Message == message &&
			existing.ObservedGeneration == cur.Generation {
			return nil
		}
		base := cur.DeepCopy()
		cur.Status.ObservedGeneration = cur.Generation
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:               snapshotpkg.ConditionChildrenSnapshotReady,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: cur.Generation,
		})
		return r.Client.Status().Patch(ctx, cur, client.MergeFrom(base))
	})
}

func mergeSnapshotManagedChildRefs(current, desired []storagev1alpha1.SnapshotChildRef) []storagev1alpha1.SnapshotChildRef {
	merged := make([]storagev1alpha1.SnapshotChildRef, 0, len(current)+len(desired))
	for _, ref := range current {
		if snapshotOwnsGeneratedChildRef(ref) {
			continue
		}
		merged = append(merged, ref)
	}
	merged = append(merged, desired...)
	sortSnapshotChildRefs(merged)
	return merged
}

func snapshotOwnsGeneratedChildRef(ref storagev1alpha1.SnapshotChildRef) bool {
	return strings.HasPrefix(ref.Name, "nss-child-")
}

// coverageRootsForNextWave returns the snapshot refs used to seed the coverage checker for the next
// (lower) priority wave during child-graph recompute. It intentionally returns ONLY the refs
// planned/confirmed in the current recompute pass and NEVER the parent's own
// status.childrenSnapshotRefs.
//
// Seeding from parent status is the self-coverage idempotency bug: a generated lower-priority child
// carried in status would be visited by the coverage checker, which reads its own spec.sourceRef
// and marks that source covered. The same source is then skipped this pass, omitted from
// desiredRefs, and finally stripped by mergeSnapshotManagedChildRefs — so the standalone child ref
// silently disappears from the root on every subsequent reconcile. Planning must be a full recompute:
// coverage between waves flows only from higher-priority subtrees planned in this pass.
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
