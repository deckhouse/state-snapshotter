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

// Command domain-controller is the demo domain controller binary. It runs the demo dedicated reconcilers
// in its own manager: the SNAPSHOT reconcilers (MCR/VCR + child snapshots + snapshot.status) are
// content-free and never touch SnapshotContent, while the RESOURCE reconcilers (DemoVirtualDisk /
// DemoVirtualMachine materialization) read SnapshotContent.status.dataRef read-only via an uncached
// APIReader to drive restore. It also hosts its own aggregated API server for the demo snapshot kinds'
// restore subresources. The aggregated restore path fetches BASE manifests from the core aggregated API
// server (via the kube-apiserver aggregation layer, SA token) and applies the demo restore mutation
// in-process — it never reads SnapshotContent/ManifestCheckpoint. No generic controllers run here.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	goruntime "runtime"
	"runtime/debug"
	"syscall"

	v1 "k8s.io/api/core/v1"
	sv1 "k8s.io/api/storage/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	v1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/domain-controller/internal/controllers/demo"
	"github.com/deckhouse/state-snapshotter/images/domain-controller/internal/domainapi"
	"github.com/deckhouse/state-snapshotter/images/domain-controller/internal/logger"
	"github.com/deckhouse/state-snapshotter/images/domain-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/domain-controller/pkg/kubutils"
)

// leaderElectionID is distinct from the core controller's lease so the two binaries never contend for
// the same leadership lock.
const leaderElectionID = "domain-controller"

var resourcesSchemeFuncs = []func(*runtime.Scheme) error{
	v1alpha1.AddToScheme,          // state-snapshotter.deckhouse.io group
	storagev1alpha1.AddToScheme,   // storage.deckhouse.io (Snapshot, SnapshotContent, VCR/VRR, ...)
	demov1alpha1.AddToScheme,      // demo.state-snapshotter.deckhouse.io (demo domain)
	deckhousev1alpha1.AddToScheme, // deckhouse.io group (ObjectKeeper)
	clientgoscheme.AddToScheme,
	extv1.AddToScheme,
	v1.AddToScheme,
	sv1.AddToScheme,
}

var (
	apiAddr        string
	apiTLSCertFile string
	apiTLSKeyFile  string
)

func init() {
	flag.StringVar(&apiAddr, "api-addr", ":8443", "Address for the domain API server to listen on")
	flag.StringVar(&apiTLSCertFile, "api-tls-cert-file", "", "Path to TLS certificate file for the domain API server")
	flag.StringVar(&apiTLSKeyFile, "api-tls-private-key-file", "", "Path to TLS private key file for the domain API server")
}

