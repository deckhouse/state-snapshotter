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
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
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
	// main suite's cluster_config).
	//   - snapshot-controller: ONE tag. Its single deprecated v0.2.0 build has no storage-foundation
	//     requirement (only deckhouse >= 1.76), so it installs standalone in phase B and ships the
	//     extended (storage-foundation) CRDs — no legacy/handoff split, no phase-C retag.
	//   - svdm: two slots — the legacy old-group image (phase B) and the v0.2.0/D1 new-group image the
	//     phase-C migration retags to.
	//   - sds-local-volume: two slots as well. Its current build depends on storage-foundation (absent
	//     in phase B), so phase B enables a LEGACY image that depends on snapshot-controller
	//     (E2E_TRANSITION_SDS_LOCAL_VOLUME_LEGACY_TAG) and phase C retags it to the storage-foundation-
	//     integrated build (the standard SDS_LOCAL_VOLUME_MODULE_PULL_OVERRIDE) after the flip.
	envSnapshotControllerTag   = "E2E_TRANSITION_SNAPSHOT_CONTROLLER_TAG"
	envSvdmLegacyTag           = "E2E_TRANSITION_SVDM_LEGACY_TAG"
	envSvdmTag                 = "E2E_TRANSITION_SVDM_TAG"
	envSdsLocalVolumeLegacyTag = "E2E_TRANSITION_SDS_LOCAL_VOLUME_LEGACY_TAG"

	// Standard storage-e2e <MODULE>_MODULE_PULL_OVERRIDE vars for modules the test enables at
	// runtime (they are preseeded disabled in cluster_config.yml, so bootstrap does not read them).
	// For sds-local-volume this is the phase-C (storage-foundation-integrated) target the flip retags
	// to; its phase-B legacy image comes from envSdsLocalVolumeLegacyTag above.
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

	// two-owner migration-race fixtures (phase-C race leg; see the race section in helpers_test.go).
	// raceImportName is the ACTIVE legacy DataImport re-seeded before the flip; raceImportPVCName is
	// the target PVC its pvcTemplate names; racePVCName is the PVC seeded with the legacy finalizer.
	raceImportName    = "race-import"
	raceImportPVCName = "race-imported-data"
	racePVCName       = "race-pvc"
)

var markerPath = "/mnt/" + srcPVCName + "/marker"

// sdsLocalVolumePhaseCTag holds the phase-C (storage-foundation-integrated) sds-local-volume tag,
// captured in BeforeSuite before SDS_LOCAL_VOLUME_MODULE_PULL_OVERRIDE is repointed at the legacy
// tag for phase B (so the storage-e2e testkit's lazy sds-local-volume enable uses the legacy image).
// The phase-C retag restores the var to this value and retags to it.
var sdsLocalVolumePhaseCTag string

var (
	suiteRes         *cluster.TestClusterResources
	anySpecFailed    bool
	transitionActive bool

	// fixture carried across phases
	sourceChecksum string
	vsName         = "legacy-snap"
	boundContent   string
	vsUID          string

	// Shared CRDs whose identity must survive the flip (updated in place during the ownership
	// handoff, never delete+recreated). UIDs are captured just before storage-foundation is enabled
	// and re-checked in phase D. CSI CRDs are installed by snapshot-controller (phase B); the unified
	// DataExport/DataImport CRDs by svdm-D1 (phase-C retag) — sf then re-applies both byte-for-byte.
	csiCRDNames = []string{
		"volumesnapshots.snapshot.storage.k8s.io",
		"volumesnapshotcontents.snapshot.storage.k8s.io",
		"volumesnapshotclasses.snapshot.storage.k8s.io",
	}
	unifiedCRDNames = []string{
		"dataexports.storage-foundation.deckhouse.io",
		"dataimports.storage-foundation.deckhouse.io",
	}
	crdUIDBeforeFlip = map[string]string{}
)

// trackedCRDs is every CRD whose identity the flip must preserve (CSI + unified).
func trackedCRDs() []string { return append(append([]string{}, csiCRDNames...), unifiedCRDNames...) }

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
	// Validate the module image-tag env vars first (before any provisioning): every one that is set
	// must be a plain-ASCII tag matching mr<N>/pr<N>/main. This catches a prod v* tag (absent from
	// the dev registry the nested cluster pulls) and — the real footgun — a tag typed in a non-Latin
	// keyboard layout (e.g. the Cyrillic "ает" instead of "main"), which otherwise only surfaces
	// minutes later as a wedged converge in a mid-run phase.
	validateModuleTagEnvVars()

	if strings.TrimSpace(os.Getenv("TEST_CLUSTER_CREATE_MODE")) == "" {
		Fail("TEST_CLUSTER_CREATE_MODE must be set: this suite only supports storage-e2e nested clusters")
	}
	// Fail fast before provisioning if any required image-tag var is missing.
	requireEnv(envSnapshotControllerTag, envSvdmLegacyTag, envSvdmTag)
	// sds-local-volume is only enabled for the data-plane steps, and its phase-B legacy image has no
	// safe default ("main" now depends on storage-foundation, which is disabled in phase B), so it is
	// required only when the data plane is exercised.
	if dataPlaneEnabled() {
		requireEnv(envSdsLocalVolumeLegacyTag)
		// The storage-e2e testkit (EnsureDefaultStorageClass, phase B) ALSO enables sds-local-volume
		// and reads its tag from SDS_LOCAL_VOLUME_MODULE_PULL_OVERRIDE — so if that var held the
		// phase-C (storage-foundation-dependent) tag, the testkit would retag sds-local-volume to it
		// mid-phase-B and Deckhouse would deny it ("dependency 'storage-foundation' is disabled").
		// Repoint the var at the legacy tag for the whole legacy phase; capture the phase-C target
		// first and restore+retag to it after the flip (phase C).
		sdsLocalVolumePhaseCTag = tagFrom(envSdsLocalVolumeOverride)
		Expect(os.Setenv(envSdsLocalVolumeOverride, tagFrom(envSdsLocalVolumeLegacyTag))).To(Succeed(),
			"repoint %s at the legacy tag for phase B", envSdsLocalVolumeOverride)
	}

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

