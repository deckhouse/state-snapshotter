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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
)

// reconcileImportBound handles Snapshot objects created by the import build endpoint
// (spec.existingContentRef is set). It skips all capture logic (MCR/VCR/child graph)
// and binds directly to the referenced SnapshotContent, then mirrors Ready.
func (r *SnapshotReconciler) reconcileImportBound(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	rootOK *deckhousev1alpha1.ObjectKeeper,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	contentName := nsSnap.Spec.ExistingContentRef.Name

	content := &storagev1alpha1.SnapshotContent{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("import-bound: SnapshotContent not found yet, requeueing", "contentName", contentName)
			notFoundErr := fmt.Errorf("SnapshotContent %s not found", contentName)
			mirrorErr := r.mirrorSnapshotReadyFromBoundContent(ctx, nsSnap, &storagev1alpha1.SnapshotContent{ObjectMeta: content.ObjectMeta}, notFoundErr)
			if mirrorErr != nil {
				return ctrl.Result{}, mirrorErr
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get SnapshotContent %s: %w", contentName, err)
	}

	// Ensure the ObjectKeeper (retention controller) owns the root SnapshotContent so
	// that TTL-based cleanup applies after the Snapshot is deleted.
	if rootOK != nil {
		if _, err := controllercommon.EnsureLifecycleOwnerRef(
			ctx, r.Client, content,
			controllercommon.RootObjectKeeperOwnerReference(rootOK),
		); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure ObjectKeeper ownerRef on SnapshotContent %s: %w", contentName, err)
		}
	}

	// Bind the Snapshot to the content if not already done.
	if nsSnap.Status.BoundSnapshotContentName != contentName {
		nsSnap.Status.BoundSnapshotContentName = contentName
		nsSnap.Status.ObservedGeneration = nsSnap.Generation
		if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Mirror Ready condition from the bound SnapshotContent (same contract as the
	// normal post-capture path).
	if err := r.mirrorSnapshotReadyFromBoundContent(ctx, nsSnap, content, nil); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}
