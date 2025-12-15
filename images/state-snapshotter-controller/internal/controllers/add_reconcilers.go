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

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// AddManifestCheckpointControllerToManager adds the ManifestCheckpoint controller to the manager.
func AddManifestCheckpointControllerToManager(
	mgr ctrl.Manager,
	log logger.LoggerInterface,
	cfg *config.Options,
	ctx context.Context,
) error {
	reconciler := &ManifestCheckpointController{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(), // Direct API reader for read-after-write scenarios
		Scheme:    mgr.GetScheme(),
		Logger:    log,
		Config:    cfg,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return err
	}
	// Start TTL scanner in background goroutine
	// Scanner periodically lists all MCRs and deletes expired ones based on completionTimestamp + TTL
	reconciler.StartTTLScanner(ctx, mgr.GetClient())
	return nil
}

// NOTE: AddRetainerControllerToManager has been removed.
// IRetainer has been replaced with ObjectKeeper, which is managed by deckhouse-controller.
