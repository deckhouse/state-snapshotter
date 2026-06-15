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
	"time"

	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// Minimal cross-repo CRDs the SnapshotExport/SnapshotImport controllers drive as unstructured.
// They are owned by storage-foundation (VolumeRestoreRequest) and storage-volume-data-manager
// (DataExport/DataImport); envtest does not run those controllers, so the integration specs simulate
// them by writing status directly. The schemas use x-kubernetes-preserve-unknown-fields on spec and
// status so the test can write arbitrary fields (conditions, url, dataRefs, ...) without pruning.
var exportImportCRDGVKs = []schema.GroupVersionKind{
	{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "VolumeRestoreRequest"},
	{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "DataExport"},
	{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "DataImport"},
}

func ptrBool(b bool) *bool { return &b }

func integrationPreserveUnknownCRD(plural, singular, kind string) *apiextensionsv1.CustomResourceDefinition {
	openObject := apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: ptrBool(true)}
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + ".storage.deckhouse.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "storage.deckhouse.io",
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   plural,
				Singular: singular,
				Kind:     kind,
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
								"spec":   openObject,
								"status": openObject,
							},
						},
					},
					Subresources: &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}},
				},
			},
		},
	}
}

// integrationInstallExportImportCRDs installs the minimal VRR/DataExport/DataImport CRDs (idempotent).
// AlreadyExists is tolerated so the real storage-foundation/SVDM CRDs (if present on the envtest path)
// take precedence.
//
// It also installs a preserve-unknown VolumeCaptureRequest CRD here (before the suite's minimal
// fallback in integrationEnsureVolumeCaptureRequestCRD, which is a no-op once the CRD exists). The
// SnapshotImport controller reads vcr.status.dataRefs to learn the captured VolumeSnapshotContent
// name; the suite's minimal VCR schema (status: object) would prune those simulated status fields,
// so a preserve-unknown status is required for the import integration spec.
//
// Precedence: a real storage-foundation VCR CRD loaded at testEnv.Start() (if present on the envtest
// CRD path) is created first and wins via the AlreadyExists tolerance below; the import spec then
// relies on that real status schema preserving status.dataRefs (it does). The preserve-unknown CRD
// here only takes effect when no real VCR CRD is on the path.
func integrationInstallExportImportCRDs(ctx context.Context, restCfg *rest.Config) {
	crdClient, err := apiextensionsv1client.NewForConfig(restCfg)
	Expect(err).NotTo(HaveOccurred())

	crds := []*apiextensionsv1.CustomResourceDefinition{
		integrationPreserveUnknownCRD("volumerestorerequests", "volumerestorerequest", "VolumeRestoreRequest"),
		integrationPreserveUnknownCRD("dataexports", "dataexport", "DataExport"),
		integrationPreserveUnknownCRD("dataimports", "dataimport", "DataImport"),
		integrationPreserveUnknownCRD("volumecapturerequests", "volumecapturerequest", "VolumeCaptureRequest"),
	}
	for _, crd := range crds {
		_, err := crdClient.CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	}
}

// integrationWaitExportImportMappings blocks until the RESTMapper discovers the cross-repo GVKs the
// export/import controllers create as unstructured (avoids a discovery race on first reconcile).
func integrationWaitExportImportMappings() {
	for _, gvk := range exportImportCRDGVKs {
		g := gvk
		Eventually(func() error {
			_, err := mgr.GetRESTMapper().RESTMapping(g.GroupKind(), g.Version)
			return err
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed(), "RESTMapper should discover %s", g.Kind)
	}
	// SnapshotExport/SnapshotImport are state-snapshotter's own CRDs (loaded from crds/); ensure they
	// are mapped too since the controllers For() them.
	for _, kind := range []string{"SnapshotExport", "SnapshotImport"} {
		k := kind
		Eventually(func() error {
			_, err := mgr.GetRESTMapper().RESTMapping(schema.GroupKind{Group: "storage.deckhouse.io", Kind: k}, "v1alpha1")
			return err
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed(), "RESTMapper should discover %s", k)
	}
}
