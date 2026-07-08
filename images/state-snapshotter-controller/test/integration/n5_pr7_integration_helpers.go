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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	vcctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	volumecaptureuc "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

const pr7TestLabelKey = "state-snapshotter.deckhouse.io/test"

// integrationContentSnapshotRef returns a syntactically valid SnapshotContent.spec.snapshotRef for tests
// that exercise content lifecycle/status rather than the restore handshake. snapshotRef is required by the
// CRD, so every created SnapshotContent must carry one; the referenced Snapshot need not actually exist.
func integrationContentSnapshotRef() *storagev1alpha1.SnapshotSubjectRef {
	return &storagev1alpha1.SnapshotSubjectRef{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "Snapshot",
		Namespace:  "default",
		Name:       "integration-test-snapshot",
	}
}

// ensureContentOwnerSnapshot idempotently creates the Snapshot referenced by integrationContentSnapshotRef.
// The SnapshotContent ManifestsArchived gate (a low-priority Ready leg, added in dd82b4d) dereferences the
// owning Snapshot to validate the declared child graph before it may latch. Since 421c6a1 every test
// SnapshotContent carries this snapshotRef; a missing owner makes the declared-graph read fail-closed
// (declaredComplete=false), pinning ManifestsArchived at Capturing and blocking the first Ready=True. An
// empty-status owner satisfies the gate (no declared children). Left in place for the rest of the suite;
// only ever referenced as an owner, never listed/counted, so leaking it is harmless and idempotent.
func ensureContentOwnerSnapshot(ctx context.Context) {
	ref := integrationContentSnapshotRef()
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ref.Namespace},
		Spec:       storagev1alpha1.SnapshotSpec{},
	}
	if err := k8sClient.Create(ctx, snap); err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

// retainContentSpec returns a CRD-valid SnapshotContent spec (deletionPolicy=Retain + required snapshotRef).
func retainContentSpec() storagev1alpha1.SnapshotContentSpec {
	return storagev1alpha1.SnapshotContentSpec{
		DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
		SnapshotRef:    integrationContentSnapshotRef(),
	}
}

// integrationContentSnapshotRefMap is the unstructured form of integrationContentSnapshotRef for tests that
// build SnapshotContent as *unstructured.Unstructured.
func integrationContentSnapshotRefMap() map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "Snapshot",
		"namespace":  "default",
		"name":       "integration-test-snapshot",
	}
}

// pr7RequireVolumeCaptureRequestAPI skips the pending-VCR scenario when VolumeCaptureRequest is not served.
func pr7RequireVolumeCaptureRequestAPI() {
	if !integrationVolumeCaptureRequestAPIAvailable {
		Skip("VolumeCaptureRequest API unavailable in envtest (set STORAGE_FOUNDATION_CRDS or checkout storage-foundation beside state-snapshotter)")
	}
}

func pr7CreateNamespace(ctx context.Context, labelValue string) *corev1.Namespace {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "nss-pr7-",
			Labels:       map[string]string{pr7TestLabelKey: labelValue},
		},
	}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	Eventually(func(g Gomega) {
		fresh := &corev1.Namespace{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ns.Name}, fresh)).To(Succeed())
		g.Expect(fresh.Status.Phase).To(Equal(corev1.NamespaceActive))
	}).Should(Succeed())
	return ns
}

func pr7CreatePVC(ctx context.Context, namespace, name string) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}
	Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
	var fresh corev1.PersistentVolumeClaim
	Eventually(func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &fresh)).To(Succeed())
		g.Expect(fresh.UID).NotTo(BeEmpty())
	}).Should(Succeed())
	return &fresh
}

func pr7PVCDataBinding(pvc *corev1.PersistentVolumeClaim, vscName string) storagev1alpha1.SnapshotDataBinding {
	return storagev1alpha1.SnapshotDataBinding{
		TargetUID: string(pvc.UID),
		Target: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "PersistentVolumeClaim",
			Name:       pvc.Name,
			Namespace:  pvc.Namespace,
			UID:        pvc.UID,
		},
		Artifact: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: "snapshot.storage.k8s.io/v1",
			Kind:       "VolumeSnapshotContent",
			Name:       vscName,
		},
	}
}

