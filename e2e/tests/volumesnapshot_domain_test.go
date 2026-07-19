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

package tests

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// Block 3d (content-single-writer design §11.6): under the deployed storage-foundation VolumeSnapshot
// DOMAIN controller a module-managed CSI VolumeSnapshot is an ordinary state-snapshotter domain snapshot.
// The fork stamps storage-foundation.deckhouse.io/processed on entry; the domain controller latches the
// exclude-veto outcome onto state-snapshotter.deckhouse.io/managed (true = domain-captured, false = plain
// CSI snapshot only) and, when managed, runs the manifest leg (MCR -> Ready MCP) + planning barrier so the
// core binder creates+binds the SnapshotContent and the aggregator projects its status.
const (
	// labelForkProcessed is the fork discriminator (storage-foundation.deckhouse.io/processed) stamped by
	// the patched external-snapshotter on every VolumeSnapshot it newly reconciles.
	labelForkProcessed = "storage-foundation.deckhouse.io/processed"
	// labelSnapshotManaged latches the adoption veto outcome (state-snapshotter.deckhouse.io/managed).
	labelSnapshotManaged = storagev1alpha1.APIGroup + "/managed"

	managedTrue  = "true"
	managedFalse = "false"
)

// vsdManifestCaptureRequestGVR is the namespaced ManifestCaptureRequest the VS domain creates for the
// manifest leg. A vetoed VolumeSnapshot must never own one.
var vsdManifestCaptureRequestGVR = schema.GroupVersionResource{
	Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "manifestcapturerequests",
}

// vsForSourcePVC returns the CSI VolumeSnapshot in ns whose spec.source.persistentVolumeClaimName == pvc,
// and whether one was found. The orphan/residual wave and the user both create such objects.
func vsForSourcePVC(ctx context.Context, ns, pvc string) (*unstructured.Unstructured, bool, error) {
	list, err := suiteDyn.Resource(volumeSnapshotGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, false, err
	}
	for i := range list.Items {
		src, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "source", "persistentVolumeClaimName")
		if src == pvc {
			return &list.Items[i], true, nil
		}
	}
	return nil, false, nil
}

// buildUserVolumeSnapshot builds a user-created standalone CSI VolumeSnapshot over a PVC (mirrors a
// backup operator taking an ad-hoc snapshot); the domain controller adopts it like any managed snapshot.
func buildUserVolumeSnapshot(ns, name, pvc, vscClass string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"volumeSnapshotClassName": vscClass,
			"source": map[string]interface{}{
				"persistentVolumeClaimName": pvc,
			},
		},
	}}
}

