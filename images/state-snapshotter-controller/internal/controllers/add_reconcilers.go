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

package controllers

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/demo"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/dsc"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/genericbinder"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/manifestcapture"
	snapshotcontroller "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

type GenericSnapshotBinderController = genericbinder.GenericSnapshotBinderController
type SnapshotContentController = snapshotcontent.SnapshotContentController
type ManifestCheckpointController = manifestcapture.ManifestCheckpointController
type DomainSpecificSnapshotControllerReconciler = dsc.DomainSpecificSnapshotControllerReconciler

func NewGenericSnapshotBinderController(
	c client.Client,
	apiReader client.Reader,
	scheme *runtime.Scheme,
	cfg *config.Options,
	snapshotGVKs []schema.GroupVersionKind,
) (*GenericSnapshotBinderController, error) {
	return genericbinder.NewGenericSnapshotBinderController(c, apiReader, scheme, cfg, snapshotGVKs)
}

func NewSnapshotContentController(
	c client.Client,
	apiReader client.Reader,
	scheme *runtime.Scheme,
	restMapper meta.RESTMapper,
	cfg *config.Options,
	snapshotContentGVKs []schema.GroupVersionKind,
) (*SnapshotContentController, error) {
	return snapshotcontent.NewSnapshotContentController(c, apiReader, scheme, restMapper, cfg, snapshotContentGVKs)
}

func NewManifestCheckpointController(
	c client.Client,
	apiReader client.Reader,
	scheme *runtime.Scheme,
	log logger.LoggerInterface,
	cfg *config.Options,
) (*ManifestCheckpointController, error) {
	return manifestcapture.NewManifestCheckpointController(c, apiReader, scheme, log, cfg)
}

func NewDomainSpecificSnapshotControllerReconciler(
	c client.Client,
	scheme *runtime.Scheme,
	log logger.LoggerInterface,
	cfg *config.Options,
) (*DomainSpecificSnapshotControllerReconciler, error) {
	return dsc.NewDomainSpecificSnapshotControllerReconciler(c, scheme, log, cfg)
}

func AddSnapshotControllerToManager(mgr ctrl.Manager, cfg *config.Options, snapshotGraphRegistry snapshotgraphregistry.LiveReader) error {
	return snapshotcontroller.AddSnapshotControllerToManager(mgr, cfg, snapshotGraphRegistry)
}

func AddDemoVirtualDiskSnapshotControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
	return demo.AddDemoVirtualDiskSnapshotControllerToManager(mgr, cfg)
}

func AddDemoVirtualMachineSnapshotControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
	return demo.AddDemoVirtualMachineSnapshotControllerToManager(mgr, cfg)
}

// AddManifestCheckpointControllerToManager adds the ManifestCheckpoint controller to the manager
// and starts TTL scanner as a leader-only runnable.
//
// TTL scanner runs only on the leader replica to prevent duplicate deletion attempts.
// When leadership changes, the scanner context is cancelled and scanner stops gracefully.
func AddManifestCheckpointControllerToManager(
	mgr ctrl.Manager,
	log logger.LoggerInterface,
	cfg *config.Options,
) error {
	reconciler, err := manifestcapture.NewManifestCheckpointController(
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
	// StartTTLScanner runs TTL scanner and blocks until ctx.Done()
	// RunnableFunc ensures leader-only execution
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		reconciler.StartTTLScanner(ctx, mgr.GetClient())
		return nil
	})); err != nil {
		return err
	}
	return nil
}

// AddDomainSpecificSnapshotControllerToManager registers the DSC reconciler (registry/status).
// unifiedRuntimeSync runs after each successful full DSC reconcile in production (additive unified watches).
// graphRegistryRefresh runs after reconcileAll and before unifiedRuntimeSync so generic
// graph code picks up new eligible DSC pairs without restarting the pod.
func AddDomainSpecificSnapshotControllerToManager(
	mgr ctrl.Manager,
	log logger.LoggerInterface,
	cfg *config.Options,
	unifiedRuntimeSync func(context.Context) error,
	graphRegistryRefresh func(context.Context) error,
) error {
	rec, err := dsc.NewDomainSpecificSnapshotControllerReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		log,
		cfg,
	)
	if err != nil {
		return err
	}
	rec.UnifiedRuntimeSync = unifiedRuntimeSync
	rec.GraphRegistryRefresh = graphRegistryRefresh
	return rec.SetupWithManager(mgr)
}
