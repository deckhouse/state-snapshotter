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

package volumecapture

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
)

// OwnedPVCManifestTargetsForSnapshot returns explicit PVC manifest targets for a logical capture node (PR-6 resolver).
func OwnedPVCManifestTargetsForSnapshot(
	ctx context.Context,
	c client.Reader,
	snap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
	dataBearing DataBearingKindFunc,
) ([]namespacemanifest.ManifestTarget, error) {
	vol, err := ListOwnedPVCTargetsForLogicalContent(ctx, c, snap, content, dataBearing)
	if err != nil {
		return nil, err
	}
	return namespacemanifest.ManifestTargetsFromVolumeTargets(vol), nil
}

// OwnedPVCManifestTargetsForSnapshotContent is the domain/demo entry point when only namespace + content are known.
func OwnedPVCManifestTargetsForSnapshotContent(
	ctx context.Context,
	c client.Reader,
	namespace string,
	content *storagev1alpha1.SnapshotContent,
) ([]namespacemanifest.ManifestTarget, error) {
	vol, err := ListOwnedPVCTargetsForSnapshotContent(ctx, c, namespace, content)
	if err != nil {
		return nil, err
	}
	return namespacemanifest.ManifestTargetsFromVolumeTargets(vol), nil
}
