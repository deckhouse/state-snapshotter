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
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// AddManifestCheckpointControllerToManager adds the ManifestCheckpoint controller to the manager
// and starts TTL scanner as a leader-only runnable.
//
// TTL scanner runs only on the leader replica to prevent duplicate deletion attempts.
// When leadership changes, the scanner context is cancelled and scanner stops gracefully.
func AddManifestCheckpointControllerToManager(
	ctx context.Context,
	mgr ctrl.Manager,
	log logger.LoggerInterface,
	cfg *config.Options,
) error {
	reconciler, err := NewManifestCheckpointController(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		mgr.GetScheme(),
		log,
		cfg,
	)
	if err != nil {
		return err
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return err
	}
	// Start TTL scanner as leader-only runnable
	// RunnableFunc is executed only on the leader replica.
	// On leadership change, ctx is cancelled and the scanner stops gracefully.
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		reconciler.runTTLScanner(ctx, mgr.GetClient())
		return nil
	})); err != nil {
		return err
	}
	return nil
}
