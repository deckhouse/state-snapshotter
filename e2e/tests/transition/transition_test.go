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

// Package transition is a manual, developer-run Ginkgo suite that verifies the
// snapshot-controller + storage-volume-data-manager (svdm) -> state-snapshotter +
// storage-foundation consolidation on ONE dev cluster. See README.md for scope, phases and the
// full list of environment variables.
//
// It is a SEPARATE suite (own cluster_config.yml, own bootstrap) because the main
// state-snapshotter suite brings its cluster up with storage-foundation/state-snapshotter already
// enabled — the opposite of what this scenario needs. All module lifecycle (enable / MPO-retag /
// order) is driven at runtime from the test.
package transition

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	clientgokube "k8s.io/client-go/kubernetes"

	"github.com/deckhouse/storage-e2e/pkg/cluster"
	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

// --- env knobs -------------------------------------------------------------

const (
	envRunTransition = "E2E_RUN_TRANSITION"

	// Scenario image-tag vars for the two modules that exist ONLY in this scenario (not in the
	// main suite's cluster_config). svdm needs two slots: the legacy old-group image and the
	// v0.2.0/D1 new-group image the migration retags to.
	envSnapshotControllerTag = "E2E_TRANSITION_SNAPSHOT_CONTROLLER_TAG"
	envSvdmLegacyTag         = "E2E_TRANSITION_SVDM_LEGACY_TAG"
	envSvdmTag               = "E2E_TRANSITION_SVDM_TAG"

	// Standard storage-e2e <MODULE>_MODULE_PULL_OVERRIDE vars for modules the test enables at
	// runtime (they are preseeded disabled in cluster_config.yml, so bootstrap does not read them).
	envStateSnapshotterOverride  = "STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE"
	envStorageFoundationOverride = "STORAGE_FOUNDATION_MODULE_PULL_OVERRIDE"
	envSdsLocalVolumeOverride    = "SDS_LOCAL_VOLUME_MODULE_PULL_OVERRIDE"

	defaultTag = "main"
)

// module names (Deckhouse ModuleConfig / cluster_config names).
const (
	modSnapshotController = "snapshot-controller"
	modSvdm               = "storage-volume-data-manager"
	modStateSnapshotter   = "state-snapshotter"
	modStorageFoundation  = "storage-foundation"
	modSdsLocalVolume     = "sds-local-volume"
)

const moduleReadyTimeout = 15 * time.Minute

// data-plane knobs / fixtures
const (
	// E2E_TRANSITION_PROBE_IMAGE must provide sh + sha256sum (busybox is enough for the data steps;
	// the svdm HTTP steps additionally need curl — see README / phase-B HTTP note).
	envProbeImage     = "E2E_TRANSITION_PROBE_IMAGE"
	defaultProbeImage = "busybox:1.36"

	// E2E_TRANSITION_STORAGE_CLASS: a snapshot-capable StorageClass provisioned on the cluster
	// (with a VolumeSnapshotClass in E2E_TRANSITION_VS_CLASS). Provisioning the sds-local-volume
	// backend (LVMVolumeGroups / LocalStorageClass / VolumeSnapshotClass) is an environmental
	// precondition of the data-plane phases; when unset those steps are skipped.
	envStorageClass = "E2E_TRANSITION_STORAGE_CLASS"
	envVSClass      = "E2E_TRANSITION_VS_CLASS"

	// legacy finalizer the pre-D1 svdm controller put on CRs and PVCs; the migration hook sweeps it.
	legacyFinalizer = "storage.deckhouse.io/storage-manager-controller"

	workloadNS   = "transition-workload"
	srcPVCName   = "src-data"
	probePodName = "probe"
)

var markerPath = "/mnt/" + srcPVCName + "/marker"

var (
	suiteRes         *cluster.TestClusterResources
	anySpecFailed    bool
	transitionActive bool

	// fixture carried across phases
	sourceChecksum string
	vsName         = "legacy-snap"
	boundContent   string
	vsUID          string
)

func probeImage() string {
	if v := strings.TrimSpace(os.Getenv(envProbeImage)); v != "" {
		return v
	}
	return defaultProbeImage
}

// dataPlaneEnabled reports whether a snapshot-capable StorageClass + VolumeSnapshotClass were
// provided; the data-integrity steps are skipped (not failed) when they are not.
func dataPlaneEnabled() bool {
	return strings.TrimSpace(os.Getenv(envStorageClass)) != "" && strings.TrimSpace(os.Getenv(envVSClass)) != ""
}

// tagFrom reads an image tag from env, falling back to "main".
func tagFrom(env string) string {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v
	}
	return defaultTag
}

func transitionEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envRunTransition))) {
	case "true", "1", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func TestSnapshotterTransition(t *testing.T) {
	if !transitionEnabled() {
		t.Skipf("%s is not set — skipping the manual transition suite (see README.md)", envRunTransition)
	}
	transitionActive = true

	RegisterFailHandler(Fail)

	suiteConfig, reporterConfig := GinkgoConfiguration()
	suiteConfig.Timeout = 180 * time.Minute
	// The scenario shares one dev cluster and carries a legacy workload across ordered phases
	// (bootstrap -> legacy -> migrate+flip -> invariants), so spec randomization MUST stay OFF.
	suiteConfig.RandomizeAllSpecs = false
	reporterConfig.Verbose = true

	RunSpecs(t, "state-snapshotter transition E2E Suite", suiteConfig, reporterConfig)
}

var _ = BeforeSuite(func() {
	if strings.TrimSpace(os.Getenv("TEST_CLUSTER_CREATE_MODE")) == "" {
		Fail("TEST_CLUSTER_CREATE_MODE must be set: this suite only supports storage-e2e nested clusters")
	}
	// Fail fast before provisioning if any required image-tag var is missing.
	requireEnv(envSnapshotControllerTag, envSvdmLegacyTag, envSvdmTag)

	suiteRes = cluster.CreateOrConnectToTestCluster()
	if suiteRes == nil || suiteRes.Kubeconfig == nil {
		Fail("storage-e2e returned a nil cluster handle")
	}

	var err error
	suiteClientset, err = clientgokube.NewForConfig(suiteRes.Kubeconfig)
	Expect(err).NotTo(HaveOccurred(), "build clientset")
	suiteDyn, err = dynamic.NewForConfig(suiteRes.Kubeconfig)
	Expect(err).NotTo(HaveOccurred(), "build dynamic client")
})

var _ = AfterSuite(func() {
	if suiteRes == nil {
		return
	}
	// Keep the cluster for triage when a spec failed and E2E_KEEP_CLUSTER_ON_FAILURE is set, or
	// always when E2E_KEEP_CLUSTER is set (mirrors the main suite's knobs).
	if envTrue("E2E_KEEP_CLUSTER") || (anySpecFailed && envTrue("E2E_KEEP_CLUSTER_ON_FAILURE")) {
		GinkgoWriter.Printf("keeping nested cluster (failed=%v)\n", anySpecFailed)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := cluster.CleanupTestCluster(ctx, suiteRes); err != nil {
		GinkgoWriter.Printf("warning: nested cluster cleanup failed: %v\n", err)
	}
})

func envTrue(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "true", "1", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// requireEnv fails the suite (before any provisioning) if any of the named env vars is empty.
func requireEnv(names ...string) {
	var missing []string
	for _, n := range names {
		if strings.TrimSpace(os.Getenv(n)) == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		Fail("missing required transition env vars: " + strings.Join(missing, ", ") + " (see README.md)")
	}
}

// enableModule enables (or retags) a single module at runtime via storage-e2e's create-or-update
// ModuleConfig + ModulePullOverride path, then waits for it to become Ready. Re-calling with a
// different imageTag retags the live MPO — that is exactly how the svdm legacy->D1 migration is
// triggered in phase C.
func enableModule(name, imageTag string, deps ...string) {
	GinkgoHelper()
	specs := []storagekube.ModuleSpec{{
		Name:               name,
		Version:            1,
		Enabled:            true,
		ModulePullOverride: imageTag,
		Dependencies:       deps,
	}}
	// res.ClusterDefinition / res.SSHClient carry storage-e2e internal types; they are passed
	// straight through to EnableModulesAndWait without being named here.
	err := storagekube.EnableModulesAndWait(
		suiteCtx(), suiteRes.Kubeconfig, suiteRes.SSHClient, suiteRes.ClusterDefinition, specs, moduleReadyTimeout,
	)
	Expect(err).NotTo(HaveOccurred(), "enable/retag module %s -> %s", name, imageTag)
}

var _ = Describe("state-snapshotter transition e2e", Ordered, func() {
	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			anySpecFailed = true
		}
	})

	// ---- Phase A: bootstrap without the snapshot stack ----
	Context("Phase A: bootstrap without snapshot stack", func() {
		It("starts clean — no snapshot-stack namespaces present", func(ctx SpecContext) {
			// Bootstrap ran in BeforeSuite from cluster_config.yml (only sds-node-configurator
			// enabled; the four snapshot modules preseeded enabled:false). The cluster must be clean
			// before the legacy phase: a dirty cluster (a target module already rolled out) must FAIL
			// here, not continue. A dev build does not enforce requirements, so the preseed
			// enabled:false ModuleConfigs are the only thing keeping them off — verify it held.
			for _, m := range []string{modSnapshotController, modSvdm, modStateSnapshotter, modStorageFoundation} {
				ns := moduleNamespace[m]
				Expect(namespaceExists(ctx, ns)).To(BeFalse(),
					"namespace %s must be absent before the legacy phase (module %s must not be rolled out yet)", ns, m)
			}
		})
	})

	// ---- Phase B: legacy snapshot-controller + svdm ----
	Context("Phase B: legacy stack (old group)", func() {
		It("enables snapshot-controller, svdm(legacy) and sds-local-volume", func() {
			enableModule(modSnapshotController, tagFrom(envSnapshotControllerTag))
			// svdm legacy image (OLD storage.deckhouse.io group). The legacy tag is a scenario var
			// because the standard STORAGE_VOLUME_DATA_MANAGER_MODULE_PULL_OVERRIDE slot is reserved
			// for the D1 image used by the phase-C retag.
			enableModule(modSvdm, tagFrom(envSvdmLegacyTag))
			// sds-local-volume depends on snapshot-controller (v0.4.x) — enable it only now.
			enableModule(modSdsLocalVolume, tagFrom(envSdsLocalVolumeOverride), modSnapshotController)
		})

		It("creates a PVC + pod, writes deterministic data and a CSI VolumeSnapshot", func(ctx SpecContext) {
			if !dataPlaneEnabled() {
				Skip("set " + envStorageClass + " and " + envVSClass + " (snapshot-capable SC + VolumeSnapshotClass) to run the data-plane steps")
			}
			ensureNamespace(ctx, workloadNS)
			DeferCleanup(func(ctx SpecContext) { _ = suiteDyn.Resource(nsGVR).Delete(ctx, workloadNS, metav1.DeleteOptions{}) })

			createPVC(ctx, workloadNS, srcPVCName, os.Getenv(envStorageClass), "1Gi")
			createProbePod(ctx, workloadNS, probePodName, probeImage(), srcPVCName)

			var err error
			sourceChecksum, err = writeMarkerChecksum(ctx, workloadNS, probePodName, "probe", markerPath)
			Expect(err).NotTo(HaveOccurred(), "write+checksum marker")
			Expect(sourceChecksum).NotTo(BeEmpty())

			Expect(createCSIVolumeSnapshot(ctx, workloadNS, vsName, os.Getenv(envVSClass), srcPVCName)).To(Succeed())
			// Do NOT flip before the VS is BOTH readyToUse and bound: an unready VS could be adopted
			// by the new controller after the flip.
			boundContent, err = waitCSIVolumeSnapshotReady(ctx, workloadNS, vsName, 10*time.Minute)
			Expect(err).NotTo(HaveOccurred())
			Expect(boundContent).NotTo(BeEmpty())

			vs, err := getUnstr(ctx, volumeSnapshotGVR, workloadNS, vsName)
			Expect(err).NotTo(HaveOccurred())
			vsUID = string(vs.GetUID())
			Expect(vsUID).NotTo(BeEmpty())
		})

		It("exports the source PVC over the svdm HTTP API and verifies the downloaded checksum", func(ctx SpecContext) {
			if !dataPlaneEnabled() {
				Skip("data-plane steps skipped (see previous spec)")
			}
			// Curl pod + download RBAC (SA token authorizes the dataexports/download subresource).
			ensureDownloadRBAC(ctx, workloadNS, httpClientSA)
			createHTTPClientPod(ctx, workloadNS, httpClientPod, httpClientSA)

			// DataExport the source PVC on the LEGACY group/schema; wait for status.url + status.ca.
			Expect(createLegacyDataExport(ctx, workloadNS, "export-pvc", "PersistentVolumeClaim", srcPVCName)).To(Succeed())
			url, caB64, err := crStatusURLCA(ctx, dataExportGVR(legacyGroup), workloadNS, "export-pvc", 5*time.Minute)
			Expect(err).NotTo(HaveOccurred())
			Expect(url).NotTo(BeEmpty())
			Expect(caB64).NotTo(BeEmpty())

			// Download the marker file (PVC root) and confirm its checksum matches the source.
			Expect(svdmDownload(ctx, workloadNS, url, caB64, "marker", "/tmp/marker")).To(Succeed())
			got, err := checksumFile(ctx, workloadNS, httpClientPod, "curl", "/tmp/marker")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(sourceChecksum), "downloaded marker checksum must match the source")
		})

		It("imports over the svdm HTTP API into a new PVC and verifies the checksum", func(ctx SpecContext) {
			if !dataPlaneEnabled() {
				Skip("data-plane steps skipped (see previous spec)")
			}
			// DataImport (legacy schema, CreatePVC via targetRef.pvcTemplate) → importer publishes url.
			Expect(createLegacyDataImport(ctx, workloadNS, "import-di", "imported-data", os.Getenv(envStorageClass), "1Gi")).To(Succeed())
			url, caB64, err := crStatusURLCA(ctx, dataImportGVR(legacyGroup), workloadNS, "import-di", 5*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			// Upload the previously downloaded marker, signal finished, wait for the import to complete.
			Expect(svdmUpload(ctx, workloadNS, url, caB64, "/tmp/marker", "marker")).To(Succeed())
			Expect(waitCRConditionTrue(ctx, dataImportGVR(legacyGroup), workloadNS, "import-di",
				[]string{"Completed", "Ready"}, 5*time.Minute)).To(Succeed())

			// Mount the imported PVC and confirm the imported marker matches the source.
			createProbePod(ctx, workloadNS, "probe-imported", probeImage(), "imported-data")
			got, err := checksumFile(ctx, workloadNS, "probe-imported", "probe", "/mnt/imported-data/marker")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(sourceChecksum), "imported marker checksum must match the source")
		})

		It("CSI-restores a PVC from the VolumeSnapshot and verifies the data", func(ctx SpecContext) {
			if !dataPlaneEnabled() {
				Skip("data-plane steps skipped (see previous spec)")
			}
			createPVCFromSnapshot(ctx, workloadNS, "restored-pvc", os.Getenv(envStorageClass), vsName, "1Gi")
			createProbePod(ctx, workloadNS, "probe-restored", probeImage(), "restored-pvc")
			got, err := checksumFile(ctx, workloadNS, "probe-restored", "probe", "/mnt/restored-pvc/marker")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(sourceChecksum), "CSI-restored marker checksum must match the source")
		})
	})

	// ---- Phase C: migrate svdm + flip to the new stack ----
	Context("Phase C: migrate svdm and flip", func() {
		It("retags svdm legacy->D1 and verifies the migration hook", func(ctx SpecContext) {
			// Retagging the live svdm MPO to the D1 image runs the OnBeforeHelm migration hook.
			enableModule(modSvdm, tagFrom(envSvdmTag))

			// The legacy CRDs must be gone (the migration hook deletes them after migrating CRs),
			// and nothing must be stuck Terminating.
			Eventually(func(ctx SpecContext) bool {
				return crdExists(ctx, "dataexports."+legacyGroup) || crdExists(ctx, "dataimports."+legacyGroup)
			}).WithContext(ctx).WithTimeout(5*time.Minute).WithPolling(pollInterval).Should(BeFalse(),
				"legacy CRDs dataexports/dataimports.%s must be removed by the migration hook", legacyGroup)

			// The unified-group CRDs must be present (installed by the D1 svdm bundle).
			Expect(crdExists(ctx, "dataexports."+unifiedGroup)).To(BeTrue())
			Expect(crdExists(ctx, "dataimports."+unifiedGroup)).To(BeTrue())

			// The legacy finalizer must be swept off every PVC (else deletes would hang forever).
			leftover, err := pvcsWithFinalizer(ctx, legacyFinalizer)
			Expect(err).NotTo(HaveOccurred())
			Expect(leftover).To(BeEmpty(), "legacy finalizer %s must be swept off all PVCs", legacyFinalizer)
		})

		It("enables state-snapshotter -> storage-foundation without disabling the legacy modules", func(ctx SpecContext) {
			enableModule(modStateSnapshotter, tagFrom(envStateSnapshotterOverride))
			enableModule(modStorageFoundation, tagFrom(envStorageFoundationOverride), modStateSnapshotter)

			// The legacy ModuleConfigs stay enabled:true — the test verifies Helm GUARDS, not
			// uninstall. Once storage-foundation is enabled, snapshot-controller/svdm must render no
			// workload (Deployments/Services): only their deprecation PrometheusRule may remain.
			for _, m := range []string{modSnapshotController, modSvdm} {
				ns := moduleNamespace[m]
				Eventually(func(ctx SpecContext) (int, error) {
					return workloadResourceCount(ctx, ns)
				}).WithContext(ctx).WithTimeout(10*time.Minute).WithPolling(pollInterval).Should(Equal(0),
					"module %s must render no Deployments/Services once storage-foundation is enabled (guard)", m)
			}
		})
	})

	// ---- Phase D: invariants after the flip ----
	Context("Phase D: invariants after the flip", func() {
		It("keeps the CSI CRDs and the legacy VolumeSnapshot intact", func(ctx SpecContext) {
			Expect(crdExists(ctx, "volumesnapshots.snapshot.storage.k8s.io")).To(BeTrue())
			Expect(crdExists(ctx, "volumesnapshotcontents.snapshot.storage.k8s.io")).To(BeTrue())

			if !dataPlaneEnabled() {
				Skip("data-plane invariants skipped (no SC/VSC provided)")
			}
			vs, err := getUnstr(ctx, volumeSnapshotGVR, workloadNS, vsName)
			Expect(err).NotTo(HaveOccurred())
			// Same object: UID unchanged, still ready+bound to the same content.
			Expect(string(vs.GetUID())).To(Equal(vsUID), "legacy VolumeSnapshot UID must not change across the flip")
			ready, _, _ := unstructured.NestedBool(vs.Object, "status", "readyToUse")
			Expect(ready).To(BeTrue())
			content, _, _ := unstructured.NestedString(vs.Object, "status", "boundVolumeSnapshotContentName")
			Expect(content).To(Equal(boundContent))
			// The legacy VS must NOT be adopted into the new domain.
			labels := vs.GetLabels()
			Expect(labels).NotTo(HaveKey("state-snapshotter.deckhouse.io/managed"))
			Expect(labels).NotTo(HaveKey("storage-foundation.deckhouse.io/processed"))

			// Source data still intact after the flip.
			got, err := checksumFile(ctx, workloadNS, probePodName, "probe", markerPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(sourceChecksum), "source PVC data must survive the flip unchanged")
		})

		It("still CSI-restores from the legacy VolumeSnapshot after the flip", func(ctx SpecContext) {
			if !dataPlaneEnabled() {
				Skip("data-plane invariants skipped (no SC/VSC provided)")
			}
			// The legacy plain-CSI snapshot must remain restorable under the new stack.
			createPVCFromSnapshot(ctx, workloadNS, "restored-postflip", os.Getenv(envStorageClass), vsName, "1Gi")
			createProbePod(ctx, workloadNS, "probe-postflip", probeImage(), "restored-postflip")
			got, err := checksumFile(ctx, workloadNS, "probe-postflip", "probe", "/mnt/restored-postflip/marker")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(sourceChecksum), "post-flip CSI restore from the legacy VS must match the source")
		})

		It("drives a fresh CSI VolumeSnapshot through the new controller", func(ctx SpecContext) {
			if !dataPlaneEnabled() {
				Skip("data-plane invariants skipped (no SC/VSC provided)")
			}
			// A brand-new PVC + CSI VolumeSnapshot created after the flip must reach ready+bound —
			// i.e. the storage-foundation snapshot-controller now services CSI snapshots.
			createPVC(ctx, workloadNS, "new-pvc", os.Getenv(envStorageClass), "1Gi")
			createProbePod(ctx, workloadNS, "probe-new", probeImage(), "new-pvc")
			_, err := writeMarkerChecksum(ctx, workloadNS, "probe-new", "probe", "/mnt/new-pvc/marker")
			Expect(err).NotTo(HaveOccurred())
			Expect(createCSIVolumeSnapshot(ctx, workloadNS, "new-snap", os.Getenv(envVSClass), "new-pvc")).To(Succeed())
			content, err := waitCSIVolumeSnapshotReady(ctx, workloadNS, "new-snap", 10*time.Minute)
			Expect(err).NotTo(HaveOccurred())
			Expect(content).NotTo(BeEmpty(), "new CSI VolumeSnapshot must be serviced by storage-foundation after the flip")

			// NOTE(first-run): the deeper unified-domain path (state-snapshotter Snapshot with
			// processed/managed labels + SnapshotContent + new-group DataExport/DataImport) is a
			// larger domain-specific surface; validate it via the existing state-snapshotter e2e
			// suite on the same cluster once the transition suite passes.
		})
	})

	AfterAll(func(ctx SpecContext) {
		// Best-effort teardown of the workload namespace (module teardown is handled in AfterSuite).
		if namespaceExists(ctx, workloadNS) {
			_ = suiteDyn.Resource(nsGVR).Delete(ctx, workloadNS, metav1.DeleteOptions{})
		}
	})
})

// suiteCtx returns a background context for module operations. Kept as a helper so a per-op timeout
// can be threaded in later without touching call sites.
func suiteCtx() context.Context { return context.Background() }
