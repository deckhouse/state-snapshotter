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
	"k8s.io/client-go/rest"

	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

const volumeCaptureRequestCRDName = "volumecapturerequests.storage.deckhouse.io"

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
	targetItem := apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"uid":        {Type: "string", MinLength: ptrInt64(1)},
			"apiVersion": {Type: "string", MinLength: ptrInt64(1)},
			"kind":       {Type: "string", MinLength: ptrInt64(1)},
			"name":       {Type: "string", MinLength: ptrInt64(1)},
			"namespace":  {Type: "string", MinLength: ptrInt64(1)},
		},
		Required: []string{"uid", "apiVersion", "kind", "name", "namespace"},
	}
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: volumeCaptureRequestCRDName},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "storage.deckhouse.io",
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
										"mode":    {Type: "string"},
										"targets": {Type: "array", Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &targetItem}},
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
