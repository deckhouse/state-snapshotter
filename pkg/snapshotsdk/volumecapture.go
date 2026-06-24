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

package snapshotsdk

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/storagefoundation"
)

// VolumeCaptureProvider abstracts the data-leg backend a snapshot's PVCs are captured through. The SDK
// ships a storage-foundation VolumeCaptureRequest implementation (NewStorageFoundationProvider); domains
// with a different data-capture mechanism can supply their own.
type VolumeCaptureProvider interface {
	// VCRName returns the deterministic, snapshot-owned capture-request name for a snapshot UID.
	VCRName(snapshotUID types.UID) string
	// EnsureVCR reconciles the snapshot's capture request toward the desired owner reference and targets.
	EnsureVCR(ctx context.Context, namespace, name string, ownerRef metav1.OwnerReference, targets []Target) error
	// OwnedPVCTargets returns the PVC targets recorded on the snapshot's capture request (for the manifest
	// leg). A missing request yields no targets.
	OwnedPVCTargets(ctx context.Context, namespace, vcrName string) ([]Target, error)
}

// NewStorageFoundationProvider returns the default VolumeCaptureProvider, backed by the storage-foundation
// VolumeCaptureRequest CRD (accessed via unstructured objects, so there is no Go dependency on the
// foundation API module).
func NewStorageFoundationProvider(c client.Client) VolumeCaptureProvider {
	return storagefoundation.NewProvider(c)
}
