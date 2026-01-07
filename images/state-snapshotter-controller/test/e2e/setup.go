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
	apiextensionsv1client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	cfg                *rest.Config
	k8sClient          client.Client
	testEnv            *envtest.Environment
	ctx                context.Context
	cancel             context.CancelFunc
	mgr                ctrl.Manager
	snapshotController *controllers.SnapshotController
	contentController  *controllers.SnapshotContentController
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

	// Install ObjectKeeper CRD manually for e2e tests
	// ObjectKeeper is managed by deckhouse-controller, but we need its CRD for tests
	testCtx := context.Background()
	objectKeeperCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "objectkeepers.deckhouse.io",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "deckhouse.io",
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"spec": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"mode": {
											Type: "string",
										},
										"followObjectRef": {
											Type: "object",
											Properties: map[string]apiextensionsv1.JSONSchemaProps{
												"apiVersion": {Type: "string"},
												"kind":       {Type: "string"},
												"name":       {Type: "string"},
												"namespace":  {Type: "string"},
												"uid":        {Type: "string"},
											},
										},
									},
								},
								"status": {
									Type: "object",
								},
							},
						},
					},
				},
			},
			Scope: apiextensionsv1.ClusterScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   "objectkeepers",
				Singular: "objectkeeper",
				Kind:     "ObjectKeeper",
			},
		},
	}

	// Create CRD client and install ObjectKeeper CRD
	crdClient, err := apiextensionsv1client.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())
	_, err = crdClient.CustomResourceDefinitions().Create(testCtx, objectKeeperCRD, metav1.CreateOptions{})
	// Ignore AlreadyExists error - CRD might already be installed
	if err != nil && !errors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}

<<<<<<< HEAD
	// Install TestSnapshot and TestSnapshotContent CRDs for unified snapshots E2E tests
	testSnapshotCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "testsnapshots.test.deckhouse.io",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "test.deckhouse.io",
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"spec": {
									Type: "object",
								},
								"status": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"contentName": {Type: "string"},
										"conditions": {
											Type: "array",
											Items: &apiextensionsv1.JSONSchemaPropsOrArray{
												Schema: &apiextensionsv1.JSONSchemaProps{
													Type: "object",
													Properties: map[string]apiextensionsv1.JSONSchemaProps{
														"type":               {Type: "string"},
														"status":             {Type: "string"},
														"reason":             {Type: "string"},
														"message":            {Type: "string"},
														"lastTransitionTime": {Type: "string", Format: "date-time"},
														"observedGeneration": {Type: "integer"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
					},
				},
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   "testsnapshots",
				Singular: "testsnapshot",
				Kind:     "TestSnapshot",
			},
		},
	}

	testSnapshotContentCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "testsnapshotcontents.test.deckhouse.io",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "test.deckhouse.io",
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"spec": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"snapshotRef": {
											Type: "object",
											Properties: map[string]apiextensionsv1.JSONSchemaProps{
												"kind":      {Type: "string"},
												"name":      {Type: "string"},
												"namespace": {Type: "string"},
											},
										},
									},
								},
								"status": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"conditions": {
											Type: "array",
											Items: &apiextensionsv1.JSONSchemaPropsOrArray{
												Schema: &apiextensionsv1.JSONSchemaProps{
													Type: "object",
													Properties: map[string]apiextensionsv1.JSONSchemaProps{
														"type":               {Type: "string"},
														"status":             {Type: "string"},
														"reason":             {Type: "string"},
														"message":            {Type: "string"},
														"lastTransitionTime": {Type: "string", Format: "date-time"},
														"observedGeneration": {Type: "integer"},
													},
												},
											},
										},
										"childrenSnapshotContentRefs": {
											Type: "array",
											Items: &apiextensionsv1.JSONSchemaPropsOrArray{
												Schema: &apiextensionsv1.JSONSchemaProps{
													Type: "object",
													Properties: map[string]apiextensionsv1.JSONSchemaProps{
														"kind": {Type: "string"},
														"name": {Type: "string"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
					},
				},
			},
			Scope: apiextensionsv1.ClusterScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   "testsnapshotcontents",
				Singular: "testsnapshotcontent",
				Kind:     "TestSnapshotContent",
			},
		},
	}

	_, err = crdClient.CustomResourceDefinitions().Create(testCtx, testSnapshotCRD, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}

	_, err = crdClient.CustomResourceDefinitions().Create(testCtx, testSnapshotContentCRD, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}

	// Wait for CRDs to be ready
	Eventually(func() bool {
		snapshotCRD, err := crdClient.CustomResourceDefinitions().Get(testCtx, "testsnapshots.test.deckhouse.io", metav1.GetOptions{})
		if err != nil {
			return false
		}
		contentCRD, err := crdClient.CustomResourceDefinitions().Get(testCtx, "testsnapshotcontents.test.deckhouse.io", metav1.GetOptions{})
		if err != nil {
			return false
		}
		// Check if CRDs are established
		for _, condition := range snapshotCRD.Status.Conditions {
			if condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue {
				for _, condition := range contentCRD.Status.Conditions {
					if condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue {
						return true
					}
				}
			}
		}
		return false
	}).Should(BeTrue(), "TestSnapshot CRDs should be established")

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
	mcpController, err := controllers.NewManifestCheckpointController(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		mgr.GetScheme(),
		testLogger,
		cfgOptions,
	)
	Expect(err).NotTo(HaveOccurred())
	err = mcpController.SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	// Setup unified snapshots controllers (SnapshotController and SnapshotContentController)
	// These controllers work with TestSnapshot/TestSnapshotContent for E2E tests
	snapshotGVK := schema.GroupVersionKind{
		Group:   "test.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "TestSnapshot",
	}
	contentGVK := schema.GroupVersionKind{
		Group:   "test.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "TestSnapshotContent",
	}

	snapshotController, err = controllers.NewSnapshotController(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		mgr.GetScheme(),
		cfgOptions,
		[]schema.GroupVersionKind{snapshotGVK},
	)
	Expect(err).NotTo(HaveOccurred())
	err = snapshotController.SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	// NOTE: RetainerController (IRetainer) has been removed.
	// ObjectKeeper is now used instead, which is managed by deckhouse-controller.

	contentController, err = controllers.NewSnapshotContentController(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		mgr.GetScheme(),
		cfgOptions,
		[]schema.GroupVersionKind{contentGVK},
	)
	Expect(err).NotTo(HaveOccurred())
	err = contentController.SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	// Create context
	ctx, cancel = context.WithCancel(testCtx)

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
