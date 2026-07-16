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
	"net/http/httptest"
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
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	deckhousev1alpha1 "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/deckhouseio/v1alpha1"
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
	// integrationContentController is the suite's SnapshotContentController, exposed so specs can drive its
	// dynamic watch registration (AddVolumeCaptureRequestWatch) against the envtest-served VCR CRD.
	integrationContentController *controllers.SnapshotContentController
	// subtreeIdentitiesServer is the suite-scoped fake aggregated API service backing the root's
	// manifest-exclude self-call (snapshotcontents/<name>/subtree-manifest-identities). envtest does not
	// register the core APIService, so the reconciler's SDK cannot reach the real subresource; this
	// httptest server (backed by the manager client) stands in and its REST client is injected into the
	// snapshot controller via controllers.WithSnapshotSubresourceREST. Closed in AfterSuite.
	subtreeIdentitiesServer *httptest.Server
)

func ptrInt64(v int64) *int64 {
	return &v
}

// snapshotContentDataRefSchema is the Variant A singular status.data schema (cardinality ≤1): a
// SnapshotContent carries at most one data binding as an object, not a list. wave5 renamed the binding
// (status.dataRef->data), moved the source PVC under data.source, and dropped the standalone targetUID
// (the volume identity is data.source.uid).
func snapshotContentDataRefSchema() apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"source": {
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
		Required: []string{"source", "artifact"},
	}
}

// snapshotStatusCaptureStateSchema is the envtest structural schema for status.captureState. It must
// list every nested field the controllers and test helpers write (commonController latches +
// domainSpecificController phase/refs), otherwise the apiserver prunes them on status update.
func snapshotStatusCaptureStateSchema() apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"commonController": {
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"manifestCaptured": {Type: "boolean"},
					"dataCaptured":     {Type: "boolean"},
				},
			},
			"domainSpecificController": {
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"phase":                      {Type: "string"},
					"reason":                     {Type: "string"},
					"message":                    {Type: "string"},
					"manifestCaptureRequestName": {Type: "string"},
					"volumeCaptureRequestName":   {Type: "string"},
				},
			},
		},
	}
}

// snapshotSourceStatusSchema is the envtest structural schema for status.sourceRef (the resolved
// top-level source object ref published by the domain/import controllers).
func snapshotSourceStatusSchema() apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"apiVersion": {Type: "string"},
			"kind":       {Type: "string"},
			"name":       {Type: "string"},
			"namespace":  {Type: "string"},
			"uid":        {Type: "string"},
		},
	}
}