func pr7InstallReadyChildSubtreeFixture(
	ctx context.Context,
	childContentName string,
	nsName string,
	pvc *corev1.PersistentVolumeClaim,
	dataRefs []storagev1alpha1.SnapshotDataBinding,
) {
	mcpName := "mcp-pr7-" + childContentName
	var objects []map[string]interface{}
	if pvc != nil {
		objects = []map[string]interface{}{
			{
				"apiVersion": "v1",
				"kind":       "PersistentVolumeClaim",
				"metadata": map[string]interface{}{
					"name":      pvc.Name,
					"namespace": nsName,
					"uid":       string(pvc.UID),
				},
			},
		}
	}
	aggregatedManifestsIntegrationMustInstallReadyMCP(ctx, k8sClient, mcpName, nsName, objects)
	Expect(pr7PatchSnapshotContent(ctx, childContentName, mcpName, dataRefs)).To(Succeed())
}

func pr7PatchSnapshotContent(
	ctx context.Context,
	contentName string,
	mcpName string,
	dataRefs []storagev1alpha1.SnapshotDataBinding,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		sc := &storagev1alpha1.SnapshotContent{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: contentName}, sc); err != nil {
			return err
		}
		base := sc.DeepCopy()
		if mcpName != "" {
			sc.Status.ManifestCheckpointName = mcpName
		}
		// Variant A (cardinality ≤1): publish the single dataRef on the child volume node. Callers pass at
		// most one binding (a domain/child leaf owns exactly one PVC); >1 would be a fixture bug.
		if len(dataRefs) > 0 {
			Expect(dataRefs).To(HaveLen(1), "Variant A: a SnapshotContent carries at most one dataRef")
			cp := dataRefs[0]
			sc.Status.DataRef = &cp
		}
		// Do not mark SnapshotContent Ready here: SCC would validate VolumeSnapshotContent objects that envtest does not install.
		return k8sClient.Status().Patch(ctx, sc, client.MergeFrom(base))
	})
}

func pr7InstallPendingVCR(ctx context.Context, namespace string, content *storagev1alpha1.SnapshotContent, pvc *corev1.PersistentVolumeClaim) {
	Expect(content.UID).NotTo(BeEmpty())
	vcr := vcctrl.NewVolumeCaptureRequestObject(
		namespace,
		vcpkg.SnapshotContentVCRName(content.UID),
		metav1.OwnerReference{
			APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
			Kind:       "SnapshotContent",
			Name:       content.Name,
			UID:        content.UID,
		},
		[]vcpkg.Target{{
			UID:        string(pvc.UID),
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "PersistentVolumeClaim",
			Name:       pvc.Name,
			Namespace:  pvc.Namespace,
		}},
	)
	Expect(k8sClient.Create(ctx, vcr)).To(Succeed())
}

func pr7WaitSnapshotBound(ctx context.Context, key types.NamespacedName) *storagev1alpha1.Snapshot {
	var snap storagev1alpha1.Snapshot
	// Explicit timeout: Gomega's 1s default is too tight for binding latency once the shared test manager
	// runs the full controller set (the manifest-only Serial spec borderline-missed the 1s window).
	Eventually(func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, key, &snap)).To(Succeed())
		g.Expect(snap.Status.BoundSnapshotContentName).NotTo(BeEmpty())
		g.Expect(snap.UID).NotTo(BeEmpty())
	}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
	Expect(k8sClient.Get(ctx, key, &snap)).To(Succeed())
	return &snap
}

func pr7GetMCR(ctx context.Context, nsName string, snap *storagev1alpha1.Snapshot) (*ssv1alpha1.ManifestCaptureRequest, error) {
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: namespacemanifest.SnapshotMCRName(snap.UID)}, mcr)
	return mcr, err
}

func pr7MCRHasPVCTarget(mcr *ssv1alpha1.ManifestCaptureRequest, pvc *corev1.PersistentVolumeClaim) bool {
	for _, t := range mcr.Spec.Targets {
		if t.Kind != "PersistentVolumeClaim" || t.APIVersion != corev1.SchemeGroupVersion.String() {
			continue
		}
		if t.Name == pvc.Name {
			return true
		}
	}
	return false
}

func pr7AssertSnapshotDoesNotUseStubAnnotation(snap *storagev1alpha1.Snapshot) {
	Expect(snap.Annotations).NotTo(HaveKey(volumecaptureuc.AnnotationStubVolumeCapturePVCs))
}

func pr7KickSnapshot(ctx context.Context, key types.NamespacedName) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		s := &storagev1alpha1.Snapshot{}
		if err := k8sClient.Get(ctx, key, s); err != nil {
			return err
		}
		base := s.DeepCopy()
		if s.Annotations == nil {
			s.Annotations = map[string]string{}
		}
		s.Annotations["state-snapshotter.deckhouse.io/integration-kick"] = fmt.Sprintf("%d", metav1.Now().UnixNano())
		return k8sClient.Patch(ctx, s, client.MergeFrom(base))
	})).To(Succeed())
}
