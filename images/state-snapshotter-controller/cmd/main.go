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
	"crypto/x509"
	"encoding/pem"
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
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	v1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/api"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/dscregistry"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/kubutils"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedruntime"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

var (
	resourcesSchemeFuncs = []func(*apiruntime.Scheme) error{
		v1alpha1.AddToScheme,          // state-snapshotter.deckhouse.io group
		deckhousev1alpha1.AddToScheme, // deckhouse.io group (ObjectKeeper)
		clientgoscheme.AddToScheme,
		extv1.AddToScheme,
		v1.AddToScheme,
		sv1.AddToScheme,
	}

	// API server flags
	apiAddr             string
	apiTLSCertFile      string
	apiTLSKeyFile       string
	apiAllowedClientCNs string
)

func init() {
	flag.StringVar(&apiAddr, "api-addr", ":8443", "Address for API server to listen on")
	flag.StringVar(&apiTLSCertFile, "api-tls-cert-file", "", "Path to TLS certificate file for API server")
	flag.StringVar(&apiTLSKeyFile, "api-tls-private-key-file", "", "Path to TLS private key file for API server")
	flag.StringVar(&apiAllowedClientCNs, "api-allowed-client-cns", "system:kube-apiserver,kubernetes,front-proxy-client", "Comma-separated list of allowed client certificate CNs for mTLS")
}