// integrationParallelSnapshotGraphGVKs returns resolved graph-registry snapshot↔content GVK slices
// from graph built-ins and eligible CSD rows. Non-built-in domain pairs are intentionally CSD-gated here.
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
										"captureState":             snapshotStatusCaptureStateSchema(),
										"sourceRef":                snapshotSourceStatusSchema(),
										"boundSnapshotContentName": {Type: "string"},
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
										"manifestCheckpointName":    {Type: "string"},
										"data":                      snapshotContentDataRefSchema(),
										"captureState":              snapshotStatusCaptureStateSchema(),
										"subtreeManifestsPersisted": {Type: "boolean"},
										"boundSnapshotDeleted":      {Type: "boolean"},
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
										"captureState":             snapshotStatusCaptureStateSchema(),
										"sourceRef":                snapshotSourceStatusSchema(),
										"boundSnapshotContentName": {Type: "string"},
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
										"manifestCheckpointName":    {Type: "string"},
										"data":                      snapshotContentDataRefSchema(),
										"captureState":              snapshotStatusCaptureStateSchema(),
										"subtreeManifestsPersisted": {Type: "boolean"},
										"boundSnapshotDeleted":      {Type: "boolean"},
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

	// GraphRegistryTestSnapshot is a dedicated, CSD-gated generic snapshot kind used ONLY by the
	// snapshot graph registry CSD-driven refresh spec. It is intentionally not referenced by any other
	// spec so the registry spec can assert exact presence/absence of this kind in the global registry
	// without cross-spec CSD pollution. It carries the snapshot status contract so the CSD reconciler
	// accepts mappings onto it.
	graphRegistrySnapshotCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "graphregistrytestsnapshots.test.deckhouse.io",
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
										"captureState":             snapshotStatusCaptureStateSchema(),
										"sourceRef":                snapshotSourceStatusSchema(),
										"boundSnapshotContentName": {Type: "string"},
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
				Plural:   "graphregistrytestsnapshots",
				Singular: "graphregistrytestsnapshot",
				Kind:     "GraphRegistryTestSnapshot",
			},
		},
	}
	_, err = crdClient.CustomResourceDefinitions().Create(testCtx, graphRegistrySnapshotCRD, metav1.CreateOptions{})
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
		"graphregistrytestsnapshots.test.deckhouse.io",
		"snapshots.state-snapshotter.deckhouse.io",
		"snapshotcontents.state-snapshotter.deckhouse.io",
		"volumesnapshots.snapshot.storage.k8s.io",
	}
	// Explicit timeout: the default Gomega Eventually (1s) is too tight for cold envtest CRD
	// establishment (apiserver just started), which flakes BeforeSuite intermittently.
	Eventually(func() bool {
		notReady := []string{}
		for _, n := range crdNamesWaitEstablished {
			if !crdEstablished(n) {
				notReady = append(notReady, n)
			}
		}
		if len(notReady) > 0 {
			fmt.Fprintf(GinkgoWriter, "CRDs not established yet: %v\n", notReady)
			return false
		}
		return true
	}, 30*time.Second, 1*time.Second).Should(BeTrue(), "CRDs should be established")

	// Create manager
	mgr, err = ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: "0",
		// Disable the metrics server: its default bind is the fixed :8080, which collides across
		// Ginkgo parallel processes (-procs>1) — each proc runs its own BeforeSuite/manager and the
		// second onward fails BeforeSuite with "address already in use". "0" disables the listener.
		Metrics:        metricsserver.Options{BindAddress: "0"},
		LeaderElection: false,
		// Several specs and the unified-runtime Syncer both register a controller for the same
		// test.deckhouse.io/RegistrationTestSnapshot GVK on this single shared manager; without this the
		// second registration is rejected for a duplicate controller name. Test-only.
		Controller: crconfig.Controller{SkipNameValidation: ptrBool(true)},
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(mgr).NotTo(BeNil())

	// Setup config
	testCfg = &config.Options{
		DefaultTTL: 168 * time.Hour,
	}

	integrationLog, err := logger.NewLogger("error")
	Expect(err).NotTo(HaveOccurred())

	var errProv error
	integrationGraphRegProvider, errProv = snapshotgraphregistry.NewProvider(testCfg, mgr.GetRESTMapper(), mgr.GetAPIReader(), ctrl.Log.WithName("integration-graph-registry"))
	Expect(errProv).NotTo(HaveOccurred())
	Expect(integrationSnapshotGraphRegistryRefresh(testCtx)).To(Succeed())

	runtimeSnapGVKs, runtimeContentGVKs := unifiedbootstrap.ResolveAvailableUnifiedGVKPairs(
		mgr.GetRESTMapper(),
		unifiedbootstrap.DefaultGraphRegistryBuiltInPairs(),
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
	// wave7 (w7-creator): mirror cmd/main.go — register the built-in root Snapshot pair on the binder at
	// startup so the root SnapshotContent is created/bound without waiting for a CSD-driven Syncer.Sync.
	if rootSnapGVK, rootContentGVK, ok := unifiedbootstrap.StartupDomainCaptureRootPair(runtimeSnapGVKs, runtimeContentGVKs); ok {
		snapshotController.MarkDomainCaptureKind(rootSnapGVK)
		Expect(snapshotController.AddWatchForPair(mgr, rootSnapGVK, rootContentGVK)).To(Succeed())
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
	integrationContentController = contentController
	for _, snapshotGVK := range runtimeSnapGVKs {
		Expect(contentController.AddSnapshotStatusWatch(mgr, snapshotGVK)).To(Succeed())
	}
	// Mirror cmd/main.go: main runs the root's capture-leg lifecycle (latches + MCR reap, decision #10),
	// so the root pair must be marked domain-capture on the content controller too.
	if rootSnapGVK, _, ok := unifiedbootstrap.StartupDomainCaptureRootPair(runtimeSnapGVKs, runtimeContentGVKs); ok {
		contentController.MarkDomainCaptureKind(rootSnapGVK)
	}

	Expect(controllers.AddManifestCheckpointControllerToManager(mgr, integrationLog, testCfg)).To(Succeed())
	// Stand up the fake subtree-manifest-identities aggregated API service and inject its REST client, so
	// the root capture's manifest-exclude self-call (sdk.SubtreeManifestIdentities) resolves in envtest
	// (which registers no core APIService). The server (aggregatedManifestsIntegrationStartServer) reads
	// live cluster state via k8sClient, so bind the global here — it holds the manager client the whole
	// suite uses (re-set to the same value after cache sync below).
	k8sClient = mgr.GetClient()
	subtreeIdentitiesServer = aggregatedManifestsIntegrationStartServer()
	subtreeIdentitiesREST, err := rest.RESTClientFor(&rest.Config{
		Host:    subtreeIdentitiesServer.URL,
		APIPath: "/apis",
		ContentConfig: rest.ContentConfig{
			GroupVersion:         &schema.GroupVersion{Group: "subresources.state-snapshotter.deckhouse.io", Version: "v1alpha1"},
			NegotiatedSerializer: clientgoscheme.Codecs.WithoutConversion(),
		},
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(controllers.AddSnapshotControllerToManager(mgr, testCfg, integrationGraphRegProvider, controllers.WithSnapshotSubresourceREST(subtreeIdentitiesREST))).To(Succeed())
	// Core is demo-free: no dedicated domain-controller activators are wired here. Domain (demo) controller
	// behavior is covered by the domain module's fake-client unit tests and by e2e; the unified runtime
	// Syncer is exercised with generic CSD-gated pairs only.
	unifiedSyncer = unifiedruntime.NewSyncer(
		mgr,
		ctrl.Log,
		unifiedbootstrap.DefaultGraphRegistryBuiltInPairs(),
		mgr.GetAPIReader(),
		snapshotController,
		contentController,
		nil,
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
	Eventually(func() error {
		return waitForMapping(schema.GroupVersionKind{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "GraphRegistryTestSnapshot"})
	}).Should(Succeed(), "RESTMapper should discover GraphRegistryTestSnapshot")
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	if subtreeIdentitiesServer != nil {
		subtreeIdentitiesServer.Close()
	}
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})
