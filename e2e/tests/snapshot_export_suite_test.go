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

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

var _ = BeforeSuite(func() {
	prepareSuite()
})

var _ = AfterSuite(func() {
	// Deliberately no cluster teardown and no namespace deletion: the suite
	// leaves the snapshotted namespace and the SnapshotExport in place so the
	// result can be inspected. cluster.CleanupTestCluster is intentionally NOT
	// called (TEST_CLUSTER_CLEANUP is irrelevant here).
	GinkgoWriter.Printf("AfterSuite: leaving the test cluster and namespace %q in place for inspection\n", suiteNamespace)
})

func TestSnapshotExport(t *testing.T) {
	RegisterFailHandler(Fail)

	suiteConfig, reporterConfig := GinkgoConfiguration()
	if os.Getenv("CI") != "" {
		suiteConfig.Timeout = 180 * time.Minute
	}
	// One ordered, dependency-chained spec; randomization must stay off.
	suiteConfig.RandomizeAllSpecs = false
	reporterConfig.Verbose = true
	reporterConfig.ShowNodeEvents = false

	RunSpecs(t, "state-snapshotter Snapshot export E2E Suite", suiteConfig, reporterConfig)
}

// prepareSuite loads config, brings up (or connects to) the nested cluster and
// builds the clients shared by the spec.
func prepareSuite() {
	suiteCfg = loadConfig()

	GinkgoWriter.Printf("E2E config:\n")
	GinkgoWriter.Printf("  TEST_CLUSTER_CREATE_MODE:   %q\n", os.Getenv("TEST_CLUSTER_CREATE_MODE"))
	GinkgoWriter.Printf("  thin StorageClass:          %q\n", suiteCfg.thinSCName)
	GinkgoWriter.Printf("  namespace prefix:           %q\n", suiteCfg.nsPrefix)
	GinkgoWriter.Printf("  probe image:                %q\n", suiteCfg.probeImage)
	GinkgoWriter.Printf("  app PVC size:               %q\n", suiteCfg.pvcSize)
	GinkgoWriter.Printf("  base VM namespace:          %q\n", suiteCfg.vmNamespace)
	GinkgoWriter.Printf("  base StorageClass:          %q\n", suiteCfg.baseStorageClass)

	ensureNestedTestCluster()
	suiteRestCfg = suiteClusterResources.Kubeconfig

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var err error
	suiteClientset, err = storagekube.NewClientsetWithRetry(ctx, suiteRestCfg)
	Expect(err).NotTo(HaveOccurred(), "build kubernetes clientset")

	suiteDyn, err = storagekube.NewDynamicClientWithRetry(ctx, suiteRestCfg)
	Expect(err).NotTo(HaveOccurred(), "build dynamic client")

	suiteApply, err = storagekube.NewApplyClient(suiteRestCfg)
	Expect(err).NotTo(HaveOccurred(), "build apply client")
}
