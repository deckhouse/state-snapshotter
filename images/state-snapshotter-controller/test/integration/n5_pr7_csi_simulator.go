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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// CSI-simulator constants for the N5 PR-7 orphan-wave specs. The driver on the StorageClass-referenced
// VolumeSnapshotClass must match the CSI driver on the bound PV, or the orphan class resolution fails
// closed (orphanPVCVolumeSnapshotClass).
const (
	pr7CSIDriver   = "csi.pr7.example.com"
	pr7SCName      = "pr7-csi-sc"
	pr7VSClassName = "pr7-csi-vsclass"
)

var (
	pr7VSListGVK   = schema.GroupVersionKind{Group: snapshotpkg.CSISnapshotGroup, Version: snapshotpkg.CSISnapshotVersion, Kind: "VolumeSnapshotList"}
	pr7VSCGVK      = schema.GroupVersionKind{Group: snapshotpkg.CSISnapshotGroup, Version: snapshotpkg.CSISnapshotVersion, Kind: snapshotpkg.KindVolumeSnapshotContent}
	pr7VSClassGVKv = schema.GroupVersionKind{Group: snapshotpkg.CSISnapshotGroup, Version: snapshotpkg.CSISnapshotVersion, Kind: snapshotpkg.KindVolumeSnapshotClass}
)

// pr7InstallCSIClassAndContentCRDs installs the cluster-scoped VolumeSnapshotClass and VolumeSnapshotContent
// CRDs (preserve-unknown) that the shared BeforeSuite deliberately omits (integration_csi_snapshot_crd.go
// installs only VolumeSnapshot, because several !isolated specs rely on VolumeSnapshotContent being absent).
// These two are needed ONLY by the isolated N5 PR-7 orphan-wave specs and are installed from the Describe's
// BeforeAll, so they never affect the shared (!isolated) pass — no other spec runs in the isolated pass.
// Idempotent (AlreadyExists tolerated).
func pr7InstallCSIClassAndContentCRDs(ctx context.Context) {
	GinkgoHelper()
	crdClient, err := apiextensionsv1client.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())
	for _, c := range []struct{ plural, singular, kind string }{
		{"volumesnapshotclasses", "volumesnapshotclass", snapshotpkg.KindVolumeSnapshotClass},
		{"volumesnapshotcontents", "volumesnapshotcontent", snapshotpkg.KindVolumeSnapshotContent},
	} {
		// Root-level preserve-unknown (NOT the spec/status-only helper): VolumeSnapshotClass carries its
		// driver/deletionPolicy as TOP-LEVEL fields, and the orphan class resolution reads vscClass.driver
		// at the root. A spec/status-only preserve CRD would prune those top-level fields, so the driver
		// reads back empty and the resolution fails "driver \"\" does not match PV CSI driver".
		crd := pr7RootPreserveCRD(c.plural, c.singular, c.kind)
		if _, cerr := crdClient.CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{}); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			Expect(cerr).NotTo(HaveOccurred())
		}
	}
	for _, gvk := range []schema.GroupVersionKind{pr7VSCGVK, pr7VSClassGVKv} {
		g := gvk
		Eventually(func() error {
			_, merr := mgr.GetRESTMapper().RESTMapping(g.GroupKind(), g.Version)
			return merr
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed(), "RESTMapper should discover %s", g.Kind)
	}
}

// pr7RootPreserveCRD builds a cluster-scoped CRD whose ROOT object is x-kubernetes-preserve-unknown-fields
// (not just spec/status), so top-level fields the controller reads on a VolumeSnapshotClass (driver,
// deletionPolicy) survive a create instead of being pruned by the apiserver.
func pr7RootPreserveCRD(plural, singular, kind string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: plural + "." + snapshotpkg.CSISnapshotGroup,
			// snapshot.storage.k8s.io is a protected (*.k8s.io) group; the apiserver rejects CRDs there
			// without an api-approved.kubernetes.io annotation.
			Annotations: map[string]string{"api-approved.kubernetes.io": "https://github.com/kubernetes-csi/external-snapshotter"},
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: snapshotpkg.CSISnapshotGroup,
			Scope: apiextensionsv1.ClusterScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: plural, Singular: singular, Kind: kind},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    snapshotpkg.CSISnapshotVersion,
				Served:  true,
				Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: ptrBool(true)},
				},
			}},
		},
	}
}