// moduleTagPattern is the set of image tags this suite accepts for its module ModulePullOverrides:
// GitLab MR builds (mr<IID>), GitHub PR builds (pr<N>), or the main build. The nested cluster pulls
// module images from the DEV registry, where builds land under exactly these tags — a prod v* tag
// is not there, so it is rejected on purpose (fail fast, not on a later image pull).
var moduleTagPattern = regexp.MustCompile(`^(mr[0-9]+|pr[0-9]+|main)$`)

// moduleTagEnvVars are the image-tag / ModulePullOverride env vars the transition suite consumes.
var moduleTagEnvVars = []string{
	envSnapshotControllerTag,
	envSvdmLegacyTag,
	envSvdmTag,
	envSdsLocalVolumeLegacyTag,
	envSdsLocalVolumeOverride,
	envStateSnapshotterOverride,
	envStorageFoundationOverride,
	"SDS_NODE_CONFIGURATOR_MODULE_PULL_OVERRIDE",
}

// isASCII reports whether s contains only ASCII bytes. A value typed in a non-Latin keyboard layout
// (e.g. the Cyrillic "ает") carries multi-byte UTF-8 runes and fails this check.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// validateModuleTagEnvVars fails the suite up front if any SET module image-tag env var is not a
// plain-ASCII tag matching mr<N>/pr<N>/main. Presence of required vars is enforced separately by
// requireEnv; unset optional vars default to "main" via tagFrom and are skipped here.
func validateModuleTagEnvVars() {
	var problems []string
	for _, name := range moduleTagEnvVars {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			continue
		}
		switch {
		case !isASCII(v):
			problems = append(problems, fmt.Sprintf("%s=%q contains non-ASCII characters — check the keyboard layout (a Latin tag typed in another layout?)", name, v))
		case !moduleTagPattern.MatchString(v):
			problems = append(problems, fmt.Sprintf("%s=%q must match one of: mr<N>, pr<N>, main (dev-registry image tags)", name, v))
		}
	}
	if len(problems) > 0 {
		Fail("invalid module image-tag env var(s):\n  - " + strings.Join(problems, "\n  - "))
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
// moduleSpec builds a ModuleSpec (enabled, chart version 1) with an image tag and optional
// dependencies. IMPORTANT: a dependency name must refer to another module passed in the SAME
// enableModules() call — the storage-e2e graph builder resolves dependencies only within the
// provided spec set, so a cross-call dependency fails with "dependency module ... not found".
// Batch co-dependent modules together instead of enabling them one-by-one.
func moduleSpec(name, imageTag string, deps ...string) storagekube.ModuleSpec {
	return storagekube.ModuleSpec{
		Name:               name,
		Version:            1,
		Enabled:            true,
		ModulePullOverride: imageTag,
		Dependencies:       deps,
	}
}

// enableModules enables (or retags) the given modules in ONE EnableModulesAndWait call, so the
// framework builds their dependency graph and brings them up together — independent modules
// concurrently, dependents after their dependencies — then waits for all of them to be Ready.
// res.ClusterDefinition / res.SSHClient carry storage-e2e internal types passed straight through.
func enableModules(specs ...storagekube.ModuleSpec) {
	GinkgoHelper()
	err := storagekube.EnableModulesAndWait(
		suiteCtx(), suiteRes.Kubeconfig, suiteRes.SSHClient, suiteRes.ClusterDefinition, specs, moduleReadyTimeout,
	)
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name
	}
	Expect(err).NotTo(HaveOccurred(), "enable/retag modules %v", names)
}

