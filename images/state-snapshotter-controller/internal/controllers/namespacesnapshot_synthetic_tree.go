/*
Copyright 2025 Flant JSC

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

// Temporary N2b synthetic one-child tree scaffold until domain-specific child wiring replaces it.
//
// Synthetic child NamespaceSnapshots always behave as N2a leaves (no synthetic-tree annotation) and never
// create nested synthetic children. Parents that opt in via annotation get a post–manifest-capture step that
// ensures one synthetic child and gates parent Ready on that child (see namespacesnapshot_synthetic_child_state.go).

package controllers

import (
	"context"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func namespaceSnapshotChildRefsEqual(a, b []storagev1alpha1.NamespaceSnapshotChildRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Namespace != b[i].Namespace {
			return false
		}
	}
	return true
}

func namespaceSnapshotChildRefKey(ref storagev1alpha1.NamespaceSnapshotChildRef) string {
	return ref.Namespace + "\x00" + ref.Name
}

// mergeNamespaceSnapshotChildRefs returns a new slice: all entries from existing, then each upsert overwrites
// or appends by key (namespace, name). Result is sorted by (namespace, name) for stable status (spec §3.2 / INV-REF-M1).
func mergeNamespaceSnapshotChildRefs(existing, upsert []storagev1alpha1.NamespaceSnapshotChildRef) []storagev1alpha1.NamespaceSnapshotChildRef {
	m := make(map[string]storagev1alpha1.NamespaceSnapshotChildRef, len(existing)+len(upsert))
	order := make([]string, 0, len(existing)+len(upsert))
	add := func(ref storagev1alpha1.NamespaceSnapshotChildRef) {
		k := namespaceSnapshotChildRefKey(ref)
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = ref
	}
	for i := range existing {
		add(existing[i])
	}
	for i := range upsert {
		add(upsert[i])
	}
	sort.Strings(order)
	out := make([]storagev1alpha1.NamespaceSnapshotChildRef, 0, len(order))
	for _, k := range order {
		out = append(out, m[k])
	}
	return out
}

func namespaceSnapshotChildRefsEqualIgnoreOrder(a, b []storagev1alpha1.NamespaceSnapshotChildRef) bool {
	if len(a) != len(b) {
		return false
	}
	sa := namespaceSnapshotChildRefsSortedCopy(a)
	sb := namespaceSnapshotChildRefsSortedCopy(b)
	return namespaceSnapshotChildRefsEqual(sa, sb)
}

func namespaceSnapshotChildRefsSortedCopy(src []storagev1alpha1.NamespaceSnapshotChildRef) []storagev1alpha1.NamespaceSnapshotChildRef {
	cp := append([]storagev1alpha1.NamespaceSnapshotChildRef(nil), src...)
	sort.Slice(cp, func(i, j int) bool {
		if cp[i].Namespace != cp[j].Namespace {
			return cp[i].Namespace < cp[j].Namespace
		}
		return cp[i].Name < cp[j].Name
	})
	return cp
}

func namespaceSnapshotContentChildRefsEqual(a, b []storagev1alpha1.NamespaceSnapshotContentChildRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			return false
		}
	}
	return true
}

// mergeNamespaceSnapshotContentChildRefs merges by child content name (key within parent NamespaceSnapshotContent).
func mergeNamespaceSnapshotContentChildRefs(existing, upsert []storagev1alpha1.NamespaceSnapshotContentChildRef) []storagev1alpha1.NamespaceSnapshotContentChildRef {
	m := make(map[string]storagev1alpha1.NamespaceSnapshotContentChildRef, len(existing)+len(upsert))
	order := make([]string, 0, len(existing)+len(upsert))
	add := func(ref storagev1alpha1.NamespaceSnapshotContentChildRef) {
		k := ref.Name
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = ref
	}
	for i := range existing {
		add(existing[i])
	}
	for i := range upsert {
		add(upsert[i])
	}
	sort.Strings(order)
	out := make([]storagev1alpha1.NamespaceSnapshotContentChildRef, 0, len(order))
	for _, k := range order {
		out = append(out, m[k])
	}
	return out
}

func namespaceSnapshotContentChildRefsEqualIgnoreOrder(a, b []storagev1alpha1.NamespaceSnapshotContentChildRef) bool {
	if len(a) != len(b) {
		return false
	}
	sa := namespaceSnapshotContentChildRefsSortedCopy(a)
	sb := namespaceSnapshotContentChildRefsSortedCopy(b)
	return namespaceSnapshotContentChildRefsEqual(sa, sb)
}

func namespaceSnapshotContentChildRefsSortedCopy(src []storagev1alpha1.NamespaceSnapshotContentChildRef) []storagev1alpha1.NamespaceSnapshotContentChildRef {
	cp := append([]storagev1alpha1.NamespaceSnapshotContentChildRef(nil), src...)
	sort.Slice(cp, func(i, j int) bool {
		return cp[i].Name < cp[j].Name
	})
	return cp
}

// removeNamespaceSnapshotChildRefsByKeys returns existing refs minus any whose (namespace,name) appears in remove (INV-REF-M2: caller must only pass keys it owns).
func removeNamespaceSnapshotChildRefsByKeys(existing, remove []storagev1alpha1.NamespaceSnapshotChildRef) []storagev1alpha1.NamespaceSnapshotChildRef {
	if len(remove) == 0 {
		return namespaceSnapshotChildRefsSortedCopy(existing)
	}
	rm := make(map[string]struct{}, len(remove))
	for i := range remove {
		rm[namespaceSnapshotChildRefKey(remove[i])] = struct{}{}
	}
	var out []storagev1alpha1.NamespaceSnapshotChildRef
	for i := range existing {
		if _, drop := rm[namespaceSnapshotChildRefKey(existing[i])]; drop {
			continue
		}
		out = append(out, existing[i])
	}
	return namespaceSnapshotChildRefsSortedCopy(out)
}

// removeNamespaceSnapshotContentChildRefsByKeys drops child content refs listed in remove (by Name).
func removeNamespaceSnapshotContentChildRefsByKeys(existing, remove []storagev1alpha1.NamespaceSnapshotContentChildRef) []storagev1alpha1.NamespaceSnapshotContentChildRef {
	if len(remove) == 0 {
		return namespaceSnapshotContentChildRefsSortedCopy(existing)
	}
	rm := make(map[string]struct{}, len(remove))
	for i := range remove {
		rm[remove[i].Name] = struct{}{}
	}
	var out []storagev1alpha1.NamespaceSnapshotContentChildRef
	for i := range existing {
		if _, drop := rm[existing[i].Name]; drop {
			continue
		}
		out = append(out, existing[i])
	}
	return namespaceSnapshotContentChildRefsSortedCopy(out)
}

func validateSyntheticChildLabelsForParent(child *storagev1alpha1.NamespaceSnapshot, parent *storagev1alpha1.NamespaceSnapshot) error {
	if child.Labels[namespacemanifest.LabelSyntheticChild] != "true" {
		return fmt.Errorf("NamespaceSnapshot %s/%s is not marked as synthetic child", child.Namespace, child.Name)
	}
	if child.Labels[namespacemanifest.LabelSyntheticParentName] != parent.Name {
		return fmt.Errorf("synthetic child %s/%s has n2b-parent-name %q, want parent name %q",
			child.Namespace, child.Name, child.Labels[namespacemanifest.LabelSyntheticParentName], parent.Name)
	}
	if child.Labels[namespacemanifest.LabelSyntheticParentUID] != string(parent.UID) {
		return fmt.Errorf("synthetic child %s/%s has n2b-parent-uid %q, want current parent UID %q (stale child or wrong object)",
			child.Namespace, child.Name, child.Labels[namespacemanifest.LabelSyntheticParentUID], string(parent.UID))
	}
	return nil
}

// mapSyntheticChildSnapshotToParent enqueues the parent named in labels for synthetic-child snapshots only
// (not a duplicate For() — it bridges child status events to parent reconcile).
// Requires n2b-parent-uid so stale map events still correlate; authoritative UID check is in reconcile.
func mapSyntheticChildSnapshotToParent(_ context.Context, o client.Object) []reconcile.Request {
	labels := o.GetLabels()
	if !namespacemanifest.IsSyntheticChildNamespaceSnapshot(labels) {
		return nil
	}
	parentName := labels[namespacemanifest.LabelSyntheticParentName]
	parentUID := labels[namespacemanifest.LabelSyntheticParentUID]
	ns := o.GetNamespace()
	if parentName == "" || ns == "" || parentUID == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: parentName}}}
}

// ensureSyntheticChildSubtreeScaffold creates the temporary synthetic child NamespaceSnapshot (if needed) and
// wires status.childrenSnapshotRefs plus root NamespaceSnapshotContent.status.childrenSnapshotContentRefs before
// the first root ManifestCaptureRequest. That way E5 subtree exclude applies to the first capture plan instead of
// relying on post–MCP drift. Idempotent.
//
// Returns (proceedWithCapture, result, err): when proceedWithCapture is false, the caller must return result.
func (r *NamespaceSnapshotReconciler) ensureSyntheticChildSubtreeScaffold(
	ctx context.Context,
	nsSnap *storagev1alpha1.NamespaceSnapshot,
	parentContent *storagev1alpha1.NamespaceSnapshotContent,
) (proceedWithCapture bool, res ctrl.Result, err error) {
	if !parentRequestsSyntheticChildTree(nsSnap) {
		return true, ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx)
	parentKey := types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}
	childName := namespacemanifest.NamespaceSnapshotSyntheticChildName(nsSnap.Name)
	childKey := types.NamespacedName{Namespace: nsSnap.Namespace, Name: childName}
	child := &storagev1alpha1.NamespaceSnapshot{}

	getErr := r.Client.Get(ctx, childKey, child)
	switch {
	case apierrors.IsNotFound(getErr):
		child = &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      childName,
				Namespace: nsSnap.Namespace,
				Labels: map[string]string{
					namespacemanifest.LabelSyntheticChild:      "true",
					namespacemanifest.LabelSyntheticParentName: nsSnap.Name,
					namespacemanifest.LabelSyntheticParentUID:  string(nsSnap.UID),
				},
			},
			Spec: storagev1alpha1.NamespaceSnapshotSpec{},
		}
		if err := r.Client.Create(ctx, child); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return false, ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
			}
			return false, ctrl.Result{}, err
		}
		logger.Info("created synthetic child NamespaceSnapshot (temporary N2b tree scaffold)", "parent", nsSnap.Name, "child", childName)
		return false, ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
	case getErr != nil:
		return false, ctrl.Result{}, getErr
	default:
		if err := validateSyntheticChildLabelsForParent(child, nsSnap); err != nil {
			return false, ctrl.Result{}, err
		}
	}

	wantRootRefs := []storagev1alpha1.NamespaceSnapshotChildRef{
		{Name: childName, Namespace: nsSnap.Namespace},
	}
	updated, err := r.patchParentRootChildrenRefsIfNeeded(ctx, parentKey, wantRootRefs)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if updated {
		return false, ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
	}

	if err := r.Client.Get(ctx, childKey, child); err != nil {
		return false, ctrl.Result{}, err
	}
	if err := validateSyntheticChildLabelsForParent(child, nsSnap); err != nil {
		return false, ctrl.Result{}, err
	}
	if child.Status.BoundSnapshotContentName == "" {
		agg := evaluateSyntheticRequiredChildState(child)
		res, err := r.patchParentSyntheticChildAggregateReady(ctx, parentKey, agg.Reason, agg.Message)
		return false, res, err
	}

	wantContentRefs := []storagev1alpha1.NamespaceSnapshotContentChildRef{
		{Name: child.Status.BoundSnapshotContentName},
	}
	contentName := parentContent.Name
	updated, err = r.patchParentContentChildRefsIfNeeded(ctx, contentName, wantContentRefs)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if updated {
		return false, ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
	}

	return true, ctrl.Result{}, nil
}

// reconcileSyntheticChildTree runs after parent N2a manifest capture has persisted (MCP on parent NSC).
// It does not alter N2a capture itself; it only adds graph + readiness gating on the parent root.
func (r *NamespaceSnapshotReconciler) reconcileSyntheticChildTree(
	ctx context.Context,
	nsSnap *storagev1alpha1.NamespaceSnapshot,
	parentContent *storagev1alpha1.NamespaceSnapshotContent,
) (ctrl.Result, error) {
	mcpName := parentContent.Status.ManifestCheckpointName
	parentKey := types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}

	proceed, res, err := r.ensureSyntheticChildSubtreeScaffold(ctx, nsSnap, parentContent)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !proceed {
		return res, nil
	}

	childName := namespacemanifest.NamespaceSnapshotSyntheticChildName(nsSnap.Name)
	childKey := types.NamespacedName{Namespace: nsSnap.Namespace, Name: childName}
	child := &storagev1alpha1.NamespaceSnapshot{}
	if err := r.Client.Get(ctx, childKey, child); err != nil {
		return ctrl.Result{}, err
	}
	if err := validateSyntheticChildLabelsForParent(child, nsSnap); err != nil {
		return ctrl.Result{}, err
	}

	agg := evaluateSyntheticRequiredChildState(child)
	if agg.Phase != syntheticChildAggregateReady {
		return r.patchParentSyntheticChildAggregateReady(ctx, parentKey, agg.Reason, agg.Message)
	}

	if err := r.patchParentRootReadyAfterSyntheticChild(ctx, parentKey, mcpName); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteNamespaceSnapshotManifestCaptureRequest(ctx, nsSnap); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// patchParentRootChildrenRefsIfNeeded returns (true, nil) if it performed an update. Status conflicts are
// retried; other errors propagate and controller-runtime will requeue.
func (r *NamespaceSnapshotReconciler) patchParentRootChildrenRefsIfNeeded(
	ctx context.Context,
	parentKey types.NamespacedName,
	want []storagev1alpha1.NamespaceSnapshotChildRef,
) (bool, error) {
	var didUpdate bool
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &storagev1alpha1.NamespaceSnapshot{}
		if err := r.Client.Get(ctx, parentKey, o); err != nil {
			return err
		}
		next := mergeNamespaceSnapshotChildRefs(o.Status.ChildrenSnapshotRefs, want)
		if namespaceSnapshotChildRefsEqualIgnoreOrder(next, o.Status.ChildrenSnapshotRefs) {
			return nil
		}
		o.Status.ChildrenSnapshotRefs = next
		o.Status.ObservedGeneration = o.Generation
		if err := r.Client.Status().Update(ctx, o); err != nil {
			return err
		}
		didUpdate = true
		return nil
	})
	return didUpdate, err
}

func (r *NamespaceSnapshotReconciler) patchParentContentChildRefsIfNeeded(
	ctx context.Context,
	contentName string,
	want []storagev1alpha1.NamespaceSnapshotContentChildRef,
) (bool, error) {
	var didUpdate bool
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		c := &storagev1alpha1.NamespaceSnapshotContent{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, c); err != nil {
			return err
		}
		next := mergeNamespaceSnapshotContentChildRefs(c.Status.ChildrenSnapshotContentRefs, want)
		if namespaceSnapshotContentChildRefsEqualIgnoreOrder(next, c.Status.ChildrenSnapshotContentRefs) {
			return nil
		}
		c.Status.ChildrenSnapshotContentRefs = next
		if err := r.Client.Status().Update(ctx, c); err != nil {
			return err
		}
		didUpdate = true
		return nil
	})
	return didUpdate, err
}

// patchParentRootChildrenRefsRemoveKeys removes only the listed snapshot ref keys (merge-safe, RetryOnConflict).
func (r *NamespaceSnapshotReconciler) patchParentRootChildrenRefsRemoveKeys(
	ctx context.Context,
	parentKey types.NamespacedName,
	remove []storagev1alpha1.NamespaceSnapshotChildRef,
) (bool, error) {
	if len(remove) == 0 {
		return false, nil
	}
	var didUpdate bool
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &storagev1alpha1.NamespaceSnapshot{}
		if err := r.Client.Get(ctx, parentKey, o); err != nil {
			return err
		}
		next := removeNamespaceSnapshotChildRefsByKeys(o.Status.ChildrenSnapshotRefs, remove)
		if namespaceSnapshotChildRefsEqualIgnoreOrder(next, o.Status.ChildrenSnapshotRefs) {
			return nil
		}
		o.Status.ChildrenSnapshotRefs = next
		o.Status.ObservedGeneration = o.Generation
		if err := r.Client.Status().Update(ctx, o); err != nil {
			return err
		}
		didUpdate = true
		return nil
	})
	return didUpdate, err
}

func (r *NamespaceSnapshotReconciler) patchParentContentChildRefsRemoveKeys(
	ctx context.Context,
	contentName string,
	remove []storagev1alpha1.NamespaceSnapshotContentChildRef,
) (bool, error) {
	if len(remove) == 0 {
		return false, nil
	}
	var didUpdate bool
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		c := &storagev1alpha1.NamespaceSnapshotContent{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, c); err != nil {
			return err
		}
		next := removeNamespaceSnapshotContentChildRefsByKeys(c.Status.ChildrenSnapshotContentRefs, remove)
		if namespaceSnapshotContentChildRefsEqualIgnoreOrder(next, c.Status.ChildrenSnapshotContentRefs) {
			return nil
		}
		c.Status.ChildrenSnapshotContentRefs = next
		if err := r.Client.Status().Update(ctx, c); err != nil {
			return err
		}
		didUpdate = true
		return nil
	})
	return didUpdate, err
}

// pruneSyntheticOwnedGraphRefsIfTreeDisabled removes only refs this reconciler added for the temporary synthetic
// scaffold when the parent opts out (spec §3.3 / INV-REF-M2 — do not touch other writers' keys).
func (r *NamespaceSnapshotReconciler) pruneSyntheticOwnedGraphRefsIfTreeDisabled(ctx context.Context, nsSnap *storagev1alpha1.NamespaceSnapshot) (ctrl.Result, error) {
	if namespacemanifest.IsSyntheticChildNamespaceSnapshot(nsSnap.GetLabels()) {
		return ctrl.Result{}, nil
	}
	if parentRequestsSyntheticChildTree(nsSnap) {
		return ctrl.Result{}, nil
	}
	parentKey := types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}
	synthRef := storagev1alpha1.NamespaceSnapshotChildRef{
		Namespace: nsSnap.Namespace,
		Name:      namespacemanifest.NamespaceSnapshotSyntheticChildName(nsSnap.Name),
	}
	updated, err := r.patchParentRootChildrenRefsRemoveKeys(ctx, parentKey, []storagev1alpha1.NamespaceSnapshotChildRef{synthRef})
	if err != nil {
		return ctrl.Result{}, err
	}
	if updated {
		return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
	}
	parentFresh := &storagev1alpha1.NamespaceSnapshot{}
	if err := r.Client.Get(ctx, parentKey, parentFresh); err != nil {
		return ctrl.Result{}, err
	}
	if parentFresh.Status.BoundSnapshotContentName == "" {
		return ctrl.Result{}, nil
	}
	childKey := types.NamespacedName{Namespace: nsSnap.Namespace, Name: synthRef.Name}
	child := &storagev1alpha1.NamespaceSnapshot{}
	if err := r.Client.Get(ctx, childKey, child); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if child.Status.BoundSnapshotContentName == "" {
		return ctrl.Result{}, nil
	}
	rm := []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: child.Status.BoundSnapshotContentName}}
	updated2, err := r.patchParentContentChildRefsRemoveKeys(ctx, parentFresh.Status.BoundSnapshotContentName, rm)
	if err != nil {
		return ctrl.Result{}, err
	}
	if updated2 {
		return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
	}
	return ctrl.Result{}, nil
}

func (r *NamespaceSnapshotReconciler) patchParentRootReadyAfterSyntheticChild(ctx context.Context, parentKey types.NamespacedName, mcpName string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &storagev1alpha1.NamespaceSnapshot{}
		if err := r.Client.Get(ctx, parentKey, o); err != nil {
			return err
		}
		o.Status.ObservedGeneration = o.Generation
		meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             snapshot.ReasonCompleted,
			Message:            fmt.Sprintf("manifest capture complete (ManifestCheckpoint %s); synthetic child ready", mcpName),
			ObservedGeneration: o.Generation,
		})
		return r.Client.Status().Update(ctx, o)
	})
}

func (r *NamespaceSnapshotReconciler) patchParentSyntheticChildAggregateReady(
	ctx context.Context,
	parentKey types.NamespacedName,
	reason string,
	msg string,
) (ctrl.Result, error) {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		nsSnap := &storagev1alpha1.NamespaceSnapshot{}
		if err := r.Client.Get(ctx, parentKey, nsSnap); err != nil {
			return err
		}
		nsSnap.Status.ObservedGeneration = nsSnap.Generation
		meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: nsSnap.Generation,
		})
		return r.Client.Status().Update(ctx, nsSnap)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
}

// parentRequestsSyntheticChildTree is true when the snapshot opts into the temporary synthetic child tree
// and is not itself the synthetic child (synthetic children stay N2a leaves).
func parentRequestsSyntheticChildTree(nsSnap *storagev1alpha1.NamespaceSnapshot) bool {
	if namespacemanifest.IsSyntheticChildNamespaceSnapshot(nsSnap.Labels) {
		return false
	}
	return namespacemanifest.SyntheticChildTreeAnnotationEnabled(nsSnap.Annotations)
}