// waitVSManagedLabel polls a VolumeSnapshot until its state-snapshotter.deckhouse.io/managed label equals
// want (the adoption-veto latch). The domain controller writes it on its first reconcile.
func waitVSManagedLabel(ctx context.Context, ns, name, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		vs, err := getResource(ctx, volumeSnapshotGVR, ns, name)
		if err == nil {
			got := vs.GetLabels()[labelSnapshotManaged]
			if got == want {
				return nil
			}
			last = fmt.Sprintf("managed=%q processed=%q", got, vs.GetLabels()[labelForkProcessed])
		} else {
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for VolumeSnapshot %s/%s label %s=%s; last: %s", ns, name, labelSnapshotManaged, want, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// waitVSBoundStateSnapshotContent polls a VolumeSnapshot until its EXTENDED status.boundSnapshotContentName
// (the state-snapshotter SnapshotContent the binder created+bound, distinct from the CSI
// status.boundVolumeSnapshotContentName) is set, returning that content name.
func waitVSBoundStateSnapshotContent(ctx context.Context, ns, name string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		vs, err := getResource(ctx, volumeSnapshotGVR, ns, name)
		if err == nil {
			content, _, _ := unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName")
			if content != "" {
				return content, nil
			}
			last = "boundSnapshotContentName empty"
		} else {
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timeout waiting for VolumeSnapshot %s/%s status.boundSnapshotContentName; last: %s", ns, name, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return "", ctx.Err()
		}
	}
}

// assertContentManifestLegReady asserts the content projects a manifestCheckpointName that points at a
// Ready ManifestCheckpoint owned by that content (the VS domain manifest leg landed via MCR -> MCP and the
// aggregator projected + re-parented it).
func assertContentManifestLegReady(ctx context.Context, g Gomega, contentName string) {
	content, err := getResource(ctx, snapshotContentGVR, "", contentName)
	g.Expect(err).NotTo(HaveOccurred(), "get SnapshotContent %s", contentName)
	mcpName, _, _ := unstructured.NestedString(content.Object, "status", "manifestCheckpointName")
	g.Expect(mcpName).NotTo(BeEmpty(), "SnapshotContent %s must project status.manifestCheckpointName", contentName)
	mcp, err := getResource(ctx, manifestCheckpointGVR, "", mcpName)
	g.Expect(err).NotTo(HaveOccurred(), "get ManifestCheckpoint %s", mcpName)
	st, _, found := conditionStatus(mcp, condReady)
	g.Expect(found && st == "True").To(BeTrue(), "ManifestCheckpoint %s must be Ready", mcpName)
	g.Expect(ownedBySnapshotContent(mcp, contentName)).To(BeTrue(),
		"ManifestCheckpoint %s must be owned by its SnapshotContent %s (aggregator handoff)", mcpName, contentName)
}

// assertContentDataRetainOwned asserts the content projects a VolumeSnapshotContent data artifact that is
// Retain + owned by the content (native-CSI data leg handoff, projected by the aggregator).
func assertContentDataRetainOwned(ctx context.Context, g Gomega, contentName string) {
	content, err := getResource(ctx, snapshotContentGVR, "", contentName)
	g.Expect(err).NotTo(HaveOccurred(), "get SnapshotContent %s", contentName)
	artifactKind, _, _ := unstructured.NestedString(content.Object, "status", "data", "artifactRef", "kind")
	vscName, _, _ := unstructured.NestedString(content.Object, "status", "data", "artifactRef", "name")
	g.Expect(artifactKind).To(Equal("VolumeSnapshotContent"), "content %s must project a VolumeSnapshotContent data artifact", contentName)
	g.Expect(vscName).NotTo(BeEmpty(), "content %s data artifact name must be set", contentName)
	vsc, err := getResource(ctx, volumeSnapshotContentGVR, "", vscName)
	g.Expect(err).NotTo(HaveOccurred(), "get VolumeSnapshotContent %s", vscName)
	policy, _, _ := unstructured.NestedString(vsc.Object, "spec", "deletionPolicy")
	g.Expect(policy).To(Equal("Retain"), "VolumeSnapshotContent %s must be Retain", vscName)
	g.Expect(ownedBySnapshotContent(vsc, contentName)).To(BeTrue(),
		"VolumeSnapshotContent %s must be owned by its SnapshotContent %s (aggregator handoff)", vscName, contentName)
}

// volumeSnapshotDomainSpecs registers the Block 3d VolumeSnapshot-domain specs (env-gated by
// E2E_VOLUME_DATA): a user-created standalone VolumeSnapshot is adopted + d8-exportable, and a
// VolumeSnapshot on a vetoed PVC stays a plain CSI snapshot (managed=false, no MCR, no state-snapshotter
// content). The orphan-PVC-as-domain-child assertions live in volumeDataSpecs (they reuse the phase-3 tree).
func volumeSnapshotDomainSpecs() {
	Context("Block 3d: VolumeSnapshot domain (user + vetoed)", func() {
		var (
			ns       string
			sc       string
			vscClass string
		)
		const (
			vsdUserPVC   = "vsd-user-pvc"
			vsdVetoedPVC = "vsd-vetoed-pvc"
			vsdProbe     = "vsd-probe"
			userVSName   = "vsd-user-snap"
			vetoedVSName = "vsd-vetoed-snap"
		)

		BeforeAll(func() {
			if !suiteCfg.volumeData {
				Skip("E2E_VOLUME_DATA=false: skipping the Block 3d VolumeSnapshot-domain specs (they run by default)")
			}
			sc = suiteCfg.storageClass
			ns = uniqueNS("vsdom")

			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
			defer cancel()

			By("Ensuring the thin, snapshot-capable StorageClass + VolumeSnapshotClass wiring (idempotent)")
			_, err := testkit.EnsureDefaultStorageClass(ctx, suiteRestCfg, testkit.DefaultStorageClassConfig{
				StorageClassName:     sc,
				LVMType:              "Thin",
				ThinPoolName:         "thinpool",
				BaseKubeconfig:       suiteClusterResources.BaseKubeconfig,
				VMNamespace:          suiteCfg.vmNamespace,
				BaseStorageClassName: suiteCfg.baseStorageClass,
			})
			Expect(err).NotTo(HaveOccurred(), "ensure StorageClass")
			Expect(ensureStorageClassVolumeSnapshotClass(ctx, sc)).To(Succeed())
			vscClass, err = resolveLocalVolumeSnapshotClass(ctx)
			Expect(err).NotTo(HaveOccurred(), "resolve VolumeSnapshotClass")

			By("Creating the namespace and the user + vetoed source PVCs")
			Expect(ensureNamespace(ctx, ns)).To(Succeed())
			userPVC := buildPlainPVC(ns, vsdUserPVC, sc, nil)
			vetoedPVC := buildPlainPVC(ns, vsdVetoedPVC, sc, map[string]interface{}{storagev1alpha1.ExcludeLabelKey: "true"})
			Expect(applyObjects(ctx, []*unstructured.Unstructured{userPVC, vetoedPVC}, ns)).To(Succeed())

			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, ns)
			})

			By("Binding both PVCs with a probe Pod (WaitForFirstConsumer StorageClass)")
			_, err = suiteClientset.CoreV1().Pods(ns).Create(ctx, probePodSpec(ns, vsdProbe, []string{vsdUserPVC, vsdVetoedPVC}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create probe pod")
			Expect(waitPodRunning(ctx, ns, vsdProbe, 10*time.Minute)).To(Succeed())
		})

		It("adopts a user-created standalone VolumeSnapshot and makes it d8-exportable", func() {
			// Four sequential per-step waits each budgeted at snapshotReadyTO (managed latch, bound content,
			// leg projection, Ready mirror) plus the connector read run under one ctx — size it to their sum
			// (N*perStepTO+buffer idiom) so a later wait is never truncated by a short parent deadline.
			ctx, cancel := context.WithTimeout(context.Background(), 4*suiteCfg.snapshotReadyTO+5*time.Minute)
			defer cancel()

			By("Creating a user VolumeSnapshot over " + vsdUserPVC)
			_, err := suiteDyn.Resource(volumeSnapshotGVR).Namespace(ns).Create(ctx, buildUserVolumeSnapshot(ns, userVSName, vsdUserPVC, vscClass), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create user VolumeSnapshot")

			By("Waiting for the domain controller to adopt it (fork label + managed=true)")
			Expect(waitVSManagedLabel(ctx, ns, userVSName, managedTrue, suiteCfg.snapshotReadyTO)).To(Succeed())
			vs, err := getResource(ctx, volumeSnapshotGVR, ns, userVSName)
			Expect(err).NotTo(HaveOccurred())
			_, hasProcessed := vs.GetLabels()[labelForkProcessed]
			Expect(hasProcessed).To(BeTrue(), "adopted VolumeSnapshot must carry the fork %s label", labelForkProcessed)

			By("Waiting for the binder to create+bind its state-snapshotter SnapshotContent")
			contentName, err := waitVSBoundStateSnapshotContent(ctx, ns, userVSName, suiteCfg.snapshotReadyTO)
			Expect(err).NotTo(HaveOccurred())

			By("Asserting the content's manifest + native-CSI data legs are projected by the aggregator")
			Eventually(func(g Gomega) {
				assertContentManifestLegReady(ctx, g, contentName)
				assertContentDataRetainOwned(ctx, g, contentName)
			}).WithTimeout(suiteCfg.snapshotReadyTO).WithPolling(pollInterval).Should(Succeed())

			By("Asserting conditions[Ready] is mirrored onto the VolumeSnapshot")
			Expect(waitObjectCondition(ctx, volumeSnapshotGVR, ns, userVSName, condReady, "True", suiteCfg.snapshotReadyTO)).To(Succeed())

			By("Reading the VS connector manifests-download (one-node tree with the PVC)")
			body, err := aggGet(ctx, vsConnectorSubPath(ns, userVSName, subManifestsDownload), nil)
			Expect(err).NotTo(HaveOccurred(), "connector manifests-download for %s", userVSName)
			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			_, hasPVC := findManifest(objs, "PersistentVolumeClaim", vsdUserPVC)
			Expect(hasPVC).To(BeTrue(), "the exported one-node tree must carry the source PVC %s manifest", vsdUserPVC)
		})

		It("leaves a VolumeSnapshot on a vetoed PVC as a plain CSI snapshot (managed=false, no MCR, no content)", func() {
			// Two sequential per-step waits at snapshotReadyTO (managed=false latch, CSI bound) plus the 30s
			// Consistently no-domain-capture guard share one ctx — size it to their sum (N*perStepTO+buffer).
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.snapshotReadyTO+2*time.Minute)
			defer cancel()

			By("Creating a user VolumeSnapshot over the vetoed PVC " + vsdVetoedPVC)
			_, err := suiteDyn.Resource(volumeSnapshotGVR).Namespace(ns).Create(ctx, buildUserVolumeSnapshot(ns, vetoedVSName, vsdVetoedPVC, vscClass), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create vetoed VolumeSnapshot")

			By("Waiting for the adoption veto to latch managed=false")
			Expect(waitVSManagedLabel(ctx, ns, vetoedVSName, managedFalse, suiteCfg.snapshotReadyTO)).To(Succeed())

			By("Asserting the plain CSI snapshot still works (bound CSI VolumeSnapshotContent)")
			Eventually(func(g Gomega) {
				vs, gerr := getResource(ctx, volumeSnapshotGVR, ns, vetoedVSName)
				g.Expect(gerr).NotTo(HaveOccurred())
				bound, _, _ := unstructured.NestedString(vs.Object, "status", "boundVolumeSnapshotContentName")
				g.Expect(bound).NotTo(BeEmpty(), "the fork must still create a plain CSI VolumeSnapshotContent for a vetoed VS")
			}).WithTimeout(suiteCfg.snapshotReadyTO).WithPolling(pollInterval).Should(Succeed())

			By("Asserting no domain capture happened: no MCR owned by the VS, no state-snapshotter content bound")
			Consistently(func(g Gomega) {
				vs, gerr := getResource(ctx, volumeSnapshotGVR, ns, vetoedVSName)
				g.Expect(gerr).NotTo(HaveOccurred())
				content, _, _ := unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName")
				g.Expect(content).To(BeEmpty(), "a vetoed VolumeSnapshot must NOT get a state-snapshotter SnapshotContent")

				mcrs, lerr := suiteDyn.Resource(vsdManifestCaptureRequestGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
				g.Expect(lerr).NotTo(HaveOccurred())
				for i := range mcrs.Items {
					g.Expect(ownedByVolumeSnapshot(&mcrs.Items[i], vetoedVSName)).To(BeFalse(),
						"a vetoed VolumeSnapshot must NOT own a ManifestCaptureRequest (%s)", mcrs.Items[i].GetName())
				}
			}).WithTimeout(30 * time.Second).WithPolling(pollInterval).Should(Succeed())
		})
	})
}

// ownedByVolumeSnapshot reports whether obj carries an ownerReference to the named VolumeSnapshot.
func ownedByVolumeSnapshot(obj *unstructured.Unstructured, vsName string) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == "VolumeSnapshot" && ref.Name == vsName {
			return true
		}
	}
	return false
}

// buildPlainPVC builds a RWO PVC on sc with optional labels (used for the exclude-veto label).
func buildPlainPVC(ns, name, sc string, labels map[string]interface{}) *unstructured.Unstructured {
	meta := map[string]interface{}{
		"name":      name,
		"namespace": ns,
	}
	if len(labels) > 0 {
		meta["labels"] = labels
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   meta,
		"spec": map[string]interface{}{
			"accessModes":      []interface{}{"ReadWriteOnce"},
			"storageClassName": sc,
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{"storage": "500Mi"},
			},
		},
	}}
}
