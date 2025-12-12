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

package usecase

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// ManifestCaptureService handles creation of ManifestCaptureRequest
// This service is a thin wrapper around client.Create - it does NOT:
// - Automatically collect namespace resources (caller must provide targets)
// - Wait for MCR to be ready (use controller reconciliation instead)
// - Update existing MCR targets (targets are immutable)
type ManifestCaptureService struct {
	client client.Client
	logger logger.LoggerInterface
}

// NewManifestCaptureService creates a new ManifestCaptureService
func NewManifestCaptureService(client client.Client, logger logger.LoggerInterface) *ManifestCaptureService {
	return &ManifestCaptureService{
		client: client,
		logger: logger,
	}
}

// CreateManifestCaptureRequest creates a ManifestCaptureRequest with the provided targets
// Targets must be provided by the caller (e.g., VirtualMachineSnapshotController)
// This ensures RBAC compliance - the caller must have GET permissions for all targets
//
// If MCR with the same name already exists, returns an error (targets are immutable)
// The caller should generate a unique name (e.g., <snapshot-name>-capture-<uid>)
func (s *ManifestCaptureService) CreateManifestCaptureRequest(
	ctx context.Context,
	namespace string,
	requestName string,
	targets []storagev1alpha1.ManifestTarget,
) error {
	if len(targets) == 0 {
		return fmt.Errorf("targets cannot be empty - caller must specify which objects to capture")
	}

	// Check if MCR already exists
	existing := &storagev1alpha1.ManifestCaptureRequest{}
	err := s.client.Get(ctx, client.ObjectKey{
		Namespace: namespace,
		Name:      requestName,
	}, existing)

	if err == nil {
		// MCR already exists - targets are immutable, return error
		return fmt.Errorf("ManifestCaptureRequest %s/%s already exists", namespace, requestName)
	}

	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to check if ManifestCaptureRequest exists: %w", err)
	}

	// Create new MCR
	mcr := &storagev1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      requestName,
			Namespace: namespace,
		},
		Spec: storagev1alpha1.ManifestCaptureRequestSpec{
			Targets: targets,
		},
	}

	if err := s.client.Create(ctx, mcr); err != nil {
		return fmt.Errorf("failed to create ManifestCaptureRequest: %w", err)
	}

	s.logger.Info("Created ManifestCaptureRequest",
		"name", requestName,
		"namespace", namespace,
		"targets", len(targets))

	return nil
}