// enableModule enables (or retags) a single module with no cross-module dependency. To enable a
// module together with a dependency, batch them via enableModules(moduleSpec(...), ...).
func enableModule(name, imageTag string) {
	GinkgoHelper()
	enableModules(moduleSpec(name, imageTag))
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

			// The current snapshot-controller v0.2.0 build has NO storage-foundation requirement, so it
			// installs standalone. But a cluster reused from BEFORE that change may still be REGISTERED
			// as an older, sf-gated snapshot-controller build (Deckhouse ignores an MPO while a module
			// is disabled, so it stays frozen on the gated version). That would webhook-deny the phase-B
			// enable ("depends on disabled module(s): storage-foundation"). Fail here with an actionable
			// message instead of a cryptic mid-B failure. Reset per README ("Resetting a reused cluster").
			Expect(moduleRequiresModule(ctx, modSnapshotController, modStorageFoundation)).To(BeFalse(),
				"cluster is contaminated: snapshot-controller is registered as an older build that requires "+
					"storage-foundation (frozen from a prior run); the current v0.2.0 build drops that requirement, "+
					"but the stale registration blocks the phase-B enable. Reset it — see README 'Resetting a reused cluster'.")
		})
	})

	// ---- Phase B: legacy snapshot-controller + svdm ----
	Context("Phase B: legacy stack (old group)", func() {
		It("enables snapshot-controller, svdm(legacy) and sds-local-volume", func() {
			// One batch: the framework brings snapshot-controller and svdm up concurrently (no
			// interdependency) and sds-local-volume after snapshot-controller (its legacy dependency,
			// declared in-batch so the graph resolves — a separate call would fail graph-build).
			// snapshot-controller runs its single deprecated v0.2.0 build (E2E_TRANSITION_SNAPSHOT_CONTROLLER_TAG):
			// no storage-foundation requirement, so it installs standalone here and ships the extended
			// (storage-foundation) CRDs — the "vanilla controller + extended CRDs" combination the next
			// spec verifies. svdm runs its legacy old-group image (E2E_TRANSITION_SVDM_LEGACY_TAG); the
			// phase-C migration retags it to the D1 image.
			specs := []storagekube.ModuleSpec{
				moduleSpec(modSnapshotController, tagFrom(envSnapshotControllerTag)),
				moduleSpec(modSvdm, tagFrom(envSvdmLegacyTag)),
			}
			// sds-local-volume is the CSI backend for the data-plane steps only, so enable it just for
			// those runs. Use its LEGACY image (E2E_TRANSITION_SDS_LOCAL_VOLUME_LEGACY_TAG), which
			// depends on snapshot-controller: the current build depends on storage-foundation, disabled
			// in phase B, and would be webhook-denied ("dependency 'storage-foundation' is disabled").
			// Phase C retags it to the storage-foundation-integrated build after the flip.
			if dataPlaneEnabled() {
				specs = append(specs, moduleSpec(modSdsLocalVolume, tagFrom(envSdsLocalVolumeLegacyTag), modSnapshotController))
			}
			enableModules(specs...)
		})

		It("creates a PVC + pod, writes deterministic data and a CSI VolumeSnapshot", func(ctx SpecContext) {
			if !dataPlaneEnabled() {
				Skip("set " + envStorageClass + " and " + envVSClass + " (snapshot-capable SC + VolumeSnapshotClass) to run the data-plane steps")
			}
			// Provision the thin StorageClass + VolumeSnapshotClass (idempotent): a no-op on a cluster
			// that already has them, full LVM-backend provisioning on a fresh alwaysCreateNew cluster.
			// Runs here (not phase A) because it needs sds-local-volume Ready, enabled just above.
			ensureDataPlaneStorage(ctx)
			// The namespace + workload (source PVC, VS, imported/restored PVCs, curl pod) must survive
			// across every phase-B/C/D spec of this Ordered scenario. Do NOT DeferCleanup it here — a
			// spec-scoped DeferCleanup runs after THIS spec and would delete it before the next one
			// (the next spec then hits "namespace is being terminated"). AfterAll tears it down.
			ensureNamespace(ctx, workloadNS)

			createPVC(ctx, workloadNS, srcPVCName, os.Getenv(envStorageClass), "1Gi")
			createProbePod(ctx, probePodName, probeImage(), srcPVCName)

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

			// "Old controller + new CRDs" check. The deprecated snapshot-controller bundles the vanilla
			// upstream external-snapshotter but ships the EXTENDED storage-foundation CRDs. Assert the
			// served VolumeSnapshot CRD carries spec.mode (so it is the extended schema, not vanilla) and
			// that the API server defaulted this user-created VS to mode=Capture — and note that the VS
			// reached readyToUse+bound above, which proves the vanilla controller reconciled a
			// Capture-mode snapshot against the extended CRD (it ignores the unknown spec.mode field).
			Expect(crdSchemaHasField(ctx, "volumesnapshots.snapshot.storage.k8s.io", "spec", "mode")).To(BeTrue(),
				"snapshot-controller must ship the extended VolumeSnapshot CRD (spec.mode) in phase B")
			mode, _, _ := unstructured.NestedString(vs.Object, "spec", "mode")
			Expect(mode).To(Equal("Capture"),
				"the extended CRD must default spec.mode=Capture, and the vanilla controller must still bind such a VS")
		})

		It("exports the source PVC over the svdm HTTP API and verifies the downloaded checksum", func(ctx SpecContext) {
			if !dataPlaneEnabled() {
				Skip("data-plane steps skipped (see previous spec)")
			}
			// Curl pod + download RBAC (SA token authorizes the dataexports/download subresource).
			ensureDownloadRBAC(ctx, workloadNS, httpClientSA)
			createHTTPClientPod(ctx, workloadNS, httpClientPod, httpClientSA)

			// svdm's PVC export reassigns the PV to an export PVC and rejects a source PVC that is
			// still mounted ("user's PVC isn't free because it's being occupied by pods probe"). The
			// probe pod that wrote the marker still holds src-data — delete it (and wait) before export.
			// The marker checksum is already captured (sourceChecksum) and the CSI VolumeSnapshot is
			// bound, so the source pod is no longer needed.
			deletePodAndWait(ctx, workloadNS, probePodName, 2*time.Minute)

			// DataExport the source PVC on the LEGACY group/schema; wait for status.url + status.ca.
			Expect(createLegacyDataExport(ctx, workloadNS, "export-pvc", "PersistentVolumeClaim", srcPVCName)).To(Succeed())
			url, caB64, err := crStatusURLCA(ctx, dataExportGVR(legacyGroup), "export-pvc")
			Expect(err).NotTo(HaveOccurred())
			Expect(url).NotTo(BeEmpty())
			Expect(caB64).NotTo(BeEmpty())

			// Download the marker file (PVC root) and confirm its checksum matches the source.
			Expect(svdmDownload(ctx, workloadNS, url, caB64, "marker", "/tmp/marker")).To(Succeed())
			got, err := checksumFile(ctx, httpClientPod, "curl", "/tmp/marker")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(sourceChecksum), "downloaded marker checksum must match the source")
		})

		It("imports over the svdm HTTP API into a new PVC and verifies the checksum", func(ctx SpecContext) {
			if !dataPlaneEnabled() {
				Skip("data-plane steps skipped (see previous spec)")
			}

			By("creating the legacy DataImport and waiting for the importer to publish status.url")
			// DataImport (legacy schema, CreatePVC via targetRef.pvcTemplate) → importer publishes url.
			Expect(createLegacyDataImport(ctx, workloadNS, "import-di", "imported-data", os.Getenv(envStorageClass), "1Gi")).To(Succeed())
			url, caB64, err := crStatusURLCA(ctx, dataImportGVR(legacyGroup), "import-di")
			Expect(err).NotTo(HaveOccurred())

			By("uploading the marker over the svdm HTTP API and signalling finished")
			Expect(svdmUpload(ctx, workloadNS, url, caB64, "/tmp/marker", "marker")).To(Succeed())
			logf("upload + POST finished done; DataImport conditions: %s", crConditions(ctx, dataImportGVR(legacyGroup), workloadNS, "import-di"))

			By("waiting for the populator to rebind the prime volume onto imported-data (PVC Bound)")
			// Import completion = the target PVC becoming Bound: the DataImport Ready condition flips
			// True early (server ready) and there is no Completed condition type, so the PVC phase is
			// the real gate. waitImportComplete narrates DI conditions / prime PVC / pods every 15s so a
			// stall is visible; podRunningTimeout() budgets the whole chain.
			waitImportComplete(ctx, legacyGroup, workloadNS, "import-di", "imported-data", podRunningTimeout())

			By("mounting imported-data and verifying the checksum")
			createProbePod(ctx, "probe-imported", probeImage(), "imported-data")
			got, err := checksumFile(ctx, "probe-imported", "probe", "/mnt/imported-data/marker")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(sourceChecksum), "imported marker checksum must match the source")
		})

		It("CSI-restores a PVC from the VolumeSnapshot and verifies the data", func(ctx SpecContext) {
			if !dataPlaneEnabled() {
				Skip("data-plane steps skipped (see previous spec)")
			}
			createPVCFromSnapshot(ctx, workloadNS, "restored-pvc", os.Getenv(envStorageClass), vsName, "1Gi")
			createProbePod(ctx, "probe-restored", probeImage(), "restored-pvc")
			got, err := checksumFile(ctx, "probe-restored", "probe", "/mnt/restored-pvc/marker")
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

			if !dataPlaneEnabled() {
				return
			}
			// The in-flight legacy DataExport (export-pvc, still serving from phase B) must be MIGRATED
			// onto the unified group with its spec mapped (targetRef kind+name preserved) — not dropped.
			// Poll: migration runs OnBeforeHelm and the unified CR appears a moment after the retag.
			expGVR := dataExportGVR(unifiedGroup)
			pollUntil(ctx, "in-flight DataExport export-pvc migrated to the unified group", 3*time.Minute,
				func() bool { _, e := getUnstr(ctx, expGVR, workloadNS, "export-pvc"); return e == nil },
				func() string { return "not on unified group yet" })
			migrated, err := getUnstr(ctx, expGVR, workloadNS, "export-pvc")
			Expect(err).NotTo(HaveOccurred())
			kind, _, _ := unstructured.NestedString(migrated.Object, "spec", "targetRef", "kind")
			tname, _, _ := unstructured.NestedString(migrated.Object, "spec", "targetRef", "name")
			Expect(kind).To(Equal("PersistentVolumeClaim"), "migrated DataExport must keep its targetRef.kind")
			Expect(tname).To(Equal(srcPVCName), "migrated DataExport must keep its targetRef.name")
		})

		It("serves a fresh new-group DataExport standalone and cleans up the migrated one (before the flip)", func(ctx SpecContext) {
			if !dataPlaneEnabled() {
				Skip("data-plane steps skipped (see phase B)")
			}
			// (a) svdm-D1 STANDALONE (storage-foundation still OFF) must serve a brand-new export on
			// the unified group — proving the D1 controller works on the new group, not only that the
			// migration hook ran. Export restored-pvc (it holds the marker from the phase-B CSI restore
			// and is unused later); free it first — svdm rejects exporting a mounted PVC.
			deletePodAndWait(ctx, workloadNS, "probe-restored", 2*time.Minute)
			ensureDownloadRBAC(ctx, workloadNS, httpClientSA)
			createHTTPClientPod(ctx, workloadNS, httpClientPod, httpClientSA)
			Expect(createUnifiedDataExport(ctx, workloadNS, "export-d1", "PersistentVolumeClaim", "restored-pvc")).To(Succeed())
			url, caB64, err := crStatusURLCA(ctx, dataExportGVR(unifiedGroup), "export-d1")
			Expect(err).NotTo(HaveOccurred())
			Expect(svdmDownload(ctx, workloadNS, url, caB64, "marker", "/tmp/marker-d1")).To(Succeed())
			got, err := checksumFile(ctx, httpClientPod, "curl", "/tmp/marker-d1")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(sourceChecksum), "svdm-D1 standalone must serve the new-group export")
			deleteCRAndWaitGone(ctx, dataExportGVR(unifiedGroup), "export-d1")

			// (b) Tear down the MIGRATED in-flight export under the D1 controller (still standalone), so
			// no live export is carried across the flip. Deleting it must remove the CR (finalizer
			// released) AND recover the source PVC: svdm restores the reassigned PV, so src-data must
			// return from Lost to Bound. This is the clean-teardown proof, not a lingering artifact.
			deleteCRAndWaitGone(ctx, dataExportGVR(unifiedGroup), "export-pvc")
			waitPVCPhase(ctx, workloadNS, srcPVCName, corev1.ClaimBound, 3*time.Minute)
		})

		It("re-seeds a legacy epoch (CRDs + active DataImport + PVC finalizer) to stage the two-owner migration race", func(ctx SpecContext) {
			// The svdm-D1 025-migrate-legacy-crds hook is mirrored by an identical hook in
			// storage-foundation (same OnBeforeHelm order, idempotent, gated on legacy-CRD presence):
			// whichever module converges first migrates, the other must cleanly no-op. The first
			// phase-C spec exercised the svdm hook alone; this leg re-creates the legacy epoch RIGHT
			// BEFORE the flip, because enabling storage-foundation triggers a global converge that
			// re-runs svdm's beforeHelm hooks too — so BOTH owners see the legacy CRDs in the same
			// converge window. Which one wins varies per run; the next spec asserts the outcome
			// winner-agnostically. Both hooks are transitional/expiring — drop this leg with them.
			Expect(crdExists(ctx, "dataexports."+legacyGroup)).To(BeFalse(),
				"the real legacy epoch must already be migrated before re-seeding the race one")
			Expect(crdExists(ctx, "dataimports."+legacyGroup)).To(BeFalse(),
				"the real legacy epoch must already be migrated before re-seeding the race one")

			By("installing minimal legacy dataexports/dataimports CRDs (stand-ins for the pre-D1 svdm CRDs)")
			createLegacyCRD(ctx, "DataExport", "dataexports")
			createLegacyCRD(ctx, "DataImport", "dataimports")

			// An ACTIVE legacy DataImport (no status yet => active): the winner must RE-CREATE it under
			// the unified group with the spec mapped, not just delete it with the CRD. The legacy
			// finalizer is seeded by hand — the pre-D1 controller that used to set it is gone.
			By("creating an active legacy DataImport carrying the legacy finalizer")
			ensureNamespace(ctx, workloadNS)
			Expect(createLegacyDataImport(ctx, workloadNS, raceImportName, raceImportPVCName,
				os.Getenv(envStorageClass), "1Gi")).To(Succeed())
			addFinalizer(ctx, dataImportGVR(legacyGroup), workloadNS, raceImportName, legacyFinalizer)

			// A PVC stuck with the legacy finalizer (storageClassName "" => stays Pending, so this leg
			// runs with or without the data-plane env). The sweep must strip exactly the legacy
			// finalizer; kubernetes.io/pvc-protection stays.
			By("creating a PVC carrying the legacy finalizer")
			createPVC(ctx, workloadNS, racePVCName, "", "1Gi")
			addFinalizer(ctx, pvcGVR, workloadNS, racePVCName, legacyFinalizer)
		})

		It("enables state-snapshotter -> storage-foundation without disabling the legacy modules", func(ctx SpecContext) {
			// Capture the shared CRD UIDs RIGHT BEFORE sf is enabled. sf re-applies the CSI and unified
			// CRDs (byte-for-byte copies) as it takes ownership; the flip must UPDATE them in place, so
			// their UIDs must be unchanged in phase D. A changed UID = delete+recreate = every
			// VolumeSnapshot/DataExport instance cascade-deleted.
			for _, n := range trackedCRDs() {
				u, err := crdUID(ctx, n)
				Expect(err).NotTo(HaveOccurred(), "read CRD %s UID before the flip", n)
				crdUIDBeforeFlip[n] = u
			}

			// One batch: state-snapshotter first, then storage-foundation (its state-snapshotter
			// dependency declared in-batch so the graph resolves — a separate call would fail
			// graph-build with "dependency module state-snapshotter not found").
			enableModules(
				moduleSpec(modStateSnapshotter, tagFrom(envStateSnapshotterOverride)),
				moduleSpec(modStorageFoundation, tagFrom(envStorageFoundationOverride), modStateSnapshotter),
			)

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

		It("migrates the re-seeded legacy epoch through the two-owner race and stays converged", func(ctx SpecContext) {
			// The flip in the previous spec ran the race: enabling storage-foundation triggered a
			// global converge in which BOTH 025 hooks (svdm-D1 and storage-foundation) executed
			// against the legacy epoch seeded two specs ago. Which hook won varies per run — every
			// assertion below is winner-agnostic: it checks the shared migration contract's OUTCOME
			// plus the loser's clean no-op (module health + outcome stability).
			By("waiting for both legacy CRDs to be removed by whichever hook won the race")
			pollUntil(ctx, "legacy CRDs removed by the migration race", 5*time.Minute,
				func() bool {
					return !crdExists(ctx, "dataexports."+legacyGroup) && !crdExists(ctx, "dataimports."+legacyGroup)
				},
				func() string {
					return fmt.Sprintf("dataexports present=%v dataimports present=%v",
						crdExists(ctx, "dataexports."+legacyGroup), crdExists(ctx, "dataimports."+legacyGroup))
				})

			By("asserting the active DataImport was re-created under the unified group with the mapped spec")
			impGVR := dataImportGVR(unifiedGroup)
			pollUntil(ctx, "race DataImport re-created under the unified group", 3*time.Minute,
				func() bool { _, e := getUnstr(ctx, impGVR, workloadNS, raceImportName); return e == nil },
				func() string { return "not on the unified group yet" })
			migrated, err := getUnstr(ctx, impGVR, workloadNS, raceImportName)
			Expect(err).NotTo(HaveOccurred())
			mode, _, _ := unstructured.NestedString(migrated.Object, "spec", "mode")
			Expect(mode).To(Equal("CreatePVC"), "migrated DataImport must get spec.mode=CreatePVC")
			tmpl, found, _ := unstructured.NestedMap(migrated.Object, "spec", "pvcTemplate")
			Expect(found).To(BeTrue(), "migrated DataImport must carry spec.pvcTemplate (hoisted out of targetRef)")
			tmplName, _, _ := unstructured.NestedString(tmpl, "metadata", "name")
			Expect(tmplName).To(Equal(raceImportPVCName), "pvcTemplate must be carried over verbatim")
			_, found, _ = unstructured.NestedMap(migrated.Object, "spec", "targetRef")
			Expect(found).To(BeFalse(), "migrated DataImport must not carry the legacy spec.targetRef")
			Expect(migrated.GetFinalizers()).NotTo(ContainElement(legacyFinalizer),
				"the unified counterpart must not inherit the legacy finalizer")
			migratedUID := string(migrated.GetUID())

			By("asserting the legacy finalizer was swept off every PVC")
			leftover, err := pvcsWithFinalizer(ctx, legacyFinalizer)
			Expect(err).NotTo(HaveOccurred())
			Expect(leftover).To(BeEmpty(), "legacy finalizer %s must be swept off all PVCs", legacyFinalizer)

			By("asserting both owners stayed Ready after the race (the loser's no-op must not error-loop)")
			for _, m := range []string{modSvdm, modStorageFoundation} {
				Expect(storagekube.WaitForModuleReady(suiteCtx(), suiteRes.Kubeconfig, m, 3*time.Minute)).To(Succeed(),
					"module %s must stay Ready after the migration race", m)
			}

			// Follow-up converges are no-ops. The flip itself already re-converged both modules
			// several times after the migration (module Ready transitions, the Helm-guard re-render of
			// the legacy modules) — each re-run saw no legacy CRDs and had to no-op. Hold the outcome
			// stable for another window: the CRDs must stay gone and the migrated CR must keep its UID
			// (a re-run that wrongly re-migrated would delete/recreate or duplicate it).
			By("holding the migrated state stable (no-op on repeated converges)")
			Consistently(func() bool {
				if crdExists(ctx, "dataexports."+legacyGroup) || crdExists(ctx, "dataimports."+legacyGroup) {
					return false
				}
				cur, gerr := getUnstr(ctx, impGVR, workloadNS, raceImportName)
				return gerr == nil && string(cur.GetUID()) == migratedUID
			}).WithTimeout(45*time.Second).WithPolling(5*time.Second).Should(BeTrue(),
				"legacy CRDs must stay gone and the migrated DataImport must keep its identity")

			By("deleting the swept PVC — it must go away cleanly, not hang in Terminating")
			deletePVCAndWaitGone(ctx, workloadNS, racePVCName, 2*time.Minute)

			// Teardown so no in-flight import crosses into phase D: the CR first (the controller
			// releases its finalizers), then the import target PVC the controller may have created
			// from pvcTemplate (it survives the CR by design — it is the import's product).
			By("tearing down the migrated DataImport and its target PVC")
			deleteCRAndWaitGone(ctx, impGVR, raceImportName)
			deletePVCAndWaitGone(ctx, workloadNS, raceImportPVCName, 2*time.Minute)
		})

		It("fires the deprecation alerts for both legacy modules", func(ctx SpecContext) {
			// Both legacy modules are now Deprecated: snapshot-controller since phase B (its single
			// v0.2.0 build is Deprecated and needs no retag — it never required storage-foundation), and
			// svdm since the phase-C retag. Deckhouse must surface, for EACH module, two firing
			// ClusterAlerts:
			//   - built-in ModuleIsDeprecated{module=<name>} — proves module.yaml stage=Deprecated took
			//     effect;
			//   - custom D8<Name>ModuleDeprecated (vector(1), severity 9) — proves the deprecation-alert
			//     template renders (svdm's under the reverse "sf enabled" guard, snapc's always-on).
			// Alert eval lags a scrape, so expectAlertFiring waits at the package-level alertTimeout.
			expectAlertFiring(ctx, "ModuleIsDeprecated", modSnapshotController)
			expectAlertFiring(ctx, "D8SnapshotControllerModuleDeprecated", "")
			expectAlertFiring(ctx, "ModuleIsDeprecated", modSvdm)
			expectAlertFiring(ctx, "D8StorageVolumeDataManagerModuleDeprecated", "")
		})

		It("retags sds-local-volume to the storage-foundation-integrated build after the flip", func(ctx SpecContext) {
			if !dataPlaneEnabled() {
				Skip("sds-local-volume is only enabled for the data-plane steps (see phase B)")
			}
			// In phase B sds-local-volume ran its legacy image (depends on snapshot-controller). Its
			// current image depends on storage-foundation, now enabled by the flip, so retag the live
			// MPO to the phase-C target (SDS_LOCAL_VOLUME_MODULE_PULL_OVERRIDE, default "main") and wait
			// Ready — the sds-local-volume analog of the svdm legacy->D1 retag. Do this LAST in phase C,
			// after the migration race and alert assertions have observed the flip's converge, and
			// before the phase-D data steps that exercise the storage-foundation-integrated CSI path
			// (unified DataImport populator, VRR-based restore).
			//
			// Restore SDS_LOCAL_VOLUME_MODULE_PULL_OVERRIDE (repointed at the legacy tag in BeforeSuite
			// so the phase-B testkit used it) back to the captured phase-C target before retagging.
			Expect(os.Setenv(envSdsLocalVolumeOverride, sdsLocalVolumePhaseCTag)).To(Succeed())
			enableModule(modSdsLocalVolume, sdsLocalVolumePhaseCTag)
		})
	})

	// ---- Phase D: invariants after the flip ----
	Context("Phase D: invariants after the flip", func() {
		It("keeps every shared CRD Established, same-UID and correctly-shaped after the flip", func(ctx SpecContext) {
			// (1) Identity: each CSI + unified CRD must still exist, stay Established, and keep the UID
			// captured before the flip — proving the handoff re-applied them in place, never
			// delete+recreated (which would cascade-delete every instance).
			for _, n := range trackedCRDs() {
				Expect(crdExists(ctx, n)).To(BeTrue(), "CRD %s must still exist after the flip", n)
				Expect(crdEstablished(ctx, n)).To(BeTrue(), "CRD %s must stay Established after the flip", n)
				if want := crdUIDBeforeFlip[n]; want != "" {
					got, err := crdUID(ctx, n)
					Expect(err).NotTo(HaveOccurred())
					Expect(got).To(Equal(want),
						"CRD %s UID must not change across the flip (in-place update, not delete+recreate)", n)
				}
			}

			// (2) Served-schema correctness: the CRDs served after the flip must be the
			// storage-foundation (extended/unified) shapes, not a vanilla reinstall. Assert their
			// marker fields. Full byte-for-byte manifest parity vs the repo YAML is verified separately
			// by storage-foundation CI (hack/check-consumer-crds.sh) — it cannot be checked against the
			// live CRD, which the API server augments (defaults/pruning/managedFields).
			Expect(crdSchemaHasField(ctx, "volumesnapshots.snapshot.storage.k8s.io", "spec", "mode")).To(BeTrue(),
				"served VolumeSnapshot CRD must carry the storage-foundation fork field spec.mode")
			Expect(crdSchemaHasField(ctx, "dataexports.storage-foundation.deckhouse.io", "spec", "targetRef", "group")).To(BeTrue(),
				"served DataExport CRD must carry the unified targetRef.group field")
			Expect(crdSchemaHasField(ctx, "dataimports.storage-foundation.deckhouse.io", "spec", "mode")).To(BeTrue(),
				"served DataImport CRD must carry the unified spec.mode field")

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

			// NOTE: source-data integrity after the flip is asserted by the next spec — a post-flip CSI
			// restore from the legacy VS whose checksum must equal sourceChecksum. We do NOT re-mount
			// src-data here: it went through the svdm PVC export (its PV was reassigned to an export
			// PVC and the probe pod removed), so re-mounting the live source is neither reliable nor
			// meaningful; the snapshot restore is the robust proof the data survived.
		})

		It("still CSI-restores from the legacy VolumeSnapshot after the flip", func(ctx SpecContext) {
			if !dataPlaneEnabled() {
				Skip("data-plane invariants skipped (no SC/VSC provided)")
			}
			// The legacy plain-CSI snapshot must remain restorable under the new stack.
			createPVCFromSnapshot(ctx, workloadNS, "restored-postflip", os.Getenv(envStorageClass), vsName, "1Gi")
			createProbePod(ctx, "probe-postflip", probeImage(), "restored-postflip")
			got, err := checksumFile(ctx, "probe-postflip", "probe", "/mnt/restored-postflip/marker")
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
			createProbePod(ctx, "probe-new", probeImage(), "new-pvc")
			_, err := writeMarkerChecksum(ctx, workloadNS, "probe-new", "probe", "/mnt/new-pvc/marker")
			Expect(err).NotTo(HaveOccurred())
			Expect(createCSIVolumeSnapshot(ctx, workloadNS, "new-snap", os.Getenv(envVSClass), "new-pvc")).To(Succeed())
			content, err := waitCSIVolumeSnapshotReady(ctx, workloadNS, "new-snap", 10*time.Minute)
			Expect(err).NotTo(HaveOccurred())
			Expect(content).NotTo(BeEmpty(), "new CSI VolumeSnapshot must be serviced by storage-foundation after the flip")

			// NOTE: the new-group DataExport/DataImport data-plane under storage-foundation is covered by
			// the next spec. The deeper state-snapshotter DOMAIN path (Snapshot with processed/managed
			// labels + SnapshotContent driven via the d8/domain SDK) is a larger domain-specific
			// surface; validate it via the existing state-snapshotter e2e suite on the same cluster.
		})

		It("drives a unified DataExport/DataImport through storage-foundation after the flip", func(ctx SpecContext) {
			if !dataPlaneEnabled() {
				Skip("data-plane invariants skipped (no SC/VSC provided)")
			}
			// After the flip svdm renders nothing; the unified DataExport/DataImport path must be served
			// by storage-foundation itself. Export new-pvc over the unified group, download+checksum,
			// then import into a fresh PVC and checksum — the new-group data-plane end-to-end under sf.
			deletePodAndWait(ctx, workloadNS, "probe-new", 2*time.Minute)
			ensureDownloadRBAC(ctx, workloadNS, httpClientSA)
			createHTTPClientPod(ctx, workloadNS, httpClientPod, httpClientSA)

			By("exporting new-pvc over the unified group (served by storage-foundation)")
			Expect(createUnifiedDataExport(ctx, workloadNS, "export-sf", "PersistentVolumeClaim", "new-pvc")).To(Succeed())
			url, caB64, err := crStatusURLCA(ctx, dataExportGVR(unifiedGroup), "export-sf")
			Expect(err).NotTo(HaveOccurred())
			Expect(svdmDownload(ctx, workloadNS, url, caB64, "marker", "/tmp/marker-sf")).To(Succeed())
			got, err := checksumFile(ctx, httpClientPod, "curl", "/tmp/marker-sf")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(sourceChecksum), "storage-foundation must serve the unified export after the flip")

			By("importing over the unified group into a fresh PVC (served by storage-foundation)")
			Expect(createUnifiedDataImport(ctx, workloadNS, "import-sf", "imported-sf", os.Getenv(envStorageClass), "1Gi")).To(Succeed())
			iurl, icaB64, err := crStatusURLCA(ctx, dataImportGVR(unifiedGroup), "import-sf")
			Expect(err).NotTo(HaveOccurred())
			Expect(svdmUpload(ctx, workloadNS, iurl, icaB64, "/tmp/marker-sf", "marker")).To(Succeed())
			waitImportComplete(ctx, unifiedGroup, workloadNS, "import-sf", "imported-sf", podRunningTimeout())

			createProbePod(ctx, "probe-sf", probeImage(), "imported-sf")
			got, err = checksumFile(ctx, "probe-sf", "probe", "/mnt/imported-sf/marker")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(sourceChecksum), "unified import under storage-foundation must match the source")

			By("tearing down the unified export/import so no in-flight CR lingers")
			// storage-foundation's PVC export reassigns the source PV (new-pvc goes Lost); deleting the
			// export must remove the CR AND recover new-pvc to Bound. Also drop the finished DataImport
			// CR (its target PVC imported-sf stays Bound — that is the delivered result). Mirrors the
			// phase-C teardown, so a kept cluster is left clean instead of with a stale in-flight export.
			deleteCRAndWaitGone(ctx, dataExportGVR(unifiedGroup), "export-sf")
			waitPVCPhase(ctx, workloadNS, "new-pvc", corev1.ClaimBound, 3*time.Minute)
			deleteCRAndWaitGone(ctx, dataImportGVR(unifiedGroup), "import-sf")
		})
	})

	AfterAll(func(ctx SpecContext) {
		// Best-effort teardown of the workload namespace (module teardown is handled in AfterSuite).
		// Keep it, like the cluster, when E2E_KEEP_CLUSTER is set (always) or a spec failed and
		// E2E_KEEP_CLUSTER_ON_FAILURE is set, so the workload can be inspected on the retained cluster.
		if envTrue("E2E_KEEP_CLUSTER") || (anySpecFailed && envTrue("E2E_KEEP_CLUSTER_ON_FAILURE")) {
			return
		}
		if namespaceExists(ctx, workloadNS) {
			_ = suiteDyn.Resource(nsGVR).Delete(ctx, workloadNS, metav1.DeleteOptions{})
		}
	})
})

// suiteCtx returns a background context for module operations. Kept as a helper so a per-op timeout
// can be threaded in later without touching call sites.
func suiteCtx() context.Context { return context.Background() }