func main() {
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	ctx, cancel := context.WithCancel(context.Background())

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
		cancel()
		os.Exit(1)
	}

	log.Info(fmt.Sprintf("[domain-main] Go Version:%s ", goruntime.Version()))
	log.Info(fmt.Sprintf("[domain-main] OS/Arch:%s/%s ", goruntime.GOOS, goruntime.GOARCH))
	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		log.Info(fmt.Sprintf("[domain-main] BuildInfo: module=%s version=%s", buildInfo.Main.Path, buildInfo.Main.Version))
	}
	log.Info(fmt.Sprintf("[domain-main] %s = %s", config.LogLevelEnvName, cfgParams.Loglevel))

	kConfig, err := kubutils.KubernetesDefaultConfigCreate()
	if err != nil {
		log.Error(err, "[domain-main] unable to KubernetesDefaultConfigCreate")
		cancel()
		os.Exit(1)
	}
	log.Info("[domain-main] kubernetes config has been successfully created.")

	scheme := runtime.NewScheme()
	for _, f := range resourcesSchemeFuncs {
		if err := f(scheme); err != nil {
			log.Error(err, "[domain-main] unable to add scheme to func")
			cancel()
			os.Exit(1)
		}
	}
	log.Info("[domain-main] successfully read scheme CR")

	managerOpts := manager.Options{
		Scheme:                  scheme,
		HealthProbeBindAddress:  cfgParams.HealthProbeBindAddress,
		LeaderElection:          true,
		LeaderElectionNamespace: cfgParams.ControllerNamespace,
		LeaderElectionID:        leaderElectionID,
	}

	mgr, err := manager.New(kConfig, managerOpts)
	if err != nil {
		log.Error(err, "[domain-main] unable to manager.New")
		cancel()
		os.Exit(1)
	}
	log.Info("[domain-main] successfully created kubernetes manager")

	// Register demo dedicated reconcilers: snapshot controllers (content-free) and resource controllers
	// (DemoVirtualDisk/DemoVirtualMachine materialization). The disk snapshot controller must be registered
	// before the VM snapshot controller (the VM snapshot controller watches the disk snapshot type). The
	// disk resource controller must be registered before the VM resource controller (the VM watches disks).
	if err := demo.AddDemoVirtualDiskSnapshotControllerToManager(mgr, cfgParams); err != nil {
		log.Error(err, "[domain-main] Failed to add DemoVirtualDiskSnapshot controller to manager")
		cancel()
		os.Exit(1)
	}
	log.Info("[domain-main] DemoVirtualDiskSnapshot controller added to manager")

	if err := demo.AddDemoVirtualMachineSnapshotControllerToManager(mgr, cfgParams); err != nil {
		log.Error(err, "[domain-main] Failed to add DemoVirtualMachineSnapshot controller to manager")
		cancel()
		os.Exit(1)
	}
	log.Info("[domain-main] DemoVirtualMachineSnapshot controller added to manager")

	if err := demo.AddDemoVirtualDiskControllerToManager(mgr, cfgParams); err != nil {
		log.Error(err, "[domain-main] Failed to add DemoVirtualDisk controller to manager")
		cancel()
		os.Exit(1)
	}
	log.Info("[domain-main] DemoVirtualDisk controller added to manager")

	if err := demo.AddDemoVirtualMachineControllerToManager(mgr, cfgParams); err != nil {
		log.Error(err, "[domain-main] Failed to add DemoVirtualMachine controller to manager")
		cancel()
		os.Exit(1)
	}
	log.Info("[domain-main] DemoVirtualMachine controller added to manager")

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "[domain-main] unable to mgr.AddHealthzCheck")
		cancel()
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "[domain-main] unable to mgr.AddReadyzCheck")
		cancel()
		os.Exit(1)
	}
	log.Info("[domain-main] successfully added health checks")

	// Domain aggregated API server. Restore fetches BASE manifests from core over the kube-apiserver
	// aggregation layer (SA token) and applies the demo mutation in-process; it never reads
	// SnapshotContent/ManifestCheckpoint.
	coreClient, err := domainapi.NewCoreManifestsClient(kConfig)
	if err != nil {
		log.Error(err, "[domain-main] unable to build core manifests client")
		cancel()
		os.Exit(1)
	}
	restoreSvc := domainapi.NewRestoreService(mgr.GetAPIReader(), coreClient, log)

	// Authentication (front-proxy requestheader + TokenReview) and authorization (SubjectAccessReview)
	// are delegated to genericapiserver, which reads the front-proxy CA + allowed-names from the
	// extension-apiserver-authentication ConfigMap itself (via the extension-apiserver-authentication-reader
	// Role). No manual front-proxy CA load or CN allowlist is needed here anymore.
	apiServer := domainapi.NewServer(apiAddr, restoreSvc, log, apiTLSCertFile, apiTLSKeyFile)

	log.Info("[domain-main] starting domain-controller", "api-addr", apiAddr)

	go func() {
		if err := mgr.Start(ctx); err != nil {
			log.Error(err, "[domain-main] unable to mgr.Start")
			cancel()
			os.Exit(1)
		}
	}()

	if err := apiServer.Start(ctx); err != nil {
		log.Error(err, "[domain-main] failed to start domain API server")
		cancel()
		os.Exit(1)
	}
}
