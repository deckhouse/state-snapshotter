//go:build integration
// +build integration

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

package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
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
	crconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/csdregistry"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
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
	integrationGraphRegProvider *snapshotgraphregistry.Provider
)

func ptrInt64(v int64) *int64 {
	return &v
}

func ptrString(v string) *string {
	return &v
}

func snapshotContentDataRefsSchema() apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{
		Type: "array",
		Items: &apiextensionsv1.JSONSchemaPropsOrArray{
			Schema: &apiextensionsv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"targetUID": {Type: "string", MinLength: ptrInt64(1)},
					"target": {
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"apiVersion": {Type: "string", MinLength: ptrInt64(1)},
							"kind":       {Type: "string", MinLength: ptrInt64(1)},
							"name":       {Type: "string", MinLength: ptrInt64(1)},
							"namespace":  {Type: "string"},
							"uid":        {Type: "string"},
						},
						Required: []string{"apiVersion", "kind", "name"},
					},
					"artifact": {
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"apiVersion": {Type: "string", MinLength: ptrInt64(1)},
							"kind":       {Type: "string", MinLength: ptrInt64(1)},
							"name":       {Type: "string", MinLength: ptrInt64(1)},
						},
						Required: []string{"apiVersion", "kind", "name"},
					},
				},
				Required: []string{"targetUID", "target", "artifact"},
			},
		},
		XListType:    ptrString("map"),
		XListMapKeys: []string{"targetUID"},
	}
}

// integrationParallelSnapshotGraphGVKs returns resolved graph-registry snapshot↔content GVK slices
// from graph built-ins and eligible CSD rows. Demo domain pairs are intentionally CSD-gated here.
func integrationParallelSnapshotGraphGVKs(ctx context.Context) ([]schema.GroupVersionKind, []schema.GroupVersionKind, error) {
	csdPairs, derr := csdregistry.EligibleUnifiedGVKPairs(ctx, mgr.GetAPIReader())
	if derr != nil {
		csdPairs = nil
	}
	merged := unifiedbootstrap.MergeBootstrapAndCSDPairs(unifiedbootstrap.DefaultGraphRegistryBuiltInPairs(), csdPairs)
	snapGVKs, contentGVKs := unifiedbootstrap.ResolveAvailableUnifiedGVKPairs(
		mgr.GetRESTMapper(),
		merged,
		ctrl.Log.WithName("integration-unified-bootstrap"),
	)
	return snapGVKs, contentGVKs, nil
}

