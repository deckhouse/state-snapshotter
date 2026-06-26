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

	captureSpecs()                // capture_test.go: apply demo source + root Snapshot, assert Ready tree (phase 1)
	aggregatedApiSpecs()          // aggregated_api_test.go: --raw manifests-download / -with-data-restoration (phase 1)
	namespaceCaptureReworkSpecs() // namespace_capture_rbac_test.go: RBAC hook, discovery inclusion, raw secrets, immutability (commit 5)
	restoreSpecs()                // restore_test.go: manifest-level restore into a fresh namespace (phase 1)
	importSpecs()                 // import_gc_test.go: export -> import round-trip (phase 2)
	gcSpecs()                     // import_gc_test.go: TTL/GC cascade (phase 2)
	volumeDataSpecs()             // volumedata_test.go: full volume-data flow (phase 3, env-gated)
	backupDownloadSpecs()         // backup_download_test.go: backup-system HTTP download (phase 4, env-gated)
	backupRestoreSpecs()          // backup_restore_test.go: backup-system restore import (phase 5, env-gated)
})

func prepareSuite() {
	suiteCfg = loadConfig()

	GinkgoWriter.Printf("E2E config:\n")
	GinkgoWriter.Printf("  TEST_CLUSTER_CREATE_MODE:   %q\n", os.Getenv("TEST_CLUSTER_CREATE_MODE"))
	GinkgoWriter.Printf("  namespace prefix:           %q\n", suiteCfg.nsPrefix)
	GinkgoWriter.Printf("  snapshot ready timeout:     %s\n", suiteCfg.snapshotReadyTO)
	GinkgoWriter.Printf("  capture ready timeout:      %s\n", suiteCfg.captureReadyTO)
	GinkgoWriter.Printf("  module ready timeout:       %s\n", suiteCfg.moduleReadyTO)
	GinkgoWriter.Printf("  GC TTL (snapshotRootOkTtl): %s\n", suiteCfg.gcTTL)
	GinkgoWriter.Printf("  volume-data phase enabled:  %v\n", suiteCfg.volumeData)
	GinkgoWriter.Printf("  phase-3 storage class:      %q\n", suiteCfg.storageClass)
	GinkgoWriter.Printf("  probe image:                %q\n", suiteCfg.probeImage)
	GinkgoWriter.Printf("  backup client image:        %q\n", suiteCfg.backupClientImage)
	GinkgoWriter.Printf("  keep cluster on failure:    %v\n", suiteCfg.keepOnFailure)

	ensureNestedTestCluster()

	var err error
	suiteRestCfg = suiteClusterResources.Kubeconfig

	suiteClientset, err = clientgokube.NewForConfig(suiteRestCfg)
	Expect(err).NotTo(HaveOccurred(), "build clientset")

	suiteDyn, err = dynamic.NewForConfig(suiteRestCfg)
	Expect(err).NotTo(HaveOccurred(), "build dynamic client")

	// waitModuleAndCSDReady runs two sequential waits (module Ready, then CSD RBACReady), each bounded
	// by moduleReadyTO, so the parent context budgets for both plus a buffer.
	ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.moduleReadyTO+5*time.Minute)
	defer cancel()

	By("Waiting for the state-snapshotter module and the demo CSD to become Ready")
	Expect(waitModuleAndCSDReady(ctx)).To(Succeed(), "module + demo CSD readiness")
}

// prepareSharedState runs once before the Ordered specs. Clients and module readiness are already set up
// in BeforeSuite; this is the hook where phase BeforeAlls wire any additional shared fixtures.
func prepareSharedState() {
	GinkgoWriter.Printf("Shared nested cluster ready; module %s + demo CSD %s are live\n", moduleName, demoCSDName)
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
