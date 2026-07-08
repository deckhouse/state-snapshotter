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

package usecase

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	"github.com/deckhouse/state-snapshotter/api/names"
)

// ObjectKeeper wire constants (mirror internal/controllers/common/constants.go; duplicated here so the
// upload usecase does not depend on the controllers layer).
const (
	deckhouseObjectKeeperAPIVersion = "deckhouse.io/v1alpha1"
	kindObjectKeeper                = "ObjectKeeper"
	objectKeeperModeFollowObject    = "FollowObject"
)

// EnsureReconstructedManifestCheckpointObjectKeeper creates (idempotently) the dedicated ObjectKeeper that
// anchors the reconstructed (import) ManifestCheckpoint for one import snapshot node, and returns the
// controller ownerReference the caller stamps onto that MCP so it is GC-safe from birth.
//
// Content-single-writer design §10.1: on the capture path the MCP is born owned by the MCR execution
// ObjectKeeper, so it is GC-safe immediately; on import the reconstructed MCP is created out-of-band at
// upload time, before the eager SnapshotContent shell necessarily exists, and a cluster-scoped MCP cannot
// be owned by the namespaced snapshot CR. Without an anchor a crash/delete during import would orphan the
// MCP + chunks with no sweeper. This ObjectKeeper FollowObjects the import snapshot itself (mode
// FollowObject, no TTL), so if the snapshot is deleted while the import is still pending the keeper — and
// with it the MCP + chunks — cascade away. Once the SnapshotContent materializes, the aggregator re-parents
// the MCP onto the content and removes this now-redundant keeper (see
// ensureManifestCheckpointOwnedByContent). The name is keyed by the snapshot UID with a DISTINCT prefix
// (names.ImportManifestCheckpointObjectKeeperName) so it never collides with the snapshot's root
// ObjectKeeper.
func EnsureReconstructedManifestCheckpointObjectKeeper(
	ctx context.Context,
	c client.Client,
	snapshotObj client.Object,
	snapshotGVK schema.GroupVersionKind,
) (metav1.OwnerReference, error) {
	uid := snapshotObj.GetUID()
	if uid == "" {
		return metav1.OwnerReference{}, fmt.Errorf("snapshot %s/%s has no UID", snapshotObj.GetNamespace(), snapshotObj.GetName())
	}
	name := names.ImportManifestCheckpointObjectKeeperName(uid)

	ok := &deckhousev1alpha1.ObjectKeeper{}
	err := c.Get(ctx, client.ObjectKey{Name: name}, ok)
	switch {
	case apierrors.IsNotFound(err):
		ok = &deckhousev1alpha1.ObjectKeeper{
			TypeMeta: metav1.TypeMeta{
				APIVersion: deckhouseObjectKeeperAPIVersion,
				Kind:       kindObjectKeeper,
			},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: deckhousev1alpha1.ObjectKeeperSpec{
				Mode: objectKeeperModeFollowObject,
				FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
					APIVersion: snapshotGVK.GroupVersion().String(),
					Kind:       snapshotGVK.Kind,
					Namespace:  snapshotObj.GetNamespace(),
					Name:       snapshotObj.GetName(),
					UID:        string(uid),
				},
			},
		}
		if cerr := c.Create(ctx, ok); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return metav1.OwnerReference{}, fmt.Errorf("create import ObjectKeeper %s: %w", name, cerr)
		}
		// Re-get to obtain the UID (needed for the MCP ownerReference), tolerating the AlreadyExists race.
		if gerr := c.Get(ctx, client.ObjectKey{Name: name}, ok); gerr != nil {
			return metav1.OwnerReference{}, fmt.Errorf("get import ObjectKeeper %s after create: %w", name, gerr)
		}
	case err != nil:
		return metav1.OwnerReference{}, fmt.Errorf("get import ObjectKeeper %s: %w", name, err)
	}

	controllerTrue := true
	return metav1.OwnerReference{
		APIVersion: deckhouseObjectKeeperAPIVersion,
		Kind:       kindObjectKeeper,
		Name:       ok.Name,
		UID:        ok.UID,
		Controller: &controllerTrue,
	}, nil
}
