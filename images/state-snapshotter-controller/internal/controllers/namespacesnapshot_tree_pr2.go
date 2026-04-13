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

// N2b PR2 synthetic one-child tree (temporary scaffold until domain wiring ~PR5).
//
// Does not change N2a leaf semantics: synthetic children have no PR2 opt-in annotation and run normal
// reconcileCaptureN2a. Parents with n2b-pr2-synthetic-tree only add a post-MCP step that waits for one child.
//
// Annotation name and parentName+"-child" naming are scaffold-only; parent/child Ready aggregation — PR3
// (namespacesnapshot_synthetic_child_pr3.go); PR5 replaces synthetic path.

package controllers

import (
	"context"
	"fmt"
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

func validateSyntheticChildLabelsForPR2Parent(child *storagev1alpha1.NamespaceSnapshot, parent *storagev1alpha1.NamespaceSnapshot) error {
	if child.Labels[namespacemanifest.LabelN2bSyntheticChild] != "true" {
		return fmt.Errorf("NamespaceSnapshot %s/%s is not marked as PR2 synthetic child", child.Namespace, child.Name)
	}
	if child.Labels[namespacemanifest.LabelN2bParentName] != parent.Name {
		return fmt.Errorf("synthetic child %s/%s has n2b-parent-name %q, want parent name %q",
			child.Namespace, child.Name, child.Labels[namespacemanifest.LabelN2bParentName], parent.Name)
	}
	if child.Labels[namespacemanifest.LabelN2bParentUID] != string(parent.UID) {
		return fmt.Errorf("synthetic child %s/%s has n2b-parent-uid %q, want current parent UID %q (stale child or wrong object)",
			child.Namespace, child.Name, child.Labels[namespacemanifest.LabelN2bParentUID], string(parent.UID))
	}
	return nil
}

// mapSyntheticChildNamespaceSnapshotToParent enqueues the parent named in labels only for PR2 synthetic
// children (not a duplicate For() — it bridges child status events to parent reconcile).
// Requires n2b-parent-uid so stale map events still correlate; authoritative UID check is in reconcile.
func mapSyntheticChildNamespaceSnapshotToParent(_ context.Context, o client.Object) []reconcile.Request {
	labels := o.GetLabels()
	if !namespacemanifest.N2bIsSyntheticChildNamespaceSnapshot(labels) {
		return nil
	}
	parentName := labels[namespacemanifest.LabelN2bParentName]
	parentUID := labels[namespacemanifest.LabelN2bParentUID]
	ns := o.GetNamespace()
	if parentName == "" || ns == "" || parentUID == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: parentName}}}
}

// reconcileSyntheticTreePR2 runs after parent N2a manifest capture has persisted (MCP on parent NSC).
// It does not alter N2a capture itself; it only adds graph + readiness gating on the parent root.
func (r *NamespaceSnapshotReconciler) reconcileSyntheticTreePR2(
	ctx context.Context,
	nsSnap *storagev1alpha1.NamespaceSnapshot,
	parentContent *storagev1alpha1.NamespaceSnapshotContent,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	mcpName := parentContent.Status.ManifestCheckpointName
	parentKey := types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}

	childName := namespacemanifest.NamespaceSnapshotSyntheticChildName(nsSnap.Name)
	childKey := types.NamespacedName{Namespace: nsSnap.Namespace, Name: childName}
	child := &storagev1alpha1.NamespaceSnapshot{}

	err := r.Client.Get(ctx, childKey, child)
	switch {
	case apierrors.IsNotFound(err):
		child = &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      childName,
				Namespace: nsSnap.Namespace,
				Labels: map[string]string{
					namespacemanifest.LabelN2bSyntheticChild: "true",
					namespacemanifest.LabelN2bParentName:     nsSnap.Name,
					namespacemanifest.LabelN2bParentUID:      string(nsSnap.UID),
				},
			},
			Spec: storagev1alpha1.NamespaceSnapshotSpec{},
		}
		if err := r.Client.Create(ctx, child); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
			}
			return ctrl.Result{}, err
		}
		logger.Info("created PR2 synthetic child NamespaceSnapshot", "parent", nsSnap.Name, "child", childName)
		return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
	case err != nil:
		return ctrl.Result{}, err
	default:
		if err := validateSyntheticChildLabelsForPR2Parent(child, nsSnap); err != nil {
			return ctrl.Result{}, err
		}
	}

	wantRootRefs := []storagev1alpha1.NamespaceSnapshotChildRef{
		{Name: childName, Namespace: nsSnap.Namespace},
	}
	updated, err := r.patchParentRootChildrenRefsIfNeeded(ctx, parentKey, wantRootRefs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if updated {
		return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
	}

	if err := r.Client.Get(ctx, childKey, child); err != nil {
		return ctrl.Result{}, err
	}
	if err := validateSyntheticChildLabelsForPR2Parent(child, nsSnap); err != nil {
		return ctrl.Result{}, err
	}
	if child.Status.BoundSnapshotContentName == "" {
		agg := evaluateSyntheticRequiredChildStateForPR2(child)
		return r.patchParentSyntheticTreeAggregateReady(ctx, parentKey, agg.Reason, agg.Message)
	}

	wantContentRefs := []storagev1alpha1.NamespaceSnapshotContentChildRef{
		{Name: child.Status.BoundSnapshotContentName},
	}
	contentName := parentContent.Name
	updated, err = r.patchParentContentChildRefsIfNeeded(ctx, contentName, wantContentRefs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if updated {
		return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
	}

	if err := r.Client.Get(ctx, childKey, child); err != nil {
		return ctrl.Result{}, err
	}
	if err := validateSyntheticChildLabelsForPR2Parent(child, nsSnap); err != nil {
		return ctrl.Result{}, err
	}

	agg := evaluateSyntheticRequiredChildStateForPR2(child)
	if agg.Phase != syntheticChildAggregateReady {
		return r.patchParentSyntheticTreeAggregateReady(ctx, parentKey, agg.Reason, agg.Message)
	}

	if err := r.patchParentRootReadyAfterSyntheticChild(ctx, parentKey, mcpName); err != nil {
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
		if namespaceSnapshotChildRefsEqual(o.Status.ChildrenSnapshotRefs, want) {
			return nil
		}
		o.Status.ChildrenSnapshotRefs = append([]storagev1alpha1.NamespaceSnapshotChildRef(nil), want...)
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
		if namespaceSnapshotContentChildRefsEqual(c.Status.ChildrenSnapshotContentRefs, want) {
			return nil
		}
		c.Status.ChildrenSnapshotContentRefs = append([]storagev1alpha1.NamespaceSnapshotContentChildRef(nil), want...)
		if err := r.Client.Status().Update(ctx, c); err != nil {
			return err
		}
		didUpdate = true
		return nil
	})
	return didUpdate, err
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

func (r *NamespaceSnapshotReconciler) patchParentSyntheticTreeAggregateReady(
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

// skipN2bPR2SyntheticTree: synthetic child must stay N2a leaf. Naming is PR2-scoped technical debt until
// a neutral tree-strategy hook exists (~PR5).
func skipN2bPR2SyntheticTree(nsSnap *storagev1alpha1.NamespaceSnapshot) bool {
	return namespacemanifest.N2bIsSyntheticChildNamespaceSnapshot(nsSnap.Labels)
}

func parentWantsN2bPR2SyntheticTree(nsSnap *storagev1alpha1.NamespaceSnapshot) bool {
	if skipN2bPR2SyntheticTree(nsSnap) {
		return false
	}
	return namespacemanifest.N2bPR2SyntheticTreeEnabled(nsSnap.Annotations)
}
