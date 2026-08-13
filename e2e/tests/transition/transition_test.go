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

// Package transition is a manual, developer-run Ginkgo suite that brings state-snapshotter +
// storage-foundation up on ONE dev cluster that is ALREADY running the legacy snapshot stack —
// snapshot-controller plus the storage-volume-data-manager module (svdm below, an abbreviation of
// that module name) — and asserts what each of the two arrangements owes the user. See README.md
// for scope, phases and the full list of environment variables.
//
// The scenario is a transition onto the new stack, NOT a migration away from the data module.
// storage-volume-data-manager stays enabled the whole way through: it keeps serving its own API
// group, its own DataExport/DataImport resources and its own volume protection while
// storage-foundation is live, and the flip must leave all of that untouched. snapshot-controller is
// the one module the flip does supersede — its own Helm chart stops rendering workload once
// storage-foundation is enabled, leaving only its deprecation alert.
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

	// storage-e2e/pkg/cluster is deprecated in favour of pkg/e2e (e2e.Connect), where the cluster
	// lifecycle is driven by the framework's bootstrap/remove commands. The deprecation notice keeps
	// the package supported for suites that already import it, and this scenario is one of them: it
	// owns its cluster lifecycle here. Moving to pkg/e2e changes how the whole run is bootstrapped, so
	// it is a standalone migration — that migration removes this suppression, no edit here can.
	"github.com/deckhouse/storage-e2e/pkg/cluster" //nolint:staticcheck // deprecated package, see the note above
	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

// --- env knobs -------------------------------------------------------------

const (
	envRunTransition = "E2E_RUN_TRANSITION"

	// Scenario-specific image-tag vars: snapshot-controller and svdm exist only in this scenario (not
	// in the main suite's cluster_config), and sds-local-volume needs a phase-B image the main suite
	// has no use for.
	//   - snapshot-controller: ONE tag. Its single deprecated v0.2.0 build has no storage-foundation
	//     requirement (only deckhouse >= 1.76), so it installs standalone in phase B and ships the
	//     extended (storage-foundation) CRDs — no legacy/handoff split, no phase-C retag.
	//   - svdm: ONE tag as well, and the suite NEVER retags it. The module serves DataExport/DataImport
	//     under storage.deckhouse.io — the group this suite calls the legacy one, hence the variable
	//     name — and keeps serving it while the new stack runs beside it. Like every module here its
	//     image is pinned explicitly rather than defaulted, so a run always records which build it
	//     exercised.
	//   - sds-local-volume: two slots. Its current build depends on storage-foundation (absent
	//     in phase B), so phase B enables a LEGACY image that depends on snapshot-controller
	//     (E2E_TRANSITION_SDS_LOCAL_VOLUME_LEGACY_TAG) and phase C retags it to the storage-foundation-
	//     integrated build (the standard SDS_LOCAL_VOLUME_MODULE_PULL_OVERRIDE) after the flip.
	envSnapshotControllerTag   = "E2E_TRANSITION_SNAPSHOT_CONTROLLER_TAG"
	envSvdmLegacyTag           = "E2E_TRANSITION_SVDM_LEGACY_TAG"
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

	// legacyFinalizer is the finalizer storage-volume-data-manager puts on the PVC it is exporting.
	// It is ACTIVE volume protection, not bookkeeping: for the lifetime of the export the PVC's PV is
	// detached and rebound into the exporter, so a PVC that loses this finalizer mid-export can be
	// deleted out from under live data. Nothing the new stack does may sweep it — phase C asserts it
	// is still on the exported PVC after the flip.
	legacyFinalizer = "storage.deckhouse.io/storage-manager-controller"

	workloadNS   = "transition-workload"
	srcPVCName   = "src-data"
	probePodName = "probe"

	// The legacy-group DataExport/DataImport created in phase B. Phase C reads them back around the
	// flip: they belong to storage-volume-data-manager and must come through it as the same objects.
	legacyExportName = "export-pvc"
	legacyImportName = "import-di"
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

	// legacyExportUID / legacyImportUID are the uids of the phase-B legacy-group CRs, read just
	// before the flip so phase C can prove the very same objects came through it.
	legacyExportUID string
	legacyImportUID string

	// CRDs whose identity the flip must preserve, for two different reasons.
	//
	// csiCRDNames are installed by snapshot-controller in phase B; storage-foundation re-applies the
	// same manifests as it takes ownership, so the flip must UPDATE them in place. A changed UID means
	// delete+recreate, which cascade-deletes every VolumeSnapshot in the cluster.
	//
	// legacyCRDNames belong to storage-volume-data-manager, which stays enabled: the new stack must
	// not touch them at all. Their UID is the strongest available proof of that — deleting either CRD
	// cascades away every DataExport/DataImport a user created through that module, and reinstalling
	// the CRD afterwards brings none of them back.
	//
	// Both sets have their UIDs captured just before storage-foundation is enabled and re-checked in
	// phase D.
	csiCRDNames = []string{
		"volumesnapshots.snapshot.storage.k8s.io",
		"volumesnapshotcontents.snapshot.storage.k8s.io",
		"volumesnapshotclasses.snapshot.storage.k8s.io",
	}
	legacyCRDNames = []string{
		"dataexports." + legacyGroup,
		"dataimports." + legacyGroup,
	}
	// unifiedCRDNames are storage-foundation's own DataExport/DataImport CRDs. Nothing installs them
	// while only the legacy stack runs, so the FLIP is what creates them: no UID is captured
	// beforehand, and phases C and D assert they appeared and are served. Their absence before the
	// flip is deliberately NOT asserted — Deckhouse leaves a module's CRDs in the cluster when the
	// module is disabled, so a re-run on a reused cluster legitimately starts with the previous run's
	// copies.
	unifiedCRDNames = []string{
		"dataexports." + unifiedGroup,
		"dataimports." + unifiedGroup,
	}
	crdUIDBeforeFlip = map[string]string{}
)