// pr7EnsureSharedCSIClasses creates the shared StorageClass (carrying the volumesnapshotclass annotation)
// and the driver-matching VolumeSnapshotClass used by all orphan-wave specs. Both are cluster-scoped and
// created idempotently in the Describe's BeforeAll.
func pr7EnsureSharedCSIClasses(ctx context.Context) {
	GinkgoHelper()
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pr7SCName,
			Annotations: map[string]string{snapshotpkg.AnnotationStorageClassVolumeSnapshotClass: pr7VSClassName},
		},
		Provisioner: pr7CSIDriver,
	}
	if err := k8sClient.Create(ctx, sc); err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
	vsClass := &unstructured.Unstructured{}
	vsClass.SetGroupVersionKind(pr7VSClassGVKv)
	vsClass.SetName(pr7VSClassName)
	_ = unstructured.SetNestedField(vsClass.Object, pr7CSIDriver, "driver")
	_ = unstructured.SetNestedField(vsClass.Object, "Delete", "deletionPolicy")
	if err := k8sClient.Create(ctx, vsClass); err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

// pr7CreateReadyVSC creates a cluster-scoped VolumeSnapshotContent that reports status.readyToUse=true, so
// a hand-authored dataRef artifact (status.data[].artifact) resolves as a healthy durable artifact instead
// of ArtifactMissing. Under wave7 a SnapshotContent with a dataRef is Ready only once the referenced VSC
// exists and is readyToUse (resolveDataReadiness -> VolumeSnapshotContent status.readyToUse), so synthetic
// dataRef fixtures need a real ready VSC behind them. The VSC CRD carries no status subresource
// (pr7RootPreserveCRD), so status is persisted on create. Idempotent; cleaned up after the spec.
func pr7CreateReadyVSC(ctx context.Context, name string) {
	GinkgoHelper()
	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(pr7VSCGVK)
	vsc.SetName(name)
	_ = unstructured.SetNestedField(vsc.Object, "Delete", "spec", "deletionPolicy")
	_ = unstructured.SetNestedField(vsc.Object, pr7CSIDriver, "spec", "driver")
	_ = unstructured.SetNestedField(vsc.Object, name+"-handle", "spec", "source", "snapshotHandle")
	_ = unstructured.SetNestedField(vsc.Object, true, "status", "readyToUse")
	if err := k8sClient.Create(ctx, vsc); err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
	DeferCleanup(func() {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(pr7VSCGVK)
		obj.SetName(name)
		_ = k8sClient.Delete(context.Background(), obj)
	})
}

// pr7CreateCSIPVC creates a CSI-backed, bound PVC: a cluster-scoped CSI PersistentVolume plus a PVC that
// references it via spec.volumeName + spec.storageClassName. This lets the orphan-wave class resolution
// (PVC -> StorageClass annotation -> VolumeSnapshotClass, validated against the PV CSI driver) succeed, so
// the residual PVC is captured as its own child volume node instead of failing closed with
// VolumeCaptureFailed. envtest has no PV/PVC binding controller, but the controller only reads
// spec.volumeName + the PV's spec.csi.driver, so an explicitly wired volumeName is sufficient.
func pr7CreateCSIPVC(ctx context.Context, namespace, name string) *corev1.PersistentVolumeClaim {
	GinkgoHelper()
	pvName := "pr7-pv-" + namespace + "-" + name
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: pr7CSIDriver, VolumeHandle: pvName + "-handle"},
			},
		},
	}
	if err := k8sClient.Create(ctx, pv); err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
	DeferCleanup(func() {
		_ = k8sClient.Delete(context.Background(), &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: pvName}})
	})
	scName := pr7SCName
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &scName,
			VolumeName:       pvName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
	fresh := &corev1.PersistentVolumeClaim{}
	Eventually(func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, fresh)).To(Succeed())
		g.Expect(fresh.UID).NotTo(BeEmpty())
	}).Should(Succeed())
	return fresh
}

