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

// reconcileSyntheticChildTree runs after parent N2a manifest capture has persisted (MCP on parent NSC).
// It does not alter N2a capture itself; it only adds graph + readiness gating on the parent root.
func (r *NamespaceSnapshotReconciler) reconcileSyntheticChildTree(
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
					namespacemanifest.LabelSyntheticChild:      "true",
					namespacemanifest.LabelSyntheticParentName: nsSnap.Name,
					namespacemanifest.LabelSyntheticParentUID:  string(nsSnap.UID),
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
		logger.Info("created synthetic child NamespaceSnapshot (temporary N2b tree scaffold)", "parent", nsSnap.Name, "child", childName)
		return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
	case err != nil:
		return ctrl.Result{}, err
	default:
		if err := validateSyntheticChildLabelsForParent(child, nsSnap); err != nil {
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
	if err := validateSyntheticChildLabelsForParent(child, nsSnap); err != nil {
		return ctrl.Result{}, err
	}
	if child.Status.BoundSnapshotContentName == "" {
		agg := evaluateSyntheticRequiredChildState(child)
		return r.patchParentSyntheticChildAggregateReady(ctx, parentKey, agg.Reason, agg.Message)
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
