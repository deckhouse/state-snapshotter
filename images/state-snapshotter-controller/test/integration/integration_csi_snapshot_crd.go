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

// csiVolumeSnapshotGVKForTest is the CSI VolumeSnapshot kind the namespace-root orphan-PVC data leg
// (internal/controllers/snapshot/orphan_pvc_volume_snapshot.go) Gets/Creates. envtest does not ship the
// external-snapshotter CRDs, so without it that Get returns a no-match error and aborts the reconcile
// before the ManifestCaptureRequest leg runs (N5 PR-7 specs then time out waiting for the root MCR).
var csiVolumeSnapshotGVKForTest = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}

func ptrBool(b bool) *bool { return &b }

// integrationPreserveUnknownCRDInGroup builds a minimal preserve-unknown CRD in an arbitrary group/scope.
// spec and status use x-kubernetes-preserve-unknown-fields so simulated fields are not pruned.
func integrationPreserveUnknownCRDInGroup(group, plural, singular, kind, version string, scope apiextensionsv1.ResourceScope) *apiextensionsv1.CustomResourceDefinition {
	openObject := apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: ptrBool(true)}
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + "." + group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Scope: scope,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   plural,
				Singular: singular,
				Kind:     kind,
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    version,
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

// integrationInstallCSISnapshotCRDs installs only the namespaced CSI VolumeSnapshot CRD (idempotent;
// AlreadyExists tolerated). It deliberately does NOT install VolumeSnapshotContent/VolumeSnapshotClass:
// the SnapshotExport happy-path spec and the PR-7 child fixture rely on VolumeSnapshotContent being
// absent (the SnapshotContent controller then requeues on a no-match instead of resolving the injected
// artifact to ArtifactMissing). The orphan-PVC class resolution terminates on the PR-7 PVCs (no
// storageClassName) before any VolumeSnapshotContent/VolumeSnapshotClass lookup, so VolumeSnapshot alone
// is sufficient to let the reconcile proceed to the manifest-capture leg.
func integrationInstallCSISnapshotCRDs(ctx context.Context, restCfg *rest.Config) {
	crdClient, err := apiextensionsv1client.NewForConfig(restCfg)
	Expect(err).NotTo(HaveOccurred())

	crd := integrationPreserveUnknownCRDInGroup(
		"snapshot.storage.k8s.io", "volumesnapshots", "volumesnapshot", "VolumeSnapshot", "v1",
		apiextensionsv1.NamespaceScoped,
	)
	// snapshot.storage.k8s.io is a protected (*.k8s.io) group; the apiserver rejects CRDs in such groups
	// without an api-approved.kubernetes.io annotation. Point at the upstream external-snapshotter approval.
	if crd.Annotations == nil {
		crd.Annotations = map[string]string{}
	}
	crd.Annotations["api-approved.kubernetes.io"] = "https://github.com/kubernetes-csi/external-snapshotter"
	if _, cerr := crdClient.CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{}); cerr != nil && !errors.IsAlreadyExists(cerr) {
		Expect(cerr).NotTo(HaveOccurred())
	}
}

// integrationWaitCSISnapshotMappings blocks until the RESTMapper discovers the CSI VolumeSnapshot GVK so
// the first namespace-root reconcile does not transiently no-match.
func integrationWaitCSISnapshotMappings() {
	gvk := csiVolumeSnapshotGVKForTest
	Eventually(func() error {
		_, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
		return err
	}, 30*time.Second, 200*time.Millisecond).Should(Succeed(), "RESTMapper should discover %s", gvk.Kind)
}