// pr7StartFakeExternalSnapshotter runs a background reactor that plays, for one namespace, BOTH roles
// envtest is missing so the orphan wave can complete end to end under the content-single-writer domain
// model (design §11.6):
//
//   - the external-snapshotter CSI sidecar (pr7ReactExternalSnapshotter): for every orphan VolumeSnapshot
//     the controller creates it creates a bound VolumeSnapshotContent (deletionPolicy=Delete on purpose, to
//     exercise the controller's force-Retain handoff) and fills the VS status (readyToUse=true +
//     boundVolumeSnapshotContentName) — the native-CSI DATA leg the aggregator projects.
//   - the storage-foundation VolumeSnapshot DOMAIN controller (pr7ReactFoundationVSDomain): it CLAIMS each
//     orphan VS (status.captureState.domainSpecificController — which is what un-gates the generic binder's
//     eager content shell, see domainHasClaimed), publishes status.sourceRef (the captured PVC identity
//     the aggregator's native-CSI data leg reads) and drives the MANIFEST leg to a Ready ManifestCheckpoint
//     on the bound content. Neither role runs in state-snapshotter's own envtest, so without them the orphan
//     VS never becomes a Ready domain child and the root capture blocks forever.
//
// The reactor stops when ctx is cancelled (wire it to a DeferCleanup cancel).
func pr7StartFakeExternalSnapshotter(ctx context.Context, namespace string) {
	GinkgoHelper()
	go func() {
		defer GinkgoRecover()
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pr7ReactExternalSnapshotter(ctx, namespace)
				pr7ReactFoundationVSDomain(ctx, namespace)
			}
		}
	}()
}

// pr7DirectClient is a non-cached client for the fake sidecar reactor, built once from the test rest
// config. The reactor must not depend on the manager cache (informers may lag or not be started for the
// reactor's own reads), so it lists/creates/patches directly against the apiserver.
var pr7DirectClient client.Client

func pr7EnsureDirectClient() client.Client {
	GinkgoHelper()
	if pr7DirectClient == nil {
		c, err := client.New(cfg, client.Options{})
		Expect(err).NotTo(HaveOccurred())
		pr7DirectClient = c
	}
	return pr7DirectClient
}

// pr7ReactExternalSnapshotter is one best-effort pass of the fake sidecar: bind every unbound orphan
// VolumeSnapshot in the namespace. Errors are swallowed (the next tick retries) so a transient conflict or
// a cache miss during teardown never fails the spec from inside the goroutine.
func pr7ReactExternalSnapshotter(ctx context.Context, namespace string) {
	dc := pr7EnsureDirectClient()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(pr7VSListGVK)
	if err := dc.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return
	}
	for i := range list.Items {
		vs := &list.Items[i]
		bound, _, _ := unstructured.NestedString(vs.Object, "status", "boundVolumeSnapshotContentName")
		if bound != "" {
			continue
		}
		vscName := "pr7-vsc-" + vs.GetName()
		vsc := &unstructured.Unstructured{}
		vsc.SetGroupVersionKind(pr7VSCGVK)
		vsc.SetName(vscName)
		_ = unstructured.SetNestedField(vsc.Object, "Delete", "spec", "deletionPolicy")
		_ = unstructured.SetNestedField(vsc.Object, pr7CSIDriver, "spec", "driver")
		_ = unstructured.SetNestedField(vsc.Object, vscName+"-handle", "spec", "source", "snapshotHandle")
		_ = unstructured.SetNestedField(vsc.Object, vs.GetName(), "spec", "volumeSnapshotRef", "name")
		_ = unstructured.SetNestedField(vsc.Object, namespace, "spec", "volumeSnapshotRef", "namespace")
		if err := dc.Create(ctx, vsc); err != nil && !apierrors.IsAlreadyExists(err) {
			continue
		}
		base := vs.DeepCopy()
		_ = unstructured.SetNestedField(vs.Object, true, "status", "readyToUse")
		_ = unstructured.SetNestedField(vs.Object, vscName, "status", "boundVolumeSnapshotContentName")
		_ = dc.Status().Patch(ctx, vs, client.MergeFrom(base))
	}
}

