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
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/dynamic"
	clientgokube "k8s.io/client-go/kubernetes"
)

// anySpecFailed records whether any spec failed during the run. cleanupSuite consults it together with
// E2E_KEEP_CLUSTER_ON_FAILURE to decide whether to skip nested-cluster teardown.
var anySpecFailed bool

var _ = BeforeSuite(func() {
	prepareSuite()
})

var _ = AfterSuite(func() {
	cleanupSuite()
})

func TestSnapshotter(t *testing.T) {
	RegisterFailHandler(Fail)

	suiteConfig, reporterConfig := GinkgoConfiguration()
	if os.Getenv("CI") != "" {
		suiteConfig.FailFast = true
		suiteConfig.Timeout = 180 * time.Minute
	}
	// The suite shares one expensive nested cluster and one captured snapshot tree across
	// dependency-ordered specs (capture -> aggregated reads -> restore -> import -> GC), so spec
	// randomization MUST stay OFF.
	suiteConfig.RandomizeAllSpecs = false
	reporterConfig.Verbose = true
	reporterConfig.ShowNodeEvents = false

	RunSpecs(t, "state-snapshotter E2E Suite", suiteConfig, reporterConfig)
}

// The single root Ordered container. Spec registration goes through builder functions called in EXPLICIT
// dependency order: per-file top-level Describes would order alphabetically and break the
// capture-before-read / capture-before-GC invariants.
var _ = Describe("state-snapshotter e2e", Ordered, ContinueOnFailure, func() {
	BeforeAll(prepareSharedState)

	// Dump module / snapshot / content conditions and controller logs on any failure.
	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		anySpecFailed = true
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		dumpFailedSpecDiagnostics(ctx)
	})

	// Global readiness invariant, enforced at the END of every spec that otherwise passed: no snapshot in
	// the cluster may be Ready=True while any descendant in its tree is not Ready (the propagation bug the
	// domain-phase-fold fix closes). Registered AFTER the diagnostics hook so it runs FIRST (Ginkgo runs
	// AfterEach nodes in reverse registration order): if it trips, the diagnostics hook then dumps the
	// offending tree. Skipped when the spec already failed (its own failure + diagnostics come first).
	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		assertReadyConsistencyAcrossTrees(ctx, 90*time.Second)
	})

	// Phases 1 & 2 merged into one manifest-only flow (no volume data): capture, aggregated subresource
	// reads, namespace-capture rework, manifest restore, export->import round-trip, and TTL/GC cascade.
	// These cheap specs share one `captured` tree and run sequentially, so they live under a single phase
	// container. (gcSpecs still builds its own short-TTL sub-tree — it deletes the root and reconfigures the
	// module TTL, so it cannot reuse the shared root.)
	Context("Phase 1 & 2: manifest-only flow (no volume data)", func() {
		captureSpecs()                  // capture_test.go: apply demo source + root Snapshot, assert Ready tree
		aggregatedAPISpecs()            // aggregated_api_test.go: --raw manifests-download / -with-data-restoration
		namespaceCaptureReworkSpecs()   // namespace_capture_rbac_test.go: RBAC hook, discovery inclusion, raw secrets, immutability
		namespaceManifestCaptureSpecs() // namespace_manifest_capture_test.go: Namespace object capture + MCR admission
		restoreSpecs()                  // restore_test.go: manifest-level restore into a fresh namespace
		importSpecs()                   // import_gc_test.go: export -> import round-trip
		gcSpecs()                       // import_gc_test.go: TTL/GC cascade (own short-TTL sub-tree)

		// The shared manifest-only `captured` namespace (created in captureSpecs' BeforeAll) is read by every
		// spec above but owned by none of them, so — unlike every other namespace, which has its own
		// DeferCleanup(deleteNamespace) — it had no teardown and leaked after each run. Reap it once the whole
		// phase completes: this AfterAll runs after all nested Contexts here (and before Phase 3), and
		// deleteNamespace honors the keep-on-failure/keep-always knobs like every other cleanup.
		AfterAll(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			deleteNamespace(ctx, captured.namespace)
		})
	})
	resourceSelectorSpecs()          // resource_selector_test.go: spec.resourceSelector include/exclude across manifests, CSD, PVC (own namespaces; phase 1b + env-gated 3b)
	vetoSelectorSpecs()              // veto_selector_test.go: exclude-veto + resourceSelector across all tree levels (root object, VM child, disk grandchild, VM companion Secret, PVC/orphan, veto+selector combo) (default on; opt-out: E2E_VETO_SELECTOR=false; data fixture also needs E2E_VOLUME_DATA)
	resourceSelectorAdmissionSpecs() // resource_selector_admission_test.go: CEL forbids spec.resourceSelector on mode: Import (admission rejection; skip-not-fail on an older CRD)
	volumeDataSpecs()                // volumedata_test.go: full volume-data flow (phase 3; default on; opt-out: E2E_VOLUME_DATA=false)
	volumeDataGcSpecs()              // volumedata_gc_test.go: durable data-bearing tree survives ns deletion, then ObjectKeeper deletion reclaims the whole tree incl. llvs (phase 3; default on; opt-out: E2E_VOLUME_DATA=false)
	volumeSnapshotDomainSpecs()      // volumesnapshot_domain_test.go: Block 3d VS domain — user + vetoed VolumeSnapshot (default on; opt-out: E2E_VOLUME_DATA=false)
	childBridgeFailureSpecs()        // child_bridge_failure_test.go: domain-disk terminal volume capture -> parent Ready=False/ChildrenFailed (default on; opt-out: E2E_CHILD_BRIDGE_FAILURE=false)
	manifestCheckpointLossSpecs()    // manifest_checkpoint_loss_test.go: root/child/grandchild MCP (or chunk) deleted after capture -> node ManifestCheckpointFailed + root ChildrenFailed (default on; opt-out: E2E_MANIFEST_CHECKPOINT_LOSS=false)
	freezeDeadlineSpecs()            // freeze_deadline_test.go: hung child disk snapshot (thick-vol CSI error, non-terminal VCR) -> VM self-Fail ConsistencyDeadlineExceeded + freeze marker cleared (default on; opt-out: E2E_FREEZE_DEADLINE=false)
	readyFlapSpecs()                 // ready_flap_test.go: Ready True->False->True flap detector on mixed orphan+domain tree (default on; opt-out: E2E_VOLUME_DATA=false)
	getLoadSpecs()                   // get_load_test.go: REST GET-load delta across the capture wave via /metrics (default on; opt-out: E2E_GET_LOAD=false)
	backupDownloadSpecs()            // backup_download_test.go: backup-system HTTP download (phase 4; default on; opt-out: E2E_VOLUME_DATA=false)
	importVariantsSpecs()            // backup_restore_test.go: import any tree node — 4 parallel variants (phase 5; default on; opt-out: E2E_VOLUME_DATA=false)
	publishDataExportSpecs()         // publish_de_test.go: DataExport publish:true — internal (status.url) + external (ingress) token auth, checksums, teardown (default on; opt-out: E2E_PUBLISH=false)
	publishDataImportSpecs()         // publish_di_test.go: DataImport publish:true — external (ingress) block upload via publicURL, terminal state, restore checksum, no-token negative, infra teardown (default on; opt-out: E2E_PUBLISH=false)
	publishManifestsSpecs()          // publish_manifests_test.go: aggregated manifests-download reachable externally through the SAME kubernetes-api ingress — internal==external + live match, 403 without RBAC (proves no separate APIService ingress; default on; opt-out: E2E_PUBLISH=false)
	deleteGuardSpecs()               // delete_guard_test.go: unified-snapshot delete protection (opt-in E2E_DELETE_GUARD; needs admission enforcement=Deny)
})