// trackedCRDs is every CRD whose identity the flip must preserve: the CSI CRDs storage-foundation
// takes over, and the legacy CRDs of the module that keeps running beside it.
func trackedCRDs() []string { return append(append([]string{}, csiCRDNames...), legacyCRDNames...) }

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
	// (bootstrap -> legacy stack -> flip -> invariants), so spec randomization MUST stay OFF.
	suiteConfig.RandomizeAllSpecs = false
	reporterConfig.Verbose = true

	RunSpecs(t, "state-snapshotter transition E2E Suite", suiteConfig, reporterConfig)
}

var _ = BeforeSuite(func() {
	// Validate the module image-tag env vars first (before any provisioning): every one that is set
	// must be a plain-ASCII tag matching mr<N>/pr<N>/main. This catches a prod v* tag (absent from
	// the dev registry the nested cluster pulls) and — the real footgun — a tag typed in a non-Latin
	// keyboard layout (e.g. a Cyrillic tag U+0430 U+0435 U+0442 instead of the Latin "main"), which otherwise only surfaces
	// minutes later as a wedged converge in a mid-run phase.
	validateModuleTagEnvVars()

	if strings.TrimSpace(os.Getenv("TEST_CLUSTER_CREATE_MODE")) == "" {
		Fail("TEST_CLUSTER_CREATE_MODE must be set: this suite only supports storage-e2e nested clusters")
	}
	// Fail fast before provisioning if any required image-tag var is missing.
	requireEnv(envSnapshotControllerTag, envSvdmLegacyTag)
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
	envSdsLocalVolumeLegacyTag,
	envSdsLocalVolumeOverride,
	envStateSnapshotterOverride,
	envStorageFoundationOverride,
	"SDS_NODE_CONFIGURATOR_MODULE_PULL_OVERRIDE",
}

// isASCII reports whether s contains only ASCII bytes. A value typed in a non-Latin keyboard layout
// (e.g. a Cyrillic tag U+0430 U+0435 U+0442) carries multi-byte UTF-8 runes and fails this check.
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

// enableModule enables (or retags) a single module with no cross-module dependency, then waits for
// it to become Ready. Re-calling it with a DIFFERENT image tag retags the live ModulePullOverride —
// that is how phase C moves sds-local-volume onto its storage-foundation-integrated build. To enable
// a module together with a dependency, batch them via enableModules(moduleSpec(...), ...).
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
	Context("Phase B: legacy stack (snapshot-controller + storage-volume-data-manager)", func() {
		It("enables snapshot-controller, svdm and sds-local-volume", func() {
			// One batch: the framework brings snapshot-controller and svdm up concurrently (no
			// interdependency) and sds-local-volume after snapshot-controller (its legacy dependency,
			// declared in-batch so the graph resolves — a separate call would fail graph-build).
			// snapshot-controller runs its single deprecated v0.2.0 build (E2E_TRANSITION_SNAPSHOT_CONTROLLER_TAG):
			// no storage-foundation requirement, so it installs standalone here and ships the extended
			// (storage-foundation) CRDs — the "vanilla controller + extended CRDs" combination the next
			// spec verifies. svdm runs the build pinned by E2E_TRANSITION_SVDM_LEGACY_TAG and is never
			// retagged: it serves its own API group here and keeps serving it through the flip.
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

			// DataExport the source PVC on the legacy group/schema; wait for status.url + status.ca.
			// This export deliberately STAYS live for the rest of the run: phase C reads it back after
			// the flip and requires it to be the same object, still serving the same bytes.
			Expect(createLegacyDataExport(ctx, workloadNS, legacyExportName, "PersistentVolumeClaim", srcPVCName)).To(Succeed())
			url, caB64, err := crStatusURLCA(ctx, dataExportGVR(legacyGroup), legacyExportName)
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
			Expect(createLegacyDataImport(ctx, workloadNS, legacyImportName, "imported-data", os.Getenv(envStorageClass), "1Gi")).To(Succeed())
			url, caB64, err := crStatusURLCA(ctx, dataImportGVR(legacyGroup), legacyImportName)
			Expect(err).NotTo(HaveOccurred())

			By("uploading the marker over the svdm HTTP API and signalling finished")
			Expect(svdmUpload(ctx, workloadNS, url, caB64, "/tmp/marker", "marker")).To(Succeed())
			logf("upload + POST finished done; DataImport conditions: %s", crConditions(ctx, dataImportGVR(legacyGroup), workloadNS, legacyImportName))

			By("waiting for the populator to rebind the prime volume onto imported-data (PVC Bound)")
			// Import completion = the target PVC becoming Bound: the DataImport Ready condition flips
			// True early (server ready) and there is no Completed condition type, so the PVC phase is
			// the real gate. waitImportComplete narrates DI conditions / prime PVC / pods every 15s so a
			// stall is visible; podRunningTimeout() budgets the whole chain.
			waitImportComplete(ctx, legacyGroup, workloadNS, legacyImportName, "imported-data", podRunningTimeout())

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

	// ---- Phase C: bring the new stack up beside the running data module ----
	Context("Phase C: flip to the new stack without disabling the data module", func() {
		It("enables state-snapshotter -> storage-foundation while storage-volume-data-manager keeps running", func(ctx SpecContext) {
			// Capture the tracked CRD UIDs RIGHT BEFORE storage-foundation is enabled: the CSI CRDs it
			// re-applies as it takes ownership, and the legacy CRDs of the module that keeps running
			// beside it. Both have to come through the flip as the SAME objects (see trackedCRDs).
			for _, n := range trackedCRDs() {
				u, err := crdUID(ctx, n)
				Expect(err).NotTo(HaveOccurred(), "read CRD %s UID before the flip", n)
				Expect(u).NotTo(BeEmpty(), "CRD %s must carry a uid before the flip", n)
				crdUIDBeforeFlip[n] = u
			}

			if dataPlaneEnabled() {
				// Record the live legacy epoch: the export still serving since phase B, the finished
				// import, and the volume protection that export holds on the source PVC. Asserting the
				// finalizer is present HERE is what keeps the post-flip check honest — "still carries
				// it" would otherwise pass just as well on an epoch that never had one.
				exp, err := getUnstr(ctx, dataExportGVR(legacyGroup), workloadNS, legacyExportName)
				Expect(err).NotTo(HaveOccurred(), "the phase-B DataExport must be live before the flip")
				legacyExportUID = string(exp.GetUID())
				Expect(legacyExportUID).NotTo(BeEmpty())

				imp, err := getUnstr(ctx, dataImportGVR(legacyGroup), workloadNS, legacyImportName)
				Expect(err).NotTo(HaveOccurred(), "the phase-B DataImport must still exist before the flip")
				legacyImportUID = string(imp.GetUID())
				Expect(legacyImportUID).NotTo(BeEmpty())

				finalizers, err := pvcFinalizers(ctx, workloadNS, srcPVCName)
				Expect(err).NotTo(HaveOccurred())
				Expect(finalizers).To(ContainElement(legacyFinalizer),
					"while the export holds its PV, the exporting module protects PVC %s/%s with its own finalizer",
					workloadNS, srcPVCName)
			}

			// One batch: state-snapshotter first, then storage-foundation (its state-snapshotter
			// dependency declared in-batch so the graph resolves — a separate call would fail
			// graph-build with "dependency module state-snapshotter not found").
			enableModules(
				moduleSpec(modStateSnapshotter, tagFrom(envStateSnapshotterOverride)),
				moduleSpec(modStorageFoundation, tagFrom(envStorageFoundationOverride), modStateSnapshotter),
			)

			// snapshot-controller's ModuleConfig stays enabled:true — what is checked here is its Helm
			// GUARD, not uninstall. Its chart renders nothing but the deprecation PrometheusRule once
			// storage-foundation is enabled, so its Deployments/Services must drain to zero.
			//
			// storage-volume-data-manager is deliberately NOT in this check, and must never be added to
			// it: it is a supported module with its own data plane, it has no such guard, and the next
			// spec asserts the OPPOSITE for it — its workload keeps running and its resources keep
			// working.
			guardedNS := moduleNamespace[modSnapshotController]
			Eventually(func(ctx SpecContext) (int, error) {
				return workloadResourceCount(ctx, guardedNS)
			}).WithContext(ctx).WithTimeout(10*time.Minute).WithPolling(pollInterval).Should(Equal(0),
				"module %s must render no Deployments/Services once storage-foundation is enabled (guard)",
				modSnapshotController)
		})

		It("leaves the legacy epoch of storage-volume-data-manager untouched by the flip", func(ctx SpecContext) {
			// This is the point of the whole scenario: the new stack comes up beside a module that keeps
			// serving its own API group, and takes nothing away from it. Every assertion below is about
			// state the flip must NOT have changed — plus the two CRDs it must have added.
			By("asserting both legacy CRDs are still the same objects")
			for _, n := range legacyCRDNames {
				Expect(crdExists(ctx, n)).To(BeTrue(), "CRD %s must still exist after the flip", n)
				Expect(crdEstablished(ctx, n)).To(BeTrue(), "CRD %s must stay Established after the flip", n)
				got, err := crdUID(ctx, n)
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(Equal(crdUIDBeforeFlip[n]),
					"CRD %s must not be deleted and reinstalled: the delete cascades away every DataExport/DataImport a user created through that module, and reinstalling the CRD brings none of them back", n)
			}

			By("asserting the storage-foundation CRDs arrived with the flip")
			for _, n := range unifiedCRDNames {
				Expect(crdExists(ctx, n)).To(BeTrue(), "CRD %s must be installed by the flip", n)
				Expect(crdEstablished(ctx, n)).To(BeTrue(), "CRD %s must be Established after the flip", n)
			}

			By("asserting the data module still renders its own workload")
			// The inverse of the snapshot-controller guard: this module has no "storage-foundation is
			// enabled" guard and must not acquire one. A zero here would mean its controllers were
			// switched off underneath the users who are still on its API group.
			Eventually(func(ctx SpecContext) (int, error) {
				return workloadResourceCount(ctx, moduleNamespace[modSvdm])
			}).WithContext(ctx).WithTimeout(10*time.Minute).WithPolling(pollInterval).Should(BeNumerically(">", 0),
				"module %s must keep running its own Deployments/Services while storage-foundation is enabled", modSvdm)

			By("asserting both modules are Ready side by side")
			for _, m := range []string{modSvdm, modStorageFoundation} {
				Expect(storagekube.WaitForModuleReady(suiteCtx(), suiteRes.Kubeconfig, m, 5*time.Minute)).To(Succeed(),
					"module %s must be Ready with the other one enabled", m)
			}

			if !dataPlaneEnabled() {
				return
			}

			By("asserting the phase-B DataExport and DataImport are the same objects")
			exp, err := getUnstr(ctx, dataExportGVR(legacyGroup), workloadNS, legacyExportName)
			Expect(err).NotTo(HaveOccurred(), "the phase-B DataExport must survive the flip")
			Expect(string(exp.GetUID())).To(Equal(legacyExportUID),
				"DataExport %s/%s must be the same object, not one re-created under a different owner", workloadNS, legacyExportName)
			imp, err := getUnstr(ctx, dataImportGVR(legacyGroup), workloadNS, legacyImportName)
			Expect(err).NotTo(HaveOccurred(), "the phase-B DataImport must survive the flip")
			Expect(string(imp.GetUID())).To(Equal(legacyImportUID),
				"DataImport %s/%s must be the same object", workloadNS, legacyImportName)

			By("asserting the source PVC still carries the data module's finalizer")
			finalizers, err := pvcFinalizers(ctx, workloadNS, srcPVCName)
			Expect(err).NotTo(HaveOccurred())
			Expect(finalizers).To(ContainElement(legacyFinalizer),
				"the volume protection of the exporting module must not be swept by the new stack coming up")

			By("downloading the marker through the still-live legacy export")
			// Same export, same file, checksum compared against the phase-B source: the export does not
			// merely still exist as an object, it still serves the volume's bytes. The download identity
			// and curl pod are re-ensured (both are idempotent) so this spec does not depend on the
			// phase-B pod having survived.
			ensureDownloadRBAC(ctx, workloadNS, httpClientSA)
			createHTTPClientPod(ctx, workloadNS, httpClientPod, httpClientSA)
			url, caB64, err := crStatusURLCA(ctx, dataExportGVR(legacyGroup), legacyExportName)
			Expect(err).NotTo(HaveOccurred())
			Expect(svdmDownload(ctx, workloadNS, url, caB64, "marker", "/tmp/marker-postflip")).To(Succeed())
			got, err := checksumFile(ctx, httpClientPod, "curl", "/tmp/marker-postflip")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(sourceChecksum),
				"the export created before the flip must still serve the source bytes after it")

			By("tearing the legacy export down under the new stack — the source PVC must recover to Bound")
			// The teardown path has to keep working with storage-foundation live: deleting the CR must
			// release its finalizers and hand the reassigned PV back, so src-data returns from Lost to
			// Bound. It also leaves no in-flight export behind for phase D or for a kept cluster.
			deleteCRAndWaitGone(ctx, dataExportGVR(legacyGroup), legacyExportName)
			waitPVCPhase(ctx, workloadNS, srcPVCName, corev1.ClaimBound, 3*time.Minute)
		})

		It("fires the deprecation alerts for snapshot-controller and none for the data module", func(ctx SpecContext) {
			// snapshot-controller has been Deprecated since phase B (its single v0.2.0 build is, and it
			// needs no retag — it never required storage-foundation). Deckhouse must surface two firing
			// ClusterAlerts for it:
			//   - built-in ModuleIsDeprecated{module=snapshot-controller} — proves module.yaml
			//     stage=Deprecated took effect;
			//   - custom D8SnapshotControllerModuleDeprecated (vector(1), severity 9) — proves that
			//     module's always-on deprecation-alert template renders.
			// Alert eval lags a scrape, so expectAlertFiring waits at the package-level alertTimeout.
			expectAlertFiring(ctx, "ModuleIsDeprecated", modSnapshotController)
			expectAlertFiring(ctx, "D8SnapshotControllerModuleDeprecated", "")

			// storage-volume-data-manager is NOT deprecated: it stays supported and runs beside
			// storage-foundation, so nothing may announce it as going away. This negative check comes
			// AFTER the two positive ones on purpose — they prove alert evaluation has caught up, and
			// without that proof "no such alert" would pass on any cluster where Prometheus simply has
			// not got there yet.
			firing, err := clusterAlertFiring(ctx, "ModuleIsDeprecated", modSvdm)
			Expect(err).NotTo(HaveOccurred())
			Expect(firing).To(BeFalse(),
				"module %s is supported and must not be announced as deprecated — firing now: %s",
				modSvdm, firingAlertNames(ctx))
		})

		It("retags sds-local-volume to the storage-foundation-integrated build after the flip", func(_ SpecContext) {
			if !dataPlaneEnabled() {
				Skip("sds-local-volume is only enabled for the data-plane steps (see phase B)")
			}
			// In phase B sds-local-volume ran its legacy image (depends on snapshot-controller). Its
			// current image depends on storage-foundation, now enabled by the flip, so retag the live
			// MPO to the phase-C target (SDS_LOCAL_VOLUME_MODULE_PULL_OVERRIDE, default "main") and wait
			// Ready. It is the ONLY module this scenario retags. Do this LAST in phase C, after the
			// legacy-epoch and alert assertions have observed the flip's converge, and before the
			// phase-D data steps that exercise the storage-foundation-integrated CSI path (unified
			// DataImport populator, VRR-based restore).
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
			// (1) Identity: each CSI + legacy CRD must still exist, stay Established, and keep the UID
			// captured before the flip. For the CSI CRDs that proves storage-foundation re-applied them
			// in place as it took ownership, never delete+recreated (which would cascade-delete every
			// instance); for the legacy CRDs it proves the new stack left the neighbouring module's own
			// resources alone. This re-checks in phase D what phase C asserted right after the flip:
			// everything the flip converged afterwards (the sds-local-volume retag, the module Ready
			// transitions) had to leave the same identities in place.
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

			// The storage-foundation CRDs are the ones the flip CREATED, so there is no pre-flip UID to
			// compare them against — they are held to existing and being served.
			for _, n := range unifiedCRDNames {
				Expect(crdExists(ctx, n)).To(BeTrue(), "CRD %s must exist after the flip installed it", n)
				Expect(crdEstablished(ctx, n)).To(BeTrue(), "CRD %s must be Established after the flip", n)
			}

			// (2) Served-schema correctness: the CRDs served after the flip must be the
			// storage-foundation (extended/unified) shapes, not a vanilla reinstall. Assert their
			// marker fields. Full byte-for-byte manifest parity vs the repo YAML is verified separately
			// by storage-foundation CI (hack/check-consumer-crds.sh, which diffs the CRDs it shares with
			// its consumers) — it cannot be checked against the live CRD, which the API server augments
			// (defaults/pruning/managedFields).
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
			// restore from the phase-B VS whose checksum must equal sourceChecksum. We do NOT re-mount
			// src-data here: it spent the flip under an svdm export that held its PV (and lost its probe
			// pod for that), and phase C already read its bytes back through that export. Restoring from
			// the snapshot is the robust proof the data survived, and it is the one the new stack serves.
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
			// The two data planes are independent: svdm keeps serving its own group (phase C), and
			// storage-foundation must serve ITS group on its own, on the same cluster. Export new-pvc over
			// the unified group, download+checksum, then import into a fresh PVC and checksum — the
			// storage-foundation data plane end-to-end, beside a live neighbour.
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