// pr7ReactFoundationVSDomain is one best-effort pass of the fake storage-foundation VolumeSnapshot DOMAIN
// controller for the namespace: for every orphan VolumeSnapshot the generic binder/root capture created it
// (a) CLAIMS it and publishes the captured source + a Finished phase, then (b) once the binder has bound the
// VS to its SnapshotContent, drives that content's MANIFEST leg to a Ready ManifestCheckpoint.
//
// Errors are swallowed (the next tick retries) so a transient conflict/cache miss during teardown never
// fails the spec from inside the goroutine. Faithful shortcut, not a full reconciler: it writes the exact
// status fields the state-snapshotter aggregator/binder READ under the domain model —
// status.captureState.domainSpecificController (binder domain-claim gate + barrier-2 Finished),
// status.sourceRef (native-CSI data-leg source), and the content's manifestCheckpointName +
// subtreeManifestsPersisted (same direct-status fixture pattern as pr7PatchSnapshotContent /
// pr7SeedSubtreeManifestsPersisted). The data leg itself is projected by the live aggregator from the CSI
// sidecar's boundVolumeSnapshotContentName.
func pr7ReactFoundationVSDomain(ctx context.Context, namespace string) {
	dc := pr7EnsureDirectClient()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(pr7VSListGVK)
	if err := dc.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return
	}
	for i := range list.Items {
		vs := &list.Items[i]
		pvcName, _, _ := unstructured.NestedString(vs.Object, "spec", "source", "persistentVolumeClaimName")
		if pvcName == "" {
			continue
		}
		pvc := &corev1.PersistentVolumeClaim{}
		if err := dc.Get(ctx, client.ObjectKey{Namespace: namespace, Name: pvcName}, pvc); err != nil {
			continue
		}
		pr7ClaimOrphanVS(ctx, dc, vs, pvc)

		boundContent, _, _ := unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName")
		if boundContent == "" {
			continue // binder has not created+bound the content shell yet; retry next tick.
		}
		mcpName := "pr7-vsmcp-" + vs.GetName()
		if err := pr7EnsureReadyMCPForPVC(ctx, dc, mcpName, pvc); err != nil {
			continue
		}
		_ = pr7SeedContentManifestLeg(ctx, dc, boundContent, mcpName)
	}
}

// pr7ClaimOrphanVS writes the domain claim (status.captureState.domainSpecificController, phase=Finished)
// and the captured source (status.sourceRef) onto an orphan VolumeSnapshot. The claim is the binder
// domain-claim gate key (domainHasClaimed) AND the barrier-2 finalize key (phase=Finished); snapshotSource
// feeds the aggregator's native-CSI data-leg binding.
func pr7ClaimOrphanVS(ctx context.Context, dc client.Client, vs *unstructured.Unstructured, pvc *corev1.PersistentVolumeClaim) {
	base := vs.DeepCopy()
	_ = unstructured.SetNestedField(vs.Object, "Finished", "status", "captureState", "domainSpecificController", "phase")
	_ = unstructured.SetNestedMap(vs.Object, map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"name":       pvc.Name,
		"namespace":  pvc.Namespace,
		"uid":        string(pvc.UID),
	}, "status", "sourceRef")
	_ = dc.Status().Patch(ctx, vs, client.MergeFrom(base))
}

