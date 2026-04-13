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

package controllers

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

const reasonChildSnapshotPending = "ChildSnapshotPending"

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

// mapSyntheticChildNamespaceSnapshotToParent requeues the parent when a PR2 synthetic child NamespaceSnapshot changes.
func mapSyntheticChildNamespaceSnapshotToParent(_ context.Context, o client.Object) []reconcile.Request {
	labels := o.GetLabels()
	if !namespacemanifest.N2bIsSyntheticChildNamespaceSnapshot(labels) {
		return nil
	}
	parentName := labels[namespacemanifest.LabelN2bParentName]
	ns := o.GetNamespace()
	if parentName == "" || ns == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: parentName}}}
}

// reconcileSyntheticTreePR2 runs after parent N2a manifest capture has persisted (content status updated).
// It ensures one synthetic child, writes graph refs, and sets parent root Ready only when the child is Ready.
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
		if child.Labels[namespacemanifest.LabelN2bSyntheticChild] != "true" ||
			child.Labels[namespacemanifest.LabelN2bParentUID] != string(nsSnap.UID) {
			return ctrl.Result{}, fmt.Errorf("NamespaceSnapshot %s/%s exists but is not the PR2 synthetic child of parent %s (labels conflict)",
				child.Namespace, child.Name, nsSnap.Name)
		}
	}

	wantRootRefs := []storagev1alpha1.NamespaceSnapshotChildRef{
		{Name: childName, Namespace: nsSnap.Namespace},
	}
	parentRoot := &storagev1alpha1.NamespaceSnapshot{}
	if err := r.Client.Get(ctx, parentKey, parentRoot); err != nil {
		return ctrl.Result{}, err
	}
	if !namespaceSnapshotChildRefsEqual(parentRoot.Status.ChildrenSnapshotRefs, wantRootRefs) {
		parentRoot.Status.ChildrenSnapshotRefs = append([]storagev1alpha1.NamespaceSnapshotChildRef(nil), wantRootRefs...)
		parentRoot.Status.ObservedGeneration = parentRoot.Generation
		if err := r.Client.Status().Update(ctx, parentRoot); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
	}

	if err := r.Client.Get(ctx, childKey, child); err != nil {
		return ctrl.Result{}, err
	}
	if child.Status.BoundSnapshotContentName == "" {
		return r.markParentWaitingForSyntheticChild(ctx, parentKey,
			"waiting for synthetic child NamespaceSnapshot to bind NamespaceSnapshotContent")
	}

	wantContentRefs := []storagev1alpha1.NamespaceSnapshotContentChildRef{
		{Name: child.Status.BoundSnapshotContentName},
	}
	contentKey := client.ObjectKey{Name: parentContent.Name}
	if err := r.Client.Get(ctx, contentKey, parentContent); err != nil {
		return ctrl.Result{}, err
	}
	if !namespaceSnapshotContentChildRefsEqual(parentContent.Status.ChildrenSnapshotContentRefs, wantContentRefs) {
		parentContent.Status.ChildrenSnapshotContentRefs = append([]storagev1alpha1.NamespaceSnapshotContentChildRef(nil), wantContentRefs...)
		if err := r.Client.Status().Update(ctx, parentContent); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
	}

	childReady := meta.FindStatusCondition(child.Status.Conditions, snapshot.ConditionReady)
	if childReady == nil || childReady.Status != metav1.ConditionTrue {
		msg := "waiting for synthetic child NamespaceSnapshot Ready=True"
		if childReady != nil && childReady.Message != "" {
			msg = fmt.Sprintf("waiting for synthetic child Ready: %s", childReady.Message)
		}
		return r.markParentWaitingForSyntheticChild(ctx, parentKey, msg)
	}

	parentFinal := &storagev1alpha1.NamespaceSnapshot{}
	if err := r.Client.Get(ctx, parentKey, parentFinal); err != nil {
		return ctrl.Result{}, err
	}
	parentFinal.Status.ObservedGeneration = parentFinal.Generation
	meta.SetStatusCondition(&parentFinal.Status.Conditions, metav1.Condition{
		Type:               snapshot.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             snapshot.ReasonCompleted,
		Message:            fmt.Sprintf("manifest capture complete (ManifestCheckpoint %s); synthetic child ready", mcpName),
		ObservedGeneration: parentFinal.Generation,
	})
	if err := r.Client.Status().Update(ctx, parentFinal); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *NamespaceSnapshotReconciler) markParentWaitingForSyntheticChild(ctx context.Context, parentKey types.NamespacedName, msg string) (ctrl.Result, error) {
	nsSnap := &storagev1alpha1.NamespaceSnapshot{}
	if err := r.Client.Get(ctx, parentKey, nsSnap); err != nil {
		return ctrl.Result{}, err
	}
	nsSnap.Status.ObservedGeneration = nsSnap.Generation
	meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
		Type:               snapshot.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reasonChildSnapshotPending,
		Message:            msg,
		ObservedGeneration: nsSnap.Generation,
	})
	if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
}

// reconcileCaptureN2a entry: synthetic child is always a leaf — never run PR2 tree from it.
func skipN2bPR2SyntheticTree(nsSnap *storagev1alpha1.NamespaceSnapshot) bool {
	return namespacemanifest.N2bIsSyntheticChildNamespaceSnapshot(nsSnap.Labels)
}

// parentWantsN2bPR2SyntheticTree is true when the root opts in and is not itself a synthetic child.
func parentWantsN2bPR2SyntheticTree(nsSnap *storagev1alpha1.NamespaceSnapshot) bool {
	if skipN2bPR2SyntheticTree(nsSnap) {
		return false
	}
	return namespacemanifest.N2bPR2SyntheticTreeEnabled(nsSnap.Annotations)
}
