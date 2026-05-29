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

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func logRootDiagEnter(ctx context.Context, nsSnap *storagev1alpha1.Snapshot) {
	if nsSnap == nil {
		return
	}
	snap := types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}
	logRootDiag(ctx, snap, "reconcileEnter",
		"snapshotUID", string(nsSnap.UID),
		"generation", nsSnap.Generation,
		"childrenSnapshotRefs", len(nsSnap.Status.ChildrenSnapshotRefs),
		"boundContent", nsSnap.Status.BoundSnapshotContentName,
		"manifestCaptureRequestName", nsSnap.Status.ManifestCaptureRequestName,
	)
}

func logRootDiagMCR(ctx context.Context, nsSnap *storagev1alpha1.Snapshot, action string, extra ...interface{}) {
	if nsSnap == nil {
		return
	}
	snap := types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}
	kv := append([]interface{}{"mcrAction", action, "mcrName", "snap-" + string(nsSnap.UID)}, extra...)
	logRootDiag(ctx, snap, "mcrLifecycle", kv...)
}

func logRootDiag(ctx context.Context, snap types.NamespacedName, phase string, keysAndValues ...interface{}) {
	args := append([]interface{}{"rootDiag", true, "snapshot", snap.String(), "phase", phase}, keysAndValues...)
	log.FromContext(ctx).Info("root-diag", args...)
}

func logRootDiagReturn(ctx context.Context, snap types.NamespacedName, reason string, res ctrl.Result, err error) {
	kv := []interface{}{
		"returnReason", reason,
		"requeue", res.Requeue,
		"requeueAfter", res.RequeueAfter.String(),
	}
	if err != nil {
		kv = append(kv, "error", err.Error())
	}
	logRootDiag(ctx, snap, "return", kv...)
}

func snapshotDiagSummary(nsSnap *storagev1alpha1.Snapshot, content *storagev1alpha1.SnapshotContent) map[string]interface{} {
	out := map[string]interface{}{
		"childrenSnapshotRefs": len(nsSnap.Status.ChildrenSnapshotRefs),
		"boundContent":         nsSnap.Status.BoundSnapshotContentName,
	}
	if gr := meta.FindStatusCondition(nsSnap.Status.Conditions, snapshotpkg.ConditionGraphReady); gr != nil {
		out["graphReady"] = fmt.Sprintf("%s/%s", gr.Status, gr.Reason)
	}
	if rd := meta.FindStatusCondition(nsSnap.Status.Conditions, snapshotpkg.ConditionReady); rd != nil {
		out["snapshotReady"] = fmt.Sprintf("%s/%s", rd.Status, rd.Reason)
	} else {
		out["snapshotReady"] = "absent"
	}
	if content != nil {
		out["contentChildrenSCRefs"] = len(content.Status.ChildrenSnapshotContentRefs)
		out["contentMCP"] = content.Status.ManifestCheckpointName
		if cr := meta.FindStatusCondition(content.Status.Conditions, snapshotpkg.ConditionReady); cr != nil {
			out["contentReady"] = fmt.Sprintf("%s/%s", cr.Status, cr.Reason)
		}
	}
	return out
}