// pr7EnsureReadyMCPForPVC idempotently installs a Ready ManifestCheckpoint (+ its single content chunk)
// that archives the given PVC manifest — the residual PVC's manifest lives in its OWN child volume node's
// ManifestCheckpoint, never in the root aggregator MCP (Variant A). AlreadyExists is tolerated so the
// reactor can re-run every tick.
func pr7EnsureReadyMCPForPVC(ctx context.Context, dc client.Client, mcpName string, pvc *corev1.PersistentVolumeClaim) error {
	objects := []map[string]interface{}{{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":      pvc.Name,
			"namespace": pvc.Namespace,
			"uid":       string(pvc.UID),
		},
	}}
	data, cs := aggregatedManifestsIntegrationEncodeChunk(objects)
	chName := mcpName + "-chunk-0"
	ch := &ssv1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: chName},
		Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: mcpName, Index: 0, Data: data, Checksum: cs, ObjectsCount: len(objects),
		},
	}
	if err := dc.Create(ctx, ch); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: mcpName},
		Spec:       ssv1alpha1.ManifestCheckpointSpec{},
	}
	if err := dc.Create(ctx, mcp); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	fresh := &ssv1alpha1.ManifestCheckpoint{}
	if err := dc.Get(ctx, client.ObjectKey{Name: mcpName}, fresh); err != nil {
		return err
	}
	if apimeta.IsStatusConditionTrue(fresh.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady) {
		return nil
	}
	fresh.Status.Chunks = []ssv1alpha1.ChunkInfo{{Name: chName, Index: 0, Checksum: cs, ObjectsCount: len(objects)}}
	fresh.Status.TotalObjects = len(objects)
	apimeta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
		Type: ssv1alpha1.ManifestCheckpointConditionTypeReady, Status: metav1.ConditionTrue,
		Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
	})
	return dc.Status().Update(ctx, fresh)
}

// pr7SeedContentManifestLeg latches the orphan VS's bound SnapshotContent onto the Ready ManifestCheckpoint
// and marks its subtree manifests persisted, mirroring the domain manifest-leg outcome without the transient
// MCR (same direct-status fixture pattern as pr7PatchSnapshotContent / pr7SeedSubtreeManifestsPersisted).
// The aggregator leaves manifestCheckpointName untouched when captureState...manifestCaptureRequestName is
// empty (reconcileManifestCheckpointNameProjection early-returns), so this direct write is stable.
func pr7SeedContentManifestLeg(ctx context.Context, dc client.Client, contentName, mcpName string) error {
	sc := &storagev1alpha1.SnapshotContent{}
	if err := dc.Get(ctx, client.ObjectKey{Name: contentName}, sc); err != nil {
		return err
	}
	if sc.Status.ManifestCheckpointName == mcpName && sc.Status.SubtreeManifestsPersisted {
		return nil
	}
	base := sc.DeepCopy()
	sc.Status.ManifestCheckpointName = mcpName
	sc.Status.SubtreeManifestsPersisted = true
	return dc.Status().Patch(ctx, sc, client.MergeFrom(base))
}

// pr7OrphanContentForPVC returns whether the orphan VolumeSnapshot domain child for the given residual PVC
// has a bound SnapshotContent whose single data binding targets that PVC. It is the observable proof that a
// residual PVC was captured as its own ordinary domain child (VolumeSnapshot + own SnapshotContent + dataRef)
// under the content-single-writer model — the successor to the old "child volume node" observable.
func pr7OrphanContentForPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (bool, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "state-snapshotter.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "SnapshotContentList",
	})
	if err := k8sClient.List(ctx, list); err != nil {
		return false, err
	}
	for i := range list.Items {
		name, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "data", "sourceRef", "name")
		ns, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "data", "sourceRef", "namespace")
		if name == pvc.Name && ns == pvc.Namespace {
			return true, nil
		}
	}
	return false, nil
}
