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
	"fmt"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// AddManifestCheckpointControllerToManager adds the ManifestCheckpoint controller to the manager.
func AddManifestCheckpointControllerToManager(
	mgr ctrl.Manager,
	log logger.LoggerInterface,
	cfg *config.Options,
) error {
	reconciler := &ManifestCheckpointController{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Logger: log,
		Config: cfg,
	}
	return reconciler.SetupWithManager(mgr)
}

// AddRetainerControllerToManager adds the Retainer controller to the manager.
// This is a system controller that requires privileged access to GET any namespaced objects.
func AddRetainerControllerToManager(
	mgr ctrl.Manager,
	log logger.LoggerInterface,
) error {
	// Build dynamic client for accessing arbitrary API resources
	dyn, err := dynamic.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}

	// Build RESTMapper for efficient Kind-to-resource mapping
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("failed to create discovery client: %w", err)
	}

	// Create RESTMapper with caching
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(
		memory.NewMemCacheClient(discoveryClient),
	)

	reconciler := &RetainerController{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Logger:     log,
		dyn:        dyn,
		restMapper: restMapper,
	}
	return reconciler.SetupWithManager(mgr)
}
