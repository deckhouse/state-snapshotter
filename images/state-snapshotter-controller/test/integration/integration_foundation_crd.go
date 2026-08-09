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
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

const volumeCaptureRequestCRDName = "volumecapturerequests.storage-foundation.deckhouse.io"

const dataImportCRDName = "dataimports.storage-foundation.deckhouse.io"

// dataImportGVKForTest is the SVDM DataImport the import data-leg projection reverse-looks-up. Like
// VolumeCaptureRequest it is served by envtest either from storage-foundation/crds (when they are on the CRD
// path) or from the minimal fallback below.
var dataImportGVKForTest = schema.GroupVersionKind{Group: "storage-foundation.deckhouse.io", Version: "v1alpha1", Kind: "DataImport"}

// integrationVolumeCaptureRequestAPIAvailable is true when envtest serves VolumeCaptureRequest
// (from storage-foundation CRD dir and/or minimal fallback CRD installed in BeforeSuite).
var integrationVolumeCaptureRequestAPIAvailable bool

// integrationResolveFoundationCRDDir locates storage-foundation/crds for envtest.
// Override with STORAGE_FOUNDATION_CRDS when repos are not checked out as siblings.
func integrationResolveFoundationCRDDir(crdsPath string) (dir string, source string, ok bool) {
	if env := os.Getenv("STORAGE_FOUNDATION_CRDS"); env != "" {
		if st, err := os.Stat(env); err == nil && st.IsDir() {
			return filepath.Clean(env), "STORAGE_FOUNDATION_CRDS", true
		}
	}
	candidates := []struct {
		path  string
		label string
	}{
		{filepath.Clean(filepath.Join(crdsPath, "..", "..", "storage-foundation", "crds")), "sibling ../../storage-foundation/crds"},
		{filepath.Clean(filepath.Join(crdsPath, "..", "storage-foundation", "crds")), "legacy ../storage-foundation/crds"},
	}
	for _, c := range candidates {
		if st, err := os.Stat(c.path); err == nil && st.IsDir() {
			return c.path, c.label, true
		}
	}
	return "", "", false
}

func integrationMinimalVolumeCaptureRequestCRD() *apiextensionsv1.CustomResourceDefinition {
	// spec.target omits namespace (the captured PVC lives in the VCR namespace).
	specTarget := apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"uid":        {Type: "string", MinLength: ptrInt64(1)},
			"apiVersion": {Type: "string", MinLength: ptrInt64(1)},
			"kind":       {Type: "string", MinLength: ptrInt64(1)},
			"name":       {Type: "string", MinLength: ptrInt64(1)},
		},
		Required: []string{"uid", "apiVersion", "kind", "name"},
	}
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: volumeCaptureRequestCRDName},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "storage-foundation.deckhouse.io",
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:     "volumecapturerequests",
				Singular:   "volumecapturerequest",
				Kind:       "VolumeCaptureRequest",
				ShortNames: []string{"vcr"},
			},
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
										"mode":   {Type: "string"},
										"target": specTarget,
									},
								},
								"status": {Type: "object"},
							},
						},
					},
					Subresources: &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}},
				},
			},
		},
	}
}