func main() {
	flag.Parse()

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
	_ = v1alpha1.AddToScheme(fullScheme)          // state-snapshotter.deckhouse.io group
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

	// Add controllers
	if err := controllers.AddManifestCheckpointControllerToManager(mgr, log, cfgParams); err != nil {
		log.Error(err, "Failed to add ManifestCheckpointController to manager")
		cancel() // Ensure cleanup before exit
		os.Exit(1)
	}
	log.Info("ManifestCheckpointController added to manager")

	// Unified snapshots: optional rollout (STATE_SNAPSHOTTER_UNIFIED_ENABLED); bootstrap list from R5 env or defaults.
	var unifiedSyncFn func(context.Context) error

	if cfgParams.UnifiedSnapshotDisabled {
		log.Info("[main] unified snapshot wiring disabled (STATE_SNAPSHOTTER_UNIFIED_ENABLED); skipping Snapshot/SnapshotContent and runtime sync; DSC reconciler runs without sync")
		unifiedSyncFn = nil
	} else {
		dscBootstrapClient, err := client.New(kConfig, client.Options{
			Scheme: scheme,
			Mapper: mgr.GetRESTMapper(),
		})
		if err != nil {
			log.Error(err, "[main] unable to create client for DSC→unified GVK bootstrap")
			cancel()
			os.Exit(1)
		}
		dscPairs, err := dscregistry.EligibleUnifiedGVKPairs(ctx, dscBootstrapClient)
		if err != nil {
			log.Warning("DSC list/parse for unified GVK bootstrap failed; using bootstrap-only merge", "error", err)
			dscPairs = nil
		} else {
			log.Info("[main] DSC-derived unified GVK pairs (eligible by conditions; before RESTMapper / CRD presence filter)", "count", len(dscPairs))
		}
		bootstrapPairs := cfgParams.EffectiveUnifiedBootstrapPairs()
		log.Info("[main] unified static bootstrap", "pairCount", len(bootstrapPairs), "bootstrapMode", cfgParams.UnifiedBootstrapMode)
		mergedPairs := unifiedbootstrap.MergeBootstrapAndDSCPairs(bootstrapPairs, dscPairs)
		log.Info("[main] unified GVK pairs after merge (bootstrap + DSC)", "count", len(mergedPairs))
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

		snapshotController, err := controllers.NewSnapshotController(
			mgr.GetClient(),
			mgr.GetAPIReader(),
			mgr.GetScheme(),
			cfgParams,
			snapshotGVKs,
		)
		if err != nil {
			log.Error(err, "Failed to create SnapshotController")
			cancel()
			os.Exit(1)
		}
		if err := snapshotController.SetupWithManager(mgr); err != nil {
			log.Error(err, "Failed to setup SnapshotController with manager")
			cancel()
			os.Exit(1)
		}
		log.Info("SnapshotController added to manager", "snapshotGVKs", len(snapshotGVKs))

		contentController, err := controllers.NewSnapshotContentController(
			mgr.GetClient(),
			mgr.GetAPIReader(),
			mgr.GetScheme(),
			mgr.GetRESTMapper(),
			cfgParams,
			snapshotContentGVKs,
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
		log.Info("SnapshotContentController added to manager", "snapshotContentGVKs", len(snapshotContentGVKs))

		unifiedSync := unifiedruntime.NewSyncer(
			mgr,
			ctrl.Log,
			cfgParams.EffectiveUnifiedBootstrapPairs(),
			mgr.GetAPIReader(),
			snapshotController,
			contentController,
		)
		unifiedSyncFn = unifiedSync.Sync
	}

	if err := controllers.AddDomainSpecificSnapshotControllerToManager(mgr, log, cfgParams, unifiedSyncFn); err != nil {
		log.Error(err, "Failed to add DomainSpecificSnapshotController reconciler to manager")
		cancel()
		os.Exit(1)
	}
	log.Info("DomainSpecificSnapshotController reconciler added to manager")

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

	// Read front-proxy CA from extension-apiserver-authentication ConfigMap
	// This CA is used to verify client certificates from kube-apiserver
	// mTLS is mandatory - server will not start if CA cannot be loaded
	var mTLSCACert []byte
	cm := &v1.ConfigMap{}
	err = directClient.Get(ctx, client.ObjectKey{
		Namespace: "kube-system",
		Name:      "extension-apiserver-authentication",
	}, cm)
	if err != nil {
		log.Error(err, "[main] Failed to read extension-apiserver-authentication ConfigMap, mTLS is required - server cannot start")
		cancel() // Ensure cleanup before exit
		os.Exit(1)
	}

	caData, ok := cm.Data["requestheader-client-ca-file"]
	if !ok || caData == "" {
		log.Error(nil, "[main] requestheader-client-ca-file not found in ConfigMap, mTLS is required - server cannot start")
		cancel() // Ensure cleanup before exit
		os.Exit(1)
	}

	mTLSCACert = []byte(caData)

	// Parse and log CA certificate details for debugging
	block, _ := pem.Decode(mTLSCACert)
	if block != nil && block.Type == "CERTIFICATE" {
		caCert, err := x509.ParseCertificate(block.Bytes)
		if err == nil {
			log.Info("[main] Successfully loaded front-proxy CA from extension-apiserver-authentication ConfigMap",
				"ca_subject", caCert.Subject.String(),
				"ca_issuer", caCert.Issuer.String(),
				"ca_serial", caCert.SerialNumber.String(),
				"ca_not_before", caCert.NotBefore.Format(time.RFC3339),
				"ca_not_after", caCert.NotAfter.Format(time.RFC3339),
				"ca_is_ca", caCert.IsCA,
				"ca_key_usage", fmt.Sprintf("%d", caCert.KeyUsage),
			)
		} else {
			log.Info("[main] Successfully loaded front-proxy CA (failed to parse for logging)", "error", err)
		}
	} else {
		log.Info("[main] Successfully loaded front-proxy CA (failed to decode PEM for logging)")
	}

	// Create API server with direct client (no informer cache)
	// ArchiveService will use directClient for all CRD operations
	// Parse allowed client CNs
	allowedCNsList := []string{}
	if apiAllowedClientCNs != "" {
		allowedCNs := strings.Split(apiAllowedClientCNs, ",")
		for _, cn := range allowedCNs {
			cn = strings.TrimSpace(cn)
			if cn != "" {
				allowedCNsList = append(allowedCNsList, cn)
			}
		}
	}

	apiServer := api.NewServer(apiAddr, directClient, directClient, log, apiTLSCertFile, apiTLSKeyFile, mTLSCACert, allowedCNsList)
	if apiServer == nil {
		log.Error(nil, "[main] Failed to create API server (mTLS configuration failed)")
		cancel() // Ensure cleanup before exit
		os.Exit(1)
	}

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
