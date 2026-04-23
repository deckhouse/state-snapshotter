//go:build integration
// +build integration

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

package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/dscregistry"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedruntime"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

var (
	cfg                         *rest.Config
	k8sClient                   client.Client
	testEnv                     *envtest.Environment
	ctx                         context.Context
	cancel                      context.CancelFunc
	mgr                         ctrl.Manager
	scheme                      *runtime.Scheme
	testCfg                     *config.Options
	unifiedSyncer               *unifiedruntime.Syncer
	integrationGraphGVKRegistry *snapshot.GVKRegistry
)

func TestIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Integration Suite")
}

var _ = BeforeSuite(func() {
	By("bootstrapping test environment")
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	// Calculate path to CRDs
	var crdsPath string
	possiblePaths := []string{
		filepath.Join("..", "..", "..", "..", "crds"),
		filepath.Join("..", "..", "..", "crds"),
		filepath.Join("..", "..", "..", "..", "..", "crds"),
	}

	for _, p := range possiblePaths {
		if _, err := filepath.Abs(p); err == nil {
			crdsPath = p
			break
		}
	}

	if crdsPath == "" {
		crdsPath = filepath.Join("..", "..", "..", "..", "crds")
		GinkgoWriter.Printf("Warning: CRDs path not verified, using: %s\n", crdsPath)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{crdsPath},
		ErrorIfCRDPathMissing: false,
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	// Create scheme
	scheme = runtime.NewScheme()
	err = clientgoscheme.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	err = apiextensionsv1.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	err = deckhousev1alpha1.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	err = ssv1alpha1.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	err = storagev1alpha1.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	err = demov1alpha1.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	// Install ObjectKeeper CRD manually
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
										// metav1.Duration in API JSON (FollowObjectWithTTL); matches deckhouse.io ObjectKeeperSpec.
										"ttl": {Type: "string"},
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

	crdClient, err := apiextensionsv1client.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())
	_, err = crdClient.CustomResourceDefinitions().Create(testCtx, objectKeeperCRD, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}

	// Install test CRDs for integration tests
	// TestSnapshot and TestSnapshotContent for generic snapshot testing
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
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"backupClassName": {
											Type: "string",
										},
									},
								},
								"status": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"manifestCaptureRequestName": {Type: "string"},
										"volumeCaptureRequestName":   {Type: "string"},
										"boundSnapshotContentName":   {Type: "string"},
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
										"manifestCheckpointName": {Type: "string"},
										"dataRef": {
											Type: "object",
											Properties: map[string]apiextensionsv1.JSONSchemaProps{
												"kind":      {Type: "string"},
												"name":      {Type: "string"},
												"namespace": {Type: "string"},
											},
										},
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

	// RegistrationTestSnapshot and RegistrationTestSnapshotContent for controller registration tests
	registrationSnapshotCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "registrationtestsnapshots.test.deckhouse.io",
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
										"backupClassName": {Type: "string"},
									},
								},
								"status": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"manifestCaptureRequestName": {Type: "string"},
										"volumeCaptureRequestName":   {Type: "string"},
										"boundSnapshotContentName":   {Type: "string"},
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
				Plural:   "registrationtestsnapshots",
				Singular: "registrationtestsnapshot",
				Kind:     "RegistrationTestSnapshot",
			},
		},
	}

	registrationSnapshotContentCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "registrationtestsnapshotcontents.test.deckhouse.io",
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
										"manifestCheckpointName": {Type: "string"},
										"dataRef": {
											Type: "object",
											Properties: map[string]apiextensionsv1.JSONSchemaProps{
												"kind":      {Type: "string"},
												"name":      {Type: "string"},
												"namespace": {Type: "string"},
											},
										},
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
				Plural:   "registrationtestsnapshotcontents",
				Singular: "registrationtestsnapshotcontent",
				Kind:     "RegistrationTestSnapshotContent",
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

	_, err = crdClient.CustomResourceDefinitions().Create(testCtx, registrationSnapshotCRD, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}

	_, err = crdClient.CustomResourceDefinitions().Create(testCtx, registrationSnapshotContentCRD, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}

	// Namespace-scoped *SnapshotContent stand-in for DSC InvalidSpec (content must be cluster-scoped) tests.
	namespacedTestSnapshotContentCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "namespacedtestsnapshotcontents.test.deckhouse.io",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "test.deckhouse.io",
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object"},
					},
				},
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   "namespacedtestsnapshotcontents",
				Singular: "namespacedtestsnapshotcontent",
				Kind:     "NamespacedTestSnapshotContent",
			},
		},
	}
	_, err = crdClient.CustomResourceDefinitions().Create(testCtx, namespacedTestSnapshotContentCRD, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}

	// Create BackupClass CRD (required for SnapshotContent creation)
	backupClassCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "backupclasses.storage.deckhouse.io",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "storage.deckhouse.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:     "BackupClass",
				ListKind: "BackupClassList",
				Plural:   "backupclasses",
				Singular: "backupclass",
			},
			Scope: apiextensionsv1.ClusterScoped,
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
										"backupRepositoryName": {
											Type: "string",
										},
										"deletionPolicy": {
											Type: "string",
											Enum: []apiextensionsv1.JSON{
												{Raw: []byte(`"Retain"`)},
												{Raw: []byte(`"Delete"`)},
											},
										},
									},
									Required: []string{"backupRepositoryName"},
								},
							},
						},
					},
				},
			},
		},
	}
	_, err = crdClient.CustomResourceDefinitions().Create(testCtx, backupClassCRD, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}

	crdEstablished := func(name string) bool {
		obj, err := crdClient.CustomResourceDefinitions().Get(testCtx, name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		for _, c := range obj.Status.Conditions {
			if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
				return true
			}
		}
		return false
	}
	crdNamesWaitEstablished := []string{
		"testsnapshots.test.deckhouse.io",
		"testsnapshotcontents.test.deckhouse.io",
		"registrationtestsnapshots.test.deckhouse.io",
		"registrationtestsnapshotcontents.test.deckhouse.io",
		"namespacedtestsnapshotcontents.test.deckhouse.io",
		"backupclasses.storage.deckhouse.io",
		"namespacesnapshots.storage.deckhouse.io",
		"namespacesnapshotcontents.storage.deckhouse.io",
		"snapshotcontents.storage.deckhouse.io",
		"demovirtualdisks.demo.state-snapshotter.deckhouse.io",
		"demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io",
		"demovirtualdisksnapshotcontents.demo.state-snapshotter.deckhouse.io",
		"demovirtualmachines.demo.state-snapshotter.deckhouse.io",
		"demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io",
		"demovirtualmachinesnapshotcontents.demo.state-snapshotter.deckhouse.io",
	}
	Eventually(func() bool {
		for _, n := range crdNamesWaitEstablished {
			if !crdEstablished(n) {
				return false
			}
		}
		return true
	}).Should(BeTrue(), "CRDs should be established")

	// Create manager
	mgr, err = ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(mgr).NotTo(BeNil())

	// Setup config
	testCfg = &config.Options{
		EnableFiltering: false,
		DefaultTTL:      168 * time.Hour,
	}

	integrationLog, err := logger.NewLogger("error")
	Expect(err).NotTo(HaveOccurred())

	dscListingClient, err := client.New(cfg, client.Options{
		Scheme: scheme,
		Mapper: mgr.GetRESTMapper(),
	})
	Expect(err).NotTo(HaveOccurred())
	dscPairs, derr := dscregistry.EligibleUnifiedGVKPairs(testCtx, dscListingClient)
	if derr != nil {
		dscPairs = nil
	}
	merged := unifiedbootstrap.MergeBootstrapAndDSCPairs(testCfg.EffectiveUnifiedBootstrapPairs(), dscPairs)
	snapGVKs, contentGVKs := unifiedbootstrap.ResolveAvailableUnifiedGVKPairs(
		mgr.GetRESTMapper(),
		merged,
		ctrl.Log.WithName("integration-unified-bootstrap"),
	)
	genericSnapGVKs, _ := unifiedbootstrap.FilterGenericSnapshotGVKPairs(snapGVKs, contentGVKs)
	genericContentGVKs := unifiedbootstrap.FilterGenericSnapshotContentGVKs(snapGVKs, contentGVKs)
	var errGraph error
	integrationGraphGVKRegistry, errGraph = snapshot.NewGVKRegistryFromParallelSnapshotContentPairs(snapGVKs, contentGVKs)
	Expect(errGraph).NotTo(HaveOccurred())
	snapshotController, err := controllers.NewSnapshotController(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		scheme,
		testCfg,
		genericSnapGVKs,
	)
	Expect(err).NotTo(HaveOccurred())
	Expect(snapshotController.SetupWithManager(mgr)).To(Succeed())

	var contentController *controllers.SnapshotContentController
	contentController, err = controllers.NewSnapshotContentController(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		scheme,
		mgr.GetRESTMapper(),
		testCfg,
		genericContentGVKs,
	)
	Expect(err).NotTo(HaveOccurred())
	Expect(contentController.SetupWithManager(mgr)).To(Succeed())

	Expect(controllers.AddManifestCheckpointControllerToManager(mgr, integrationLog, testCfg)).To(Succeed())
	Expect(controllers.AddNamespaceSnapshotControllerToManager(mgr, testCfg, integrationGraphGVKRegistry)).To(Succeed())
	Expect(controllers.AddNamespaceSnapshotContentControllerToManager(mgr, testCfg)).To(Succeed())
	Expect(controllers.AddDemoVirtualDiskSnapshotControllerToManager(mgr)).To(Succeed())
	Expect(controllers.AddDemoVirtualMachineSnapshotControllerToManager(mgr)).To(Succeed())

	unifiedSyncer = unifiedruntime.NewSyncer(
		mgr,
		ctrl.Log,
		testCfg.EffectiveUnifiedBootstrapPairs(),
		mgr.GetAPIReader(),
		snapshotController,
		contentController,
	)
	Expect(controllers.AddDomainSpecificSnapshotControllerToManager(mgr, integrationLog, testCfg, unifiedSyncer.Sync)).To(Succeed())

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

	// Wait for RESTMapper to discover test GVKs (avoid discovery race)
	waitForMapping := func(gvk schema.GroupVersionKind) error {
		_, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
		return err
	}
	Eventually(func() error {
		return waitForMapping(schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "TestSnapshot"})
	}).Should(Succeed(), "RESTMapper should discover TestSnapshot")
	Eventually(func() error {
		return waitForMapping(schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "TestSnapshotContent"})
	}).Should(Succeed(), "RESTMapper should discover TestSnapshotContent")
	Eventually(func() error {
		return waitForMapping(schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "RegistrationTestSnapshotContent"})
	}).Should(Succeed(), "RESTMapper should discover RegistrationTestSnapshotContent")
	Eventually(func() error {
		return waitForMapping(schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "RegistrationTestSnapshot"})
	}).Should(Succeed(), "RESTMapper should discover RegistrationTestSnapshot")

	// Create default BackupClass for tests (after client is ready)
	backupClassObj := &unstructured.Unstructured{}
	backupClassObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "storage.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "BackupClass",
	})
	backupClassObj.SetName("test-backup-class")
	backupClassObj.Object["spec"] = map[string]interface{}{
		"backupRepositoryName": "test-repository",
		"deletionPolicy":       "Retain",
	}
	err = k8sClient.Create(testCtx, backupClassObj)
	if err != nil && !errors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})