func prepareSuite() {
	suiteCfg = loadConfig()

	GinkgoWriter.Printf("E2E config:\n")
	GinkgoWriter.Printf("  TEST_CLUSTER_CREATE_MODE:   %q\n", os.Getenv("TEST_CLUSTER_CREATE_MODE"))
	GinkgoWriter.Printf("  namespace prefix:           %q\n", suiteCfg.nsPrefix)
	GinkgoWriter.Printf("  run id (ns %s-<runID>-<role>): %q  (E2E_RUN_ID or generated MMDD-HHMM-<rand>)\n", suiteCfg.nsPrefix, suiteCfg.runID)
	GinkgoWriter.Printf("  snapshot ready timeout:     %s\n", suiteCfg.snapshotReadyTO)
	GinkgoWriter.Printf("  capture ready timeout:      %s\n", suiteCfg.captureReadyTO)
	GinkgoWriter.Printf("  data transfer timeout:      %s\n", suiteCfg.dataTransferTO)
	GinkgoWriter.Printf("  module ready timeout:       %s\n", suiteCfg.moduleReadyTO)
	GinkgoWriter.Printf("  GC TTL (snapshotTtlAfterDelete): %s\n", suiteCfg.gcTTL)
	GinkgoWriter.Printf("  volume-data phase enabled:  %v  (default on; E2E_VOLUME_DATA=false to disable)\n", suiteCfg.volumeData)
	GinkgoWriter.Printf("  GET-load measurement:       %v  (default on; E2E_GET_LOAD=false to disable)\n", suiteCfg.getLoad)
	GinkgoWriter.Printf("  namespace-capture extended: %v  (default on; E2E_NS_CAPTURE_REWORK=false to disable)\n", envEnabledByDefault(os.Getenv(envNSCaptureRework)))
	GinkgoWriter.Printf("  publish sanity-check:       %v  (default on; E2E_PUBLISH=false to disable)\n", suiteCfg.publish)
	GinkgoWriter.Printf("  phase-3 storage class:      %q\n", suiteCfg.storageClass)
	GinkgoWriter.Printf("  probe image:                %q\n", suiteCfg.probeImage)
	GinkgoWriter.Printf("  backup client image:        %q\n", suiteCfg.backupClientImage)
	GinkgoWriter.Printf("  keep cluster on failure:    %v\n", suiteCfg.keepOnFailure)
	GinkgoWriter.Printf("  keep resources (always):    %v\n", suiteCfg.keepAlways)

	ensureNestedTestCluster()

	var err error
	suiteRestCfg = suiteClusterResources.Kubeconfig

	suiteClientset, err = clientgokube.NewForConfig(suiteRestCfg)
	Expect(err).NotTo(HaveOccurred(), "build clientset")

	suiteDyn, err = dynamic.NewForConfig(suiteRestCfg)
	Expect(err).NotTo(HaveOccurred(), "build dynamic client")

	// Publish (ingress + tokens) prerequisites are a hard, cluster-side gate that does NOT depend on the
	// snapshot module stack, so check them FIRST — before the multi-minute module-readiness wait below —
	// so a cluster that cannot support publish fails immediately instead of only after the whole stack
	// converges. The gate is ON by default (opt-out via E2E_PUBLISH=false). The storage-e2e bootstrap
	// wires these prerequisites (global publicDomainTemplate + user-authn publishAPI + a working `nginx`
	// IngressClass) ONLY on alwaysCreateNew; an alwaysUseExisting/commander cluster must already provide
	// them — they are cluster-global (publicDomainTemplate) or infra-specific (the ingress controller
	// inlet) and thus NOT something a test can safely install — otherwise set E2E_PUBLISH=false.
	// checkPublishInfra INSTALLS NOTHING: it asserts the profile (fail-fast) and records the ingress facts.
	if suiteCfg.publish {
		checkPublishInfra()
	}

	// waitModuleAndCSDReady enables + waits for the whole module stack (five modules across three
	// dependency levels: state-snapshotter/sds-node-configurator -> storage-foundation/poc ->
	// sds-local-volume), then the demo CSD. Each wait is bounded by moduleReadyTO; convergence is largely
	// serial along the dependency chain, so the parent context budgets for a few of them plus a buffer for
	// the (retrying) enable step.
	ctx, cancel := context.WithTimeout(context.Background(), 3*suiteCfg.moduleReadyTO+10*time.Minute)
	defer cancel()

	By("Enabling and waiting for the required modules (state-snapshotter, storage-foundation, sds-node-configurator, sds-local-volume, PoC), demo CSDs AccessGranted, and demo CRDs Established")
	Expect(waitModuleAndCSDReady(ctx)).To(Succeed(), "module + demo CSD/CRD readiness")
}

