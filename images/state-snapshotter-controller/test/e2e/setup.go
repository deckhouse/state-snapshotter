//go:build e2e
// +build e2e

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

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       context.Context
	cancel    context.CancelFunc
	mgr       ctrl.Manager
)

var _ = BeforeSuite(func() {
	By("bootstrapping test environment")
	// Use zap logger for controller-runtime
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	// Calculate path to CRDs relative to test directory
	// test/e2e -> images/state-snapshotter-controller -> state-snapshotter -> crds
	// Try multiple possible paths
	var crdsPath string
	possiblePaths := []string{
		filepath.Join("..", "..", "..", "..", "crds"),       // From test/e2e
		filepath.Join("..", "..", "..", "crds"),             // Alternative
		filepath.Join("..", "..", "..", "..", "..", "crds"), // From module root
	}

	for _, p := range possiblePaths {
		if _, err := os.Stat(p); err == nil {
			crdsPath = p
			break
		}
	}

	if crdsPath == "" {
		// If CRDs not found, try to get absolute path from module root
		// This is a fallback - in production CRDs should be in the expected location
		crdsPath = filepath.Join("..", "..", "..", "..", "crds")
		GinkgoWriter.Printf("Warning: CRDs path not verified, using: %s\n", crdsPath)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			crdsPath,
		},
		ErrorIfCRDPathMissing: false, // Will be set to true once CRDs are verified
		// BinaryAssetsDirectory can be set via KUBEBUILDER_ASSETS env var
		// or will be downloaded automatically by envtest
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	// Create scheme
	scheme := runtime.NewScheme()
	err = clientgoscheme.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	err = storagev1alpha1.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	err = deckhousev1alpha1.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	err = apiextensionsv1.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	// Create manager
	mgr, err = ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		// Disable health probes for tests
		HealthProbeBindAddress: "0",
		// Disable leader election for tests
		LeaderElection: false,
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(mgr).NotTo(BeNil())

	// Create test logger
	testLogger, err := logger.NewLogger("error")
	Expect(err).NotTo(HaveOccurred())

	// Setup config
	cfgOptions := &config.Options{
		EnableFiltering: false, // Disable filtering for tests to capture all objects
		DefaultTTL:      168 * time.Hour,
	}

	// Setup ManifestCheckpointController
	mcpController := &controllers.ManifestCheckpointController{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Logger: testLogger,
		Config: cfgOptions,
	}
	err = mcpController.SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	// NOTE: RetainerController (IRetainer) has been removed.
	// ObjectKeeper is now used instead, which is managed by deckhouse-controller.

	// Create context
	ctx, cancel = context.WithCancel(context.Background())

	// Start manager in goroutine
	go func() {
		defer GinkgoRecover()
		err := mgr.Start(ctx)
		Expect(err).NotTo(HaveOccurred())
	}()

	// Get client
	k8sClient = mgr.GetClient()
	Expect(k8sClient).NotTo(BeNil())

	// Wait for cache to sync
	Eventually(func() bool {
		return mgr.GetCache().WaitForCacheSync(ctx)
	}).Should(BeTrue())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})
