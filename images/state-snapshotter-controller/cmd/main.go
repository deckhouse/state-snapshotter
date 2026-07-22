/*
Copyright 2024 Flant JSC

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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	goruntime "runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	v1 "k8s.io/api/core/v1"
	sv1 "k8s.io/api/storage/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	v1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/api"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumesnapshotimport"
	deckhousev1alpha1 "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/deckhouseio/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/csdregistry"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/kubutils"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedruntime"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

var (
	resourcesSchemeFuncs = []func(*runtime.Scheme) error{
		v1alpha1.AddToScheme,          // state-snapshotter.deckhouse.io group
		storagev1alpha1.AddToScheme,   // state-snapshotter.deckhouse.io (Snapshot, SnapshotContent, ...)
		deckhousev1alpha1.AddToScheme, // deckhouse.io group (ObjectKeeper)
		clientgoscheme.AddToScheme,
		extv1.AddToScheme,
		v1.AddToScheme,
		sv1.AddToScheme,
	}

	// API server flags
	apiAddr        string
	apiTLSCertFile string
	apiTLSKeyFile  string

	// version is the human-readable build marker, injected at build time via
	// -ldflags "-X main.version=...". It defaults to "dev" for local `go run` and is set by the dev
	// image build (Makefile fox_build_and_push -> Dockerfile APP_VERSION) to git sha + dirty + timestamp,
	// so the startup log unambiguously identifies which build is running (debug.ReadBuildInfo VCS data is
	// empty in the docker build because .git is not in the build context).
	version = "dev"
)

func init() {
	flag.StringVar(&apiAddr, "api-addr", ":8443", "Address for API server to listen on")
	flag.StringVar(&apiTLSCertFile, "api-tls-cert-file", "", "Path to TLS certificate file for API server")
	flag.StringVar(&apiTLSKeyFile, "api-tls-private-key-file", "", "Path to TLS private key file for API server")
}

// buildTimeFromVersion extracts the UTC build timestamp embedded as the trailing
// "<sha>[-dirty]-YYYYMMDDTHHMMSSZ" segment of the version marker (see Makefile build_ts).
// Returns ok=false for versions without a timestamp suffix, e.g. the "dev" default.
func buildTimeFromVersion(v string) (time.Time, bool) {
	idx := strings.LastIndex(v, "-")
	if idx < 0 || idx+1 >= len(v) {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102T150405Z", v[idx+1:])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func main() {
	flag.Parse()

	// Print the build version unconditionally, before the logger exists and independent of the configurable
	// logrus level (LOG_LEVEL defaults to warn=3 in production, which would suppress an Info-level version
	// log). This guarantees the running build is always identifiable in `kubectl logs`.
	fmt.Printf("[main] Version: %s\n", version)
	if buildTime, ok := buildTimeFromVersion(version); ok {
		fmt.Printf("[main] Build time: %s UTC\n", buildTime.Format("2006-01-02 15:04:05"))
	}

	// Enable controller-runtime logs FIRST, before any manager/recorder creation
	// This prevents the warning: "[controller-runtime] log.SetLogger(...) was never called; logs will not be displayed"
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	ctx, cancel := context.WithCancel(context.Background())

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("Received shutdown signal")
		cancel()
	}()

	cfgParams := config.NewConfig()

	log, err := logger.NewLogger(string(cfgParams.Loglevel))
	if err != nil {
		fmt.Printf("unable to create NewLogger, err: %v\n", err)
		cancel() // Ensure cleanup before exit
		os.Exit(1)
	}

	log.Info(fmt.Sprintf("[main] Go Version:%s ", goruntime.Version()))
	log.Info(fmt.Sprintf("[main] OS/Arch:Go OS/Arch:%s/%s ", goruntime.GOOS, goruntime.GOARCH))
	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		log.Info(fmt.Sprintf("[main] BuildInfo: module=%s version=%s", buildInfo.Main.Path, buildInfo.Main.Version))
		var vcsRevision, vcsTime, vcsModified string
		for _, setting := range buildInfo.Settings {
			switch setting.Key {
			case "vcs.revision":
				vcsRevision = setting.Value
			case "vcs.time":
				vcsTime = setting.Value
			case "vcs.modified":
				vcsModified = setting.Value
			}
		}
		if vcsRevision != "" || vcsTime != "" || vcsModified != "" {
			log.Info(fmt.Sprintf("[main] VCS: revision=%s time=%s modified=%s", vcsRevision, vcsTime, vcsModified))
		}
	}

	log.Info("[main] CfgParams has been successfully created")
	log.Info(fmt.Sprintf("[main] %s = %s", config.LogLevelEnvName, cfgParams.Loglevel))
	log.Info(fmt.Sprintf("[main] RequeueStorageClassInterval = %d", cfgParams.RequeueStorageClassInterval))

	kConfig, err := kubutils.KubernetesDefaultConfigCreate()
	if err != nil {
		log.Error(err, "[main] unable to KubernetesDefaultConfigCreate")
		cancel() // Ensure cleanup before exit
		os.Exit(1)
	}
	log.Info("[main] kubernetes config has been successfully created.")

	// Raise the client-go rate limiter above the conservative library defaults (QPS=5 / Burst=10).
	// Those defaults serialize uncached reads and status patches on the shared manager client and
	// become the dominant bottleneck under high controller concurrency and large snapshot fan-out —
	// independent of MaxConcurrentReconciles. 200/400 is the measured saturation knee (500/1000 only
	// adds apiserver pressure for no gain).
	const (
		kubeClientQPS   = 200
		kubeClientBurst = 400
	)
	kConfig.QPS = kubeClientQPS
	kConfig.Burst = kubeClientBurst
	log.Info("[main] kubernetes client rate limiter configured",
		"qps", kConfig.QPS,
		"burst", kConfig.Burst,
	)

	// Create scheme for controller manager (includes all CRD types for informers)
	scheme := runtime.NewScheme()
	for _, f := range resourcesSchemeFuncs {
		err := f(scheme)
		if err != nil {
			log.Error(err, "[main] unable to add scheme to func")
			cancel() // Ensure cleanup before exit
			os.Exit(1)
		}
	}
	log.Info("[main] successfully read scheme CR")

	// Create full scheme for API direct client (no informers)
	fullScheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(fullScheme)
	_ = v1alpha1.AddToScheme(fullScheme)          // state-snapshotter.deckhouse.io group (MCP, chunks, …)
	_ = storagev1alpha1.AddToScheme(fullScheme)   // state-snapshotter.deckhouse.io (Snapshot, SnapshotContent)
	_ = deckhousev1alpha1.AddToScheme(fullScheme) // deckhouse.io group (ObjectKeeper)

	// Create controller manager with full scheme (for informers)
	// Don't restrict cache to specific namespace - ManifestCaptureRequest can be in any namespace
	// Cluster-scoped resources (ManifestCheckpoint, Retainer) are always watched
	managerOpts := manager.Options{
		Scheme: scheme,
		//MetricsBindAddress: cfgParams.MetricsPort,
		HealthProbeBindAddress:  cfgParams.HealthProbeBindAddress,
		LeaderElection:          true,
		LeaderElectionNamespace: cfgParams.ControllerNamespace,
		LeaderElectionID:        config.ControllerName,
		// Logger removed - manager will use default logger
	}

	mgr, err := manager.New(kConfig, managerOpts)
	if err != nil {
		log.Error(err, "[main] unable to manager.New")
		cancel() // Ensure cleanup before exit
		os.Exit(1)
	}
	log.Info("[main] successfully created kubernetes manager")

	graphRegProvider, err := snapshotgraphregistry.NewProvider(cfgParams, mgr.GetRESTMapper(), mgr.GetAPIReader(), ctrl.Log.WithName("snapshot-graph-registry"))
	if err != nil {
		log.Error(err, "[main] snapshot graph registry provider")
		cancel()
		os.Exit(1)
	}
	if err := graphRegProvider.Refresh(ctx); err != nil {
		log.Warning("initial snapshot graph registry refresh failed (generic graph may be empty until CSD reconcile)", "error", err)
	}

	// Add controllers
	if err := controllers.AddManifestCheckpointControllerToManager(mgr, log, cfgParams); err != nil {
		log.Error(err, "Failed to add ManifestCheckpointController to manager")
		cancel() // Ensure cleanup before exit
		os.Exit(1)
	}
	log.Info("ManifestCheckpointController added to manager")

	if err := controllers.AddSnapshotControllerToManager(mgr, cfgParams, graphRegProvider); err != nil {
		log.Error(err, "Failed to add NamespaceGenericSnapshotBinderController to manager")
		cancel()
		os.Exit(1)
	}
	log.Info("NamespaceGenericSnapshotBinderController added to manager")

	// Out-of-process domain controllers (the PoC demo domain, and real domains such as virtualization) run
	// in a separate pod/binary, not in core. Core carries no domain-specific kind names: it discovers such
	// kinds purely from their CustomSnapshotDefinition, owns their SnapshotContent (see the unified runtime
	// Syncer wiring below), and delegates their restore subtrees to the domain aggregated apiserver by
	// CSD-origin (see the isOutOfProcessDomainKind predicate passed to api.NewServer).

	contentController, err := controllers.NewSnapshotContentController(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		mgr.GetScheme(),
		mgr.GetRESTMapper(),
		cfgParams,
		[]schema.GroupVersionKind{unifiedbootstrap.CommonSnapshotContentGVK()},
	)
	if err != nil {
		log.Error(err, "Failed to create SnapshotContentController")
		cancel()
		os.Exit(1)
	}
	if err := contentController.SetupWithManager(mgr); err != nil {
		log.Error(err, "Failed to setup SnapshotContentController with manager")
		cancel()
		os.Exit(1)
	}
	log.Info("SnapshotContentController added to manager", "snapshotContentGVKs", 1)

	// Unified runtime is always on in v0; the static bootstrap list is the hardcoded built-in default
	// (unifiedbootstrap.DefaultGraphRegistryBuiltInPairs) — domain kinds come only from eligible CSD.
	csdBootstrapClient, err := client.New(kConfig, client.Options{
		Scheme: scheme,
		Mapper: mgr.GetRESTMapper(),
	})
	if err != nil {
		log.Error(err, "[main] unable to create client for CSD→unified GVK bootstrap")
		cancel()
		os.Exit(1)
	}
	csdPairs, err := csdregistry.EligibleUnifiedGVKPairs(ctx, csdBootstrapClient)
	if err != nil {
		log.Warning("CSD list/parse for unified GVK bootstrap failed; using bootstrap-only merge", "error", err)
		csdPairs = nil
	} else {
		log.Info("[main] CSD-derived unified GVK pairs (eligible by conditions; before RESTMapper / CRD presence filter)", "count", len(csdPairs))
	}
	bootstrapPairs := unifiedbootstrap.DefaultGraphRegistryBuiltInPairs()
	log.Info("[main] unified static bootstrap", "pairCount", len(bootstrapPairs))
	mergedPairs := unifiedbootstrap.MergeBootstrapAndCSDPairs(bootstrapPairs, csdPairs)
	log.Info("[main] unified GVK pairs after merge (bootstrap + CSD)", "count", len(mergedPairs))
	snapshotGVKs, snapshotContentGVKs := unifiedbootstrap.ResolveAvailableUnifiedGVKPairs(
		mgr.GetRESTMapper(),
		mergedPairs,
		ctrl.Log.WithName("unified-bootstrap"),
	)
	if len(snapshotGVKs) == 0 {
		log.Info("[main] no unified snapshot CRDs found in API; unified snapshot controllers run with zero watches (manifest/MCR and other controllers continue)")
	} else {
		log.Info("[main] unified snapshot GVKs after API discovery filter", "count", len(snapshotGVKs))
	}

	genericSnapshotGVKs, genericContentGVKs := unifiedbootstrap.FilterGenericSnapshotGVKPairs(snapshotGVKs, snapshotContentGVKs)
	for _, snapshotGVK := range snapshotGVKs {
		if err := contentController.AddSnapshotStatusWatch(mgr, snapshotGVK); err != nil {
			log.Error(err, "Failed to setup SnapshotContentController snapshot status watch", "snapshotGVK", snapshotGVK.String())
			cancel()
			os.Exit(1)
		}
	}

	snapshotController, err := controllers.NewGenericSnapshotBinderController(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		mgr.GetScheme(),
		cfgParams,
		nil,
	)
	if err != nil {
		log.Error(err, "Failed to create GenericSnapshotBinderController")
		cancel()
		os.Exit(1)
	}
	// Carry CSD spec.requiresDataArtifact from the merged pairs onto BOTH controllers' GVK registries
	// (they hold separate instances): the binder's import path and main's capture-leg eager-init
	// (main-owned commonController, decision #10) read the same flag. Built-in/bootstrap pairs stay false.
	anyDataArtifactKind := false
	for _, p := range mergedPairs {
		snapshotController.MarkRequiresDataArtifact(p.Snapshot.Kind, p.RequiresDataArtifact)
		contentController.MarkRequiresDataArtifact(p.Snapshot.Kind, p.RequiresDataArtifact)
		if p.RequiresDataArtifact {
			anyDataArtifactKind = true
		}
	}
	// Event-driven VCR (vcr-watch-core-terminal Phase 3): a registered data-artifact kind means the forked
	// storage-foundation VolumeCaptureRequest CRD is deployed and RESTMappable now, so add the VCR watch to
	// the existing content controller (single content queue). This replaces the data-leg 500 ms poll: a VCR
	// status flip enqueues the owning SnapshotContent directly. Idempotent + RESTMapping-guarded internally.
	if anyDataArtifactKind {
		if err := contentController.AddVolumeCaptureRequestWatch(mgr); err != nil {
			log.Error(err, "Failed to add VolumeCaptureRequest watch to SnapshotContentController")
			cancel()
			os.Exit(1)
		}
	}
	for i := range genericSnapshotGVKs {
		if err := snapshotController.AddWatchForPair(mgr, genericSnapshotGVKs[i], genericContentGVKs[i]); err != nil {
			log.Error(err, "Failed to setup GenericSnapshotBinderController watch", "snapshotGVK", genericSnapshotGVKs[i].String(), "snapshotContentGVK", genericContentGVKs[i].String())
			cancel()
			os.Exit(1)
		}
	}
	// wave7 (w7-creator): additionally register the built-in root Snapshot pair on the binder at boot.
	// FilterGenericSnapshotGVKPairs strips the root (a dedicated kind) but since wave5 the binder is the
	// creator/owner of the root SnapshotContent, and the compensating unifiedSync.Sync only runs on CSD
	// reconciles — so without this the binder never watches the root at startup and root content is never
	// created. See unifiedbootstrap.StartupDomainCaptureRootPair. Idempotent w.r.t. a later Sync.
	if rootSnapGVK, rootContentGVK, ok := unifiedbootstrap.StartupDomainCaptureRootPair(snapshotGVKs, snapshotContentGVKs); ok {
		snapshotController.MarkDomainCaptureKind(rootSnapGVK)
		// Main runs the root's capture-leg lifecycle (latches + MCR reap, decision #10).
		contentController.MarkDomainCaptureKind(rootSnapGVK)
		if err := snapshotController.AddWatchForPair(mgr, rootSnapGVK, rootContentGVK); err != nil {
			log.Error(err, "Failed to setup GenericSnapshotBinderController root Snapshot watch", "snapshotGVK", rootSnapGVK.String(), "snapshotContentGVK", rootContentGVK.String())
			cancel()
			os.Exit(1)
		}
		log.Info("GenericSnapshotBinderController watching built-in root Snapshot at startup (w7-creator)", "snapshotGVK", rootSnapGVK.String())
	}
	// Built-in CSI VolumeSnapshot: mark it domain-capture at boot. Unlike the root it is NOT a dedicated
	// kind, so FilterGenericSnapshotGVKPairs kept it and the loop above already added its watch — only the
	// domain-capture MARK is missing. Without it the binder would eagerly create + bind a SnapshotContent
	// shell before the out-of-process storage-foundation VolumeSnapshot domain controller claims the object
	// (dual content writer). The compensating unifiedSync.Sync mark runs only on CSD reconciles (never at
	// boot, and not at all with zero CSDs), so wire it here; a later Sync re-asserts it idempotently. See
	// unifiedbootstrap.StartupBuiltInVolumeSnapshotPair.
	if vsSnapGVK, _, ok := unifiedbootstrap.StartupBuiltInVolumeSnapshotPair(snapshotGVKs, snapshotContentGVKs); ok {
		snapshotController.MarkDomainCaptureKind(vsSnapGVK)
		contentController.MarkDomainCaptureKind(vsSnapGVK)
		log.Info("GenericSnapshotBinderController marked built-in VolumeSnapshot domain-capture at startup", "snapshotGVK", vsSnapGVK.String())
	}
	log.Info("GenericSnapshotBinderController added to manager", "snapshotGVKs", len(genericSnapshotGVKs))

	// Import binder for extended generic-PVC VolumeSnapshots (spec.mode: Import; owning DataImport
	// found by reverse-lookup of DataImport.spec.targetRef).
	// The forked snapshot-controller skips these; this common controller materializes their SnapshotContent
	// and writes the binding (extended boundSnapshotContentName + legacy boundVolumeSnapshotContentName/readyToUse).
	// Self-guards by RESTMapping: a not-yet-installed VolumeSnapshot CRD degrades to "no controller".
	if err := volumesnapshotimport.AddToManager(mgr); err != nil {
		log.Error(err, "Failed to add VolumeSnapshot import binder to manager")
		cancel()
		os.Exit(1)
	}
	log.Info("VolumeSnapshot import binder added to manager")

	// No dedicated controller activators in core: the only in-process dedicated kind is the namespace-root
	// "Snapshot", whose controller is registered statically at startup (AddSnapshotControllerToManager
	// above), so nothing needs runtime activation. Out-of-process domain kinds (CSD-driven modules, e.g.
	// the PoC demo domain) have no in-process planning controller at all; the Syncer drives the generic
	// Snapshot/SnapshotContent watches AND the generic binder's ownership of their domain-capture
	// SnapshotContent (the binder's unstructured informer needs no field index, so there is no
	// informer-ordering hazard). See unifiedruntime.Syncer.Sync.
	unifiedSync := unifiedruntime.NewSyncer(
		mgr,
		ctrl.Log,
		unifiedbootstrap.DefaultGraphRegistryBuiltInPairs(),
		mgr.GetAPIReader(),
		snapshotController,
		contentController,
		nil,
	)

	if err := controllers.AddCustomSnapshotDefinitionControllerToManager(mgr, log, cfgParams, unifiedSync.Sync, graphRegProvider.Refresh); err != nil {
		log.Error(err, "Failed to add CustomSnapshotDefinition reconciler to manager")
		cancel()
		os.Exit(1)
	}
	log.Info("CustomSnapshotDefinition reconciler added to manager")

	// NOTE: RetainerController (IRetainer) has been removed.
	// ObjectKeeper is now used instead, which is managed by deckhouse-controller.

	// Add health checks
	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "[main] unable to mgr.AddHealthzCheck")
		cancel() // Ensure cleanup before exit
		os.Exit(1)
	}
	log.Info("[main] successfully AddHealthzCheck")

	if err = mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "[main] unable to mgr.AddReadyzCheck")
		cancel() // Ensure cleanup before exit
		os.Exit(1)
	}
	log.Info("[main] successfully AddReadyzCheck")

	// Create direct client for API server (without informer cache)
	// API server doesn't need informers - it only does direct GET requests
	// This avoids requiring list/watch permissions on any CRD resources
	httpClient, err := rest.HTTPClientFor(kConfig)
	if err != nil {
		log.Error(err, "[main] unable to create HTTP client for API server")
		cancel() // Ensure cleanup before exit
		os.Exit(1)
	}
	mapper, err := apiutil.NewDynamicRESTMapper(kConfig, httpClient)
	if err != nil {
		log.Error(err, "[main] unable to create REST mapper for API server")
		cancel() // Ensure cleanup before exit
		os.Exit(1)
	}
	directClient, err := client.New(kConfig, client.Options{
		Scheme: fullScheme,
		Mapper: mapper,
	})
	if err != nil {
		log.Error(err, "[main] unable to create direct client for API server")
		cancel() // Ensure cleanup before exit
		os.Exit(1)
	}

	// The aggregated apiserver's authentication (front-proxy requestheader + TokenReview) and
	// authorization (SubjectAccessReview) are delegated to genericapiserver: it reads the front-proxy CA
	// + allowed-names from the extension-apiserver-authentication ConfigMap itself (via the
	// extension-apiserver-authentication-reader Role) and validates the kube-apiserver proxy client cert.
	// No manual ConfigMap read or CN allowlist is needed here anymore.

	// Domain restore delegation: core orchestrates restore over the run tree and, on a domain snapshot
	// subtree, calls the domain controller's aggregated apiserver
	// (manifests-with-data-restoration) through the kube-apiserver aggregation layer (SA token). Until
	// the domain APIService is registered (deploy commit), this path simply errors for domain subtrees;
	// pure-generic restores are unaffected.
	domainRestoreClient, err := api.NewDomainRestoreClient(kConfig, mapper, log)
	if err != nil {
		log.Error(err, "[main] unable to create domain restore delegation client")
		cancel()
		os.Exit(1)
	}

	// The restore delegate predicate is CSD-origin driven: a snapshot kind that is registered in the live
	// graph registry but is NOT a built-in in-process pair (root "Snapshot" / CSI "VolumeSnapshot") can only
	// have come from an eligible CustomSnapshotDefinition — i.e. an out-of-process domain that serves its own
	// restore apiserver, so the compiler delegates it. Built-in kinds restore in-process (delegating the root
	// back to core's own endpoint would be self-recursion → HTTP 500). graphRegProvider.Current() is live
	// (refreshed on CSD changes), so this needs no hardcoded domain kind names.
	isOutOfProcessDomainKind := func(kind string) bool {
		return unifiedbootstrap.IsOutOfProcessDomainSnapshotKind(graphRegProvider.Current(), kind)
	}
	apiServer := api.NewServer(apiAddr, directClient, directClient, log, graphRegProvider, domainRestoreClient, isOutOfProcessDomainKind, apiTLSCertFile, apiTLSKeyFile)

	log.Info("Starting state-snapshotter-controller", "api-addr", apiAddr)

	// Start controller manager in background
	go func() {
		if err := mgr.Start(ctx); err != nil {
			log.Error(err, "[main] unable to mgr.Start")
			cancel() // Ensure cleanup before exit
			os.Exit(1)
		}
	}()

	// Start API server (blocks until context cancellation)
	if err := apiServer.Start(ctx); err != nil {
		log.Error(err, "[main] Failed to start API server")
		cancel() // Ensure cleanup before exit
		os.Exit(1)
	}
}