// prepareSharedState runs once before the Ordered specs. Clients and module readiness are already set up
// in BeforeSuite; this is the hook where phase BeforeAlls wire any additional shared fixtures.
func prepareSharedState() {
	GinkgoWriter.Printf("Shared nested cluster ready; module %s + demo CSDs %s/%s are live\n", moduleName, demoVMCSDName, demoDiskCSDName)
}

func cleanupSuite() {
	// Keep the nested cluster alive for manual debugging when a spec failed and the operator asked for
	// it. Otherwise tear it down (the only mandatory step; resource-level cleanup is driven by the specs).
	if suiteCfg.keepOnFailure && anySpecFailed {
		printKeepClusterBanner()
		return
	}
	cleanupNestedTestCluster()
}

func printKeepClusterBanner() {
	GinkgoWriter.Printf("\n========== E2E_KEEP_CLUSTER_ON_FAILURE: cluster preserved ==========\n")
	GinkgoWriter.Printf("A spec failed and nested-cluster teardown was SKIPPED for debugging.\n")
	if suiteClusterResources != nil && suiteClusterResources.KubeconfigPath != "" {
		GinkgoWriter.Printf("  kubeconfig (export KUBECONFIG):   %s\n", suiteClusterResources.KubeconfigPath)
	}
	GinkgoWriter.Printf("Remember to delete the VMs / nested cluster manually when finished.\n")
	GinkgoWriter.Printf("====================================================================\n")
}
