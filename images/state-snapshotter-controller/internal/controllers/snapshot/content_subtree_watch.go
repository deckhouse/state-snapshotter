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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

const maxSnapshotContentAncestorHops = 32

// snapshotContentControllerParentName returns the controller owner SnapshotContent name, if any.
func snapshotContentControllerParentName(sc *storagev1alpha1.SnapshotContent) string {
	if sc == nil {
		return ""
	}
	for _, ref := range sc.OwnerReferences {
		if ref.Kind != controllercommon.KindSnapshotContent || ref.Name == "" {
			continue
		}
		if ref.Controller != nil && !*ref.Controller {
			continue
		}
		return ref.Name
	}
	return ""
}

// rootSnapshotContentName walks controller owner refs from contentName to the root SnapshotContent.
func rootSnapshotContentName(ctx context.Context, c client.Reader, contentName string) (string, error) {
	if contentName == "" {
		return "", fmt.Errorf("empty SnapshotContent name")
	}
	current := contentName
	for hop := 0; hop < maxSnapshotContentAncestorHops; hop++ {
		sc := &storagev1alpha1.SnapshotContent{}
		if err := c.Get(ctx, client.ObjectKey{Name: current}, sc); err != nil {
			return "", err
		}
		parent := snapshotContentControllerParentName(sc)
		if parent == "" {
			return current, nil
		}
		if parent == current {
			return "", fmt.Errorf("SnapshotContent %q has self-referential controller parent", current)
		}
		current = parent
	}
	return "", fmt.Errorf("SnapshotContent ancestor depth exceeded from %q", contentName)
}

// MapSnapshotContentUpdateToSnapshots enqueues Snapshots bound to content and, when content is a
// descendant in the SnapshotContent tree, Snapshots bound to the root SnapshotContent ancestor.
func MapSnapshotContentUpdateToSnapshots(ctx context.Context, c client.Client, content *storagev1alpha1.SnapshotContent) []reconcile.Request {
	if content == nil || content.Name == "" {
		return nil
	}
	seen := make(map[types.NamespacedName]struct{})
	var out []reconcile.Request
	add := func(reqs []reconcile.Request) {
		for _, req := range reqs {
			if _, ok := seen[req.NamespacedName]; ok {
				continue
			}
			seen[req.NamespacedName] = struct{}{}
			out = append(out, req)
		}
	}
	add(MapSnapshotContentToBoundSnapshots(ctx, c, content))
	rootName, err := rootSnapshotContentName(ctx, c, content.Name)
	if err != nil || rootName == "" || rootName == content.Name {
		return out
	}
	rootContent := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: rootName}}
	add(MapSnapshotContentToBoundSnapshots(ctx, c, rootContent))
	return out
}

func snapshotContentSubtreeWakeupStatusChanged(oldSC, newSC *storagev1alpha1.SnapshotContent) bool {
	if newSC == nil {
		return false
	}
	if oldSC == nil {
		return true
	}
	if oldSC.Status.ManifestCheckpointName != newSC.Status.ManifestCheckpointName {
		return true
	}
	if !controllercommon.SnapshotContentChildRefsEqualIgnoreOrder(
		oldSC.Status.ChildrenSnapshotContentRefs,
		newSC.Status.ChildrenSnapshotContentRefs,
	) {
		return true
	}
	return snapshotContentReadyConditionChanged(oldSC.Status.Conditions, newSC.Status.Conditions)
}

func snapshotContentReadyConditionChanged(oldConds, newConds []metav1.Condition) bool {
	oldReady := meta.FindStatusCondition(oldConds, snapshot.ConditionReady)
	newReady := meta.FindStatusCondition(newConds, snapshot.ConditionReady)
	if oldReady == nil && newReady == nil {
		return false
	}
	if oldReady == nil || newReady == nil {
		return true
	}
	return oldReady.Status != newReady.Status ||
		oldReady.Reason != newReady.Reason ||
		oldReady.Message != newReady.Message
}