func integrationEnsureVolumeCaptureRequestCRD(ctx context.Context, restCfg *rest.Config) {
	crdClient, err := apiextensionsv1client.NewForConfig(restCfg)
	Expect(err).NotTo(HaveOccurred())

	_, err = crdClient.CustomResourceDefinitions().Get(ctx, volumeCaptureRequestCRDName, metav1.GetOptions{})
	if err == nil {
		integrationVolumeCaptureRequestAPIAvailable = true
		return
	}
	Expect(errors.IsNotFound(err)).To(BeTrue(), "unexpected error loading VolumeCaptureRequest CRD: %v", err)

	_, err = crdClient.CustomResourceDefinitions().Create(ctx, integrationMinimalVolumeCaptureRequestCRD(), metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
	GinkgoWriter.Println("integration: installed minimal VolumeCaptureRequest CRD (storage-foundation/crds not on envtest CRD path)")

	Eventually(func(g Gomega) {
		_, err := mgr.GetRESTMapper().RESTMapping(vcpkg.VolumeCaptureRequestGVK.GroupKind(), vcpkg.VolumeCaptureRequestGVK.Version)
		g.Expect(err).NotTo(HaveOccurred())
	}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

	integrationVolumeCaptureRequestAPIAvailable = true
}

// integrationMinimalDataImportCRD is the fallback DataImport CRD used when storage-foundation/crds are not
// on the envtest CRD path. It mirrors the real schema (crds/dataimports.yaml) for every field
// state-snapshotter actually reads or a fixture actually writes — spec.mode/snapshotRef/storageParams and
// status.data.artifactRef/status.volumeMode — including the same `required` sets, so a fixture that is valid
// here is valid against the real CRD too. It deliberately omits the mode-discriminator CEL and the CreatePVC
// half of the spec: those belong to storage-foundation and nothing on this side is under test for them.
// It is a real, pruning structural CRD, not a preserve-unknown stub — a stub would let a spec "pass" on a
// field the apiserver would have rejected or dropped.
func integrationMinimalDataImportCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: dataImportCRDName},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: dataImportGVKForTest.Group,
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   "dataimports",
				Singular: "dataimport",
				Kind:     dataImportGVKForTest.Kind,
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    dataImportGVKForTest.Version,
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"spec": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"ttl":                  {Type: "string"},
										"publish":              {Type: "boolean"},
										"waitForFirstConsumer": {Type: "boolean"},
										"mode":                 {Type: "string", Enum: []apiextensionsv1.JSON{{Raw: []byte(`"CreatePVC"`)}, {Raw: []byte(`"PopulateData"`)}}},
										"snapshotRef": {
											Type: "object",
											Properties: map[string]apiextensionsv1.JSONSchemaProps{
												"apiVersion": {Type: "string"},
												"kind":       {Type: "string"},
												"name":       {Type: "string"},
											},
											Required: []string{"kind", "name"},
										},
										"storageParams": {
											Type: "object",
											Properties: map[string]apiextensionsv1.JSONSchemaProps{
												"storageClassName": {Type: "string", MinLength: ptrInt64(1)},
												"size":             {Type: "string"},
												"volumeMode":       {Type: "string", Enum: []apiextensionsv1.JSON{{Raw: []byte(`"Block"`)}, {Raw: []byte(`"Filesystem"`)}}},
											},
											Required: []string{"storageClassName", "size"},
										},
									},
									Required: []string{"ttl"},
								},
								"status": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"phase":      {Type: "string"},
										"volumeMode": {Type: "string"},
										"data": {
											Type: "object",
											Properties: map[string]apiextensionsv1.JSONSchemaProps{
												"artifactRef": {
													Type: "object",
													Properties: map[string]apiextensionsv1.JSONSchemaProps{
														"apiVersion": {Type: "string"},
														"kind":       {Type: "string"},
														"name":       {Type: "string"},
														"uid":        {Type: "string"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
					Subresources: &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}},
				},
			},
		},
	}
}

// integrationEnsureDataImportCRD makes the DataImport API available to envtest, preferring the real
// storage-foundation CRD when it is on the CRD path and falling back to the minimal one above otherwise —
// the same two-tier arrangement already used for VolumeCaptureRequest, so the specs that need DataImport are
// deterministic on any checkout instead of silently skipping. It blocks until the RESTMapper sees the kind,
// so the first reverse-lookup cannot transiently no-match. Idempotent.
func integrationEnsureDataImportCRD(ctx context.Context, restCfg *rest.Config) {
	crdClient, err := apiextensionsv1client.NewForConfig(restCfg)
	Expect(err).NotTo(HaveOccurred())

	_, err = crdClient.CustomResourceDefinitions().Get(ctx, dataImportCRDName, metav1.GetOptions{})
	if err != nil {
		Expect(errors.IsNotFound(err)).To(BeTrue(), "unexpected error loading DataImport CRD: %v", err)
		_, err = crdClient.CustomResourceDefinitions().Create(ctx, integrationMinimalDataImportCRD(), metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
		GinkgoWriter.Println("integration: installed minimal DataImport CRD (storage-foundation/crds not on envtest CRD path)")
	}

	Eventually(func() error {
		_, mErr := mgr.GetRESTMapper().RESTMapping(dataImportGVKForTest.GroupKind(), dataImportGVKForTest.Version)
		return mErr
	}, 30*time.Second, 200*time.Millisecond).Should(Succeed(), "RESTMapper should discover %s", dataImportGVKForTest.Kind)
}

// crdServesStatusSubresource reports whether the named CRD declares the status subresource for the given
// version. Which write verb persists a fixture's status depends on it: a Status().Update against a CRD
// WITHOUT the subresource 404s (the /status endpoint does not exist), while a plain Update against a CRD
// WITH it silently drops the status. Envtest may serve either shape of the same kind — the real
// storage-foundation CRD when it is on the CRD path, or a minimal fallback installed by a spec — so specs
// must ask instead of assuming.
func crdServesStatusSubresource(ctx context.Context, crdName, version string) bool {
	GinkgoHelper()
	crdClient, err := apiextensionsv1client.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())
	crd, err := crdClient.CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "CRD %s must be installed before its status shape is queried", crdName)
	for _, v := range crd.Spec.Versions {
		if v.Name == version {
			return v.Subresources != nil && v.Subresources.Status != nil
		}
	}
	Fail("CRD " + crdName + " does not serve version " + version)
	return false
}