// integrationSnapshotGraphRegistryRefresh rebuilds the integration graph registry (same hook as production CSD→refresh).
func integrationSnapshotGraphRegistryRefresh(ctx context.Context) error {
	if integrationGraphRegProvider == nil {
		return fmt.Errorf("integration graph registry provider is nil")
	}
	snapGVKs, contentGVKs, err := integrationParallelSnapshotGraphGVKs(ctx)
	if err != nil {
		return err
	}
	reg, err := snapshot.NewGVKRegistryFromParallelSnapshotContentPairs(snapGVKs, contentGVKs)
	if err != nil {
		return err
	}
	integrationGraphRegProvider.ReplaceCurrent(reg)
	return nil
}

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

	crdPaths := []string{crdsPath}
	if foundationCRDs, foundationSource, ok := integrationResolveFoundationCRDDir(crdsPath); ok {
		crdPaths = append(crdPaths, foundationCRDs)
		GinkgoWriter.Printf("integration: storage-foundation CRDs from %s (%s)\n", foundationCRDs, foundationSource)
	} else {
		GinkgoWriter.Println("integration: storage-foundation/crds not found; PR-7 pending-VCR uses minimal VolumeCaptureRequest CRD after envtest start")
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     crdPaths,
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
								},
								"status": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"manifestCheckpointName": {Type: "string"},
										"dataRefs":               snapshotContentDataRefsSchema(),
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
								},
								"status": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"manifestCheckpointName": {Type: "string"},
										"dataRefs":               snapshotContentDataRefsSchema(),
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

	// Namespace-scoped *SnapshotContent stand-in for CSD InvalidSpec (content must be cluster-scoped) tests.
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

	// Minimal CSI VolumeSnapshot CRD so the namespace-root orphan-PVC data leg can Get/Create it instead
	// of aborting the reconcile on a no-match (N5 PR-7 specs). VolumeSnapshotContent/Class are
	// intentionally NOT installed (see integrationInstallCSISnapshotCRDs).
	integrationInstallCSISnapshotCRDs(testCtx, cfg)

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
		"snapshots.storage.deckhouse.io",
		"snapshotcontents.storage.deckhouse.io",
		"volumesnapshots.snapshot.storage.k8s.io",
		"demovirtualdisks.demo.state-snapshotter.deckhouse.io",
		"demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io",
		"demovirtualmachines.demo.state-snapshotter.deckhouse.io",
		"demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io",
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
		// Several specs and the unified-runtime Syncer both register a controller for the same
		// test.deckhouse.io/RegistrationTestSnapshot GVK on this single shared manager; without this the
		// second registration is rejected for a duplicate controller name. Test-only.
		Controller: crconfig.Controller{SkipNameValidation: ptrBool(true)},
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

	var errProv error
	integrationGraphRegProvider, errProv = snapshotgraphregistry.NewProvider(testCfg, mgr.GetRESTMapper(), mgr.GetAPIReader(), ctrl.Log.WithName("integration-graph-registry"))
	Expect(errProv).NotTo(HaveOccurred())
	Expect(integrationSnapshotGraphRegistryRefresh(testCtx)).To(Succeed())

	runtimeSnapGVKs, runtimeContentGVKs := unifiedbootstrap.ResolveAvailableUnifiedGVKPairs(
		mgr.GetRESTMapper(),
		testCfg.EffectiveUnifiedBootstrapPairs(),
		ctrl.Log.WithName("integration-unified-runtime-bootstrap"),
	)
	genericSnapGVKs, genericContentGVKs := unifiedbootstrap.FilterGenericSnapshotGVKPairs(runtimeSnapGVKs, runtimeContentGVKs)
	snapshotController, err := controllers.NewGenericSnapshotBinderController(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		scheme,
		testCfg,
		nil,
	)
	Expect(err).NotTo(HaveOccurred())
	for i := range genericSnapGVKs {
		Expect(snapshotController.AddWatchForPair(mgr, genericSnapGVKs[i], genericContentGVKs[i])).To(Succeed())
	}

	var contentController *controllers.SnapshotContentController
	contentController, err = controllers.NewSnapshotContentController(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		scheme,
		mgr.GetRESTMapper(),
		testCfg,
		[]schema.GroupVersionKind{unifiedbootstrap.CommonSnapshotContentGVK()},
	)
	Expect(err).NotTo(HaveOccurred())
	Expect(contentController.SetupWithManager(mgr)).To(Succeed())
	for _, snapshotGVK := range runtimeSnapGVKs {
		Expect(contentController.AddSnapshotStatusWatch(mgr, snapshotGVK)).To(Succeed())
	}

	Expect(controllers.AddManifestCheckpointControllerToManager(mgr, integrationLog, testCfg)).To(Succeed())
	Expect(controllers.AddSnapshotControllerToManager(mgr, testCfg, integrationGraphRegProvider)).To(Succeed())
	// Demo dedicated controllers are NOT registered at boot (mirrors cmd/main.go). They are activated by
	// the unified runtime Syncer via demoActivators once their CSD is watch-eligible. The suite activates
	// them through that real deferred path below (see "activate demo dedicated controllers").
	demoActivators := map[string]unifiedruntime.DedicatedControllerActivator{
		controllercommon.KindDemoVirtualDiskSnapshot: func(m ctrl.Manager) error {
			return controllers.AddDemoVirtualDiskSnapshotControllerToManager(m, testCfg)
		},
		controllercommon.KindDemoVirtualMachineSnapshot: func(m ctrl.Manager) error {
			return controllers.AddDemoVirtualMachineSnapshotControllerToManager(m, testCfg)
		},
	}

	unifiedSyncer = unifiedruntime.NewSyncer(
		mgr,
		ctrl.Log,
		testCfg.EffectiveUnifiedBootstrapPairs(),
		mgr.GetAPIReader(),
		snapshotController,
		contentController,
		demoActivators,
	)
	Expect(controllers.AddCustomSnapshotDefinitionControllerToManager(mgr, integrationLog, testCfg, unifiedSyncer.Sync, integrationSnapshotGraphRegistryRefresh)).To(Succeed())

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

	integrationEnsureVolumeCaptureRequestCRD(testCtx, cfg)
	integrationWaitCSISnapshotMappings()

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

	// Activate demo dedicated controllers through the real deferred path: create a temporary eligible
	// CSD (disk+VM), wait until the unified runtime Syncer registers both demo controllers, then delete
	// it and refresh the graph registry. controller-runtime controllers are never unregistered and
	// activeSnapshotGVKKeys is monotonic, so the demo controllers stay running for the whole suite (like
	// the previous boot registration) — while the snapshot graph registry is left empty of demo kinds at
	// suite start, so CSD-gated discovery specs still observe "no demo without CSD". This exercises the
	// production activation path instead of registering demo controllers eagerly at boot.
	const suiteDemoBootstrapCSD = "integration-suite-bootstrap-demo"
	createEligibleDemoVMAndDiskCSD(testCtx, suiteDemoBootstrapCSD)
	demoDiskSnapshotKey := demov1alpha1.SchemeGroupVersion.WithKind(controllercommon.KindDemoVirtualDiskSnapshot).String()
	demoVMSnapshotKey := demov1alpha1.SchemeGroupVersion.WithKind(controllercommon.KindDemoVirtualMachineSnapshot).String()
	Eventually(func(g Gomega) {
		keys := unifiedSyncer.ActiveSnapshotGVKKeys()
		g.Expect(keys).To(HaveKey(demoDiskSnapshotKey))
		g.Expect(keys).To(HaveKey(demoVMSnapshotKey))
	}).WithTimeout(60*time.Second).WithPolling(200*time.Millisecond).Should(Succeed(),
		"unified runtime Syncer should activate both demo dedicated controllers after the CSD is watch-eligible")
	Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, &ssv1alpha1.CustomSnapshotDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: suiteDemoBootstrapCSD},
	}))).To(Succeed())
	Eventually(func() bool {
		err := k8sClient.Get(testCtx, client.ObjectKey{Name: suiteDemoBootstrapCSD}, &ssv1alpha1.CustomSnapshotDefinition{})
		return errors.IsNotFound(err)
	}).WithTimeout(15 * time.Second).WithPolling(100 * time.Millisecond).Should(BeTrue())
	Expect(integrationSnapshotGraphRegistryRefresh(testCtx)).To(Succeed())

})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})
