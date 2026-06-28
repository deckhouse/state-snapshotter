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
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// getLoadRootSnapshot is the root Snapshot for the GET-load run. It reuses the shared vol-tree fixture
// (buildVolumeSource) but lives in its own namespace/name so it does not collide with the phase-3 or
// flap-detector runs over the same fixture.
const getLoadRootSnapshot = "vol-tree-getload"

// controllerLeaseName is the controller-runtime leader-election Lease (LeaderElectionID = "controller")
// in the module namespace. Its holderIdentity is "<podName>_<uuid>", which identifies the single pod that
// actually runs reconciliation.
const controllerLeaseName = "controller"

// getLoadMaxPerSecEnv optionally enforces an upper bound on GET/sec across the capture wave. It is left
// UNSET for the baseline run (log only) and set to the baseline figure for the new run, so the same spec
// hard-asserts the improvement only when the operator opts in (the counter is process-wide; see the spec).
const getLoadMaxPerSecEnv = "E2E_GET_LOAD_MAX_PER_SEC"

// sumRestClientGETs parses a Prometheus text exposition body and sums every rest_client_requests_total
// series carrying method="GET". client-go splits the counter by code/host/method, so all GET series are
// summed into one number. It errors if no such series is present (the metric registration regressed or the
// scrape hit the wrong endpoint), so a silent zero never masquerades as "no load".
func sumRestClientGETs(body []byte) (float64, error) {
	const metricName = "rest_client_requests_total"
	var total float64
	matched := false
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, metricName+"{") {
			continue
		}
		lb := strings.IndexByte(line, '{')
		rb := strings.LastIndexByte(line, '}')
		if lb < 0 || rb < 0 || rb < lb {
			continue
		}
		if !strings.Contains(line[lb+1:rb], `method="GET"`) {
			continue
		}
		valStr := strings.TrimSpace(line[rb+1:])
		v, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			return 0, fmt.Errorf("parse value %q in %q: %w", valStr, line, err)
		}
		total += v
		matched = true
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("scan metrics body: %w", err)
	}
	if !matched {
		return 0, fmt.Errorf("no %s{method=\"GET\"} series in controller /metrics", metricName)
	}
	return total, nil
}

// leaderControllerPod resolves the pod name of the current reconciliation leader from the controller's
// leader-election Lease (holderIdentity = "<podName>_<uuid>"). Pinning both scrapes to this single pod is
// what makes the delta meaningful: rest_client_requests_total is per-process and the capture wave's GETs
// run only on the leader, so a Service proxy (which load-balances across replicas in an HA cluster) could
// otherwise route the before/after scrapes to different pods.
func leaderControllerPod(ctx context.Context) (string, error) {
	lease, err := suiteClientset.CoordinationV1().Leases(d8ModuleNS).Get(ctx, controllerLeaseName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get controller leader Lease %s/%s: %w", d8ModuleNS, controllerLeaseName, err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		return "", fmt.Errorf("controller Lease %s/%s has no holderIdentity", d8ModuleNS, controllerLeaseName)
	}
	id := *lease.Spec.HolderIdentity
	// Pod names are DNS-1123 (no '_'); controller-runtime joins podName and a uuid with a single '_'.
	if i := strings.IndexByte(id, '_'); i > 0 {
		return id[:i], nil
	}
	return id, nil
}

// podMetricsPath is the apiserver pod-proxy path to one controller pod's plain-HTTP /metrics endpoint
// (controller-runtime's default metrics server on the container port named "metrics"). No Prometheus/RBAC
// setup is needed: the cluster-admin suite kubeconfig proxies it.
func podMetricsPath(pod string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:metrics/proxy/metrics", d8ModuleNS, pod)
}

// scrapeRestClientGETs scrapes one controller pod's /metrics via the apiserver pod-proxy and returns the
// summed rest_client_requests_total{method="GET"} counter.
func scrapeRestClientGETs(ctx context.Context, metricsPath string) (float64, error) {
	body, err := aggGet(ctx, metricsPath, nil)
	if err != nil {
		return 0, fmt.Errorf("scrape controller /metrics (%s): %w", metricsPath, err)
	}
	return sumRestClientGETs(body)
}

// getLoadSpecs registers the GET-load measurement spec (env-gated by E2E_VOLUME_DATA). It captures the
// vol-tree fixture and reports the delta of rest_client_requests_total{method="GET"} over the capture wave
// (root Snapshot create -> first Ready=True), normalized per second.
//
// Methodology (see the APIReader->cache batch plan): the controller exposes a PER-PROCESS client-go GET
// counter (every controller in the leader pod, not just SnapshotContent), so the absolute number includes a
// constant background. Both scrapes are pinned to the leader pod (reconciliation runs only there), so the
// delta is well-defined even with HA replicas. Compare the SAME scenario across two deployed images —
// baseline (main) vs new (swap branch) — and attribute the delta-of-deltas to the cache swaps; the
// background cancels. The spec always LOGS the figure; it hard-asserts an upper bound only when
// E2E_GET_LOAD_MAX_PER_SEC is set (the new run).
func getLoadSpecs() {
	Context("GET-load measurement (vol-tree capture wave)", func() {
		var (
			srcNS string
			sc    string
		)

		BeforeAll(func() {
			if !suiteCfg.volumeData {
				Skip("E2E_VOLUME_DATA not set: skipping the GET-load measurement (needs the real capture wave)")
			}
			sc = suiteCfg.storageClass
			srcNS = uniqueNS("getload")

			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
			defer cancel()

			By("Provisioning a thin, snapshot-capable default StorageClass via storage-e2e (" + sc + ")")
			_, err := testkit.EnsureDefaultStorageClass(ctx, suiteRestCfg, testkit.DefaultStorageClassConfig{
				StorageClassName:     sc,
				LVMType:              "Thin",
				ThinPoolName:         "thinpool",
				BaseKubeconfig:       suiteClusterResources.BaseKubeconfig,
				VMNamespace:          suiteCfg.vmNamespace,
				BaseStorageClassName: suiteCfg.baseStorageClass,
			})
			Expect(err).NotTo(HaveOccurred(), "provision default StorageClass")

			By("Wiring the StorageClass to a VolumeSnapshotClass for the local CSI driver")
			Expect(ensureStorageClassVolumeSnapshotClass(ctx, sc)).To(Succeed())

			By("Creating the source namespace and applying the full PVC source")
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			Expect(applyObjects(ctx, buildVolumeSource(srcNS, sc), srcNS)).To(Succeed())

			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, srcNS)
			})

			By("Starting the source probe Pod and waiting for it to run (binds all three PVCs)")
			_, err = suiteClientset.CoreV1().Pods(srcNS).Create(ctx, probePodSpec(srcNS, vdProbePod, []string{vdPVCRoot, vdPVCDisk, vdPVCStandalone}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create source probe pod")
			Expect(waitPodRunning(ctx, srcNS, vdProbePod, 10*time.Minute)).To(Succeed())
		})

		It("reports REST GET load across the capture wave (log; optional hard bound via E2E_GET_LOAD_MAX_PER_SEC)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*suiteCfg.captureReadyTO+15*time.Minute)
			defer cancel()

			By("Resolving the reconciliation leader pod and pinning both scrapes to it")
			leaderPod, err := leaderControllerPod(ctx)
			Expect(err).NotTo(HaveOccurred(), "resolve controller leader pod")
			metricsPath := podMetricsPath(leaderPod)
			GinkgoWriter.Printf("GET-load: pinning /metrics scrapes to leader pod %q\n", leaderPod)

			By("Scraping the leader /metrics counter BEFORE creating the root Snapshot")
			before, err := scrapeRestClientGETs(ctx, metricsPath)
			Expect(err).NotTo(HaveOccurred(), "scrape baseline rest_client_requests_total{GET}")
			// windowStart anchors the rate denominator to the SAME scrape-to-scrape window the counter delta
			// spans, so GET/sec is not skewed by GETs accrued outside the create->Ready interval.
			windowStart := time.Now()

			// Background timeline: surfaces where the capture wave spends time alongside the GET-load figure.
			tl := startCaptureTimeline(srcNS)
			defer tl.stop()

			By("Creating the root Snapshot and timing the wave to the FIRST Ready=True")
			start := time.Now()
			Expect(createRootSnapshot(ctx, srcNS, getLoadRootSnapshot)).To(Succeed())
			_, err = waitSnapshotReady(ctx, srcNS, getLoadRootSnapshot, 4*suiteCfg.captureReadyTO+10*time.Minute)
			Expect(err).NotTo(HaveOccurred(), "root Snapshot reached Ready=True")
			waveDur := time.Since(start)

			By("Confirming the reconciliation leader did not move during the wave (else the per-pod delta is invalid)")
			leaderAfter, err := leaderControllerPod(ctx)
			Expect(err).NotTo(HaveOccurred(), "re-resolve controller leader pod")
			Expect(leaderAfter).To(Equal(leaderPod),
				"controller leader moved %q -> %q during the capture wave: the pinned-pod GET delta would read a flat standby counter (false low). Re-run the measurement.",
				leaderPod, leaderAfter)

			By("Scraping the SAME leader pod's counter immediately AFTER the first Ready=True")
			after, err := scrapeRestClientGETs(ctx, metricsPath)
			Expect(err).NotTo(HaveOccurred(), "scrape post-wave rest_client_requests_total{GET}")
			windowDur := time.Since(windowStart)
			Expect(after).To(BeNumerically(">=", before),
				"per-pod GET counter must be monotonic non-decreasing (if it dropped, the leader changed pods mid-wave: %q)", leaderPod)

			deltaGet := after - before
			// Normalize over the scrape-to-scrape window (matches the counter delta exactly); waveDur is logged
			// separately as the create->first-Ready capture duration for context.
			perSec := 0.0
			if s := windowDur.Seconds(); s > 0 {
				perSec = deltaGet / s
			}

			GinkgoWriter.Printf("\n==== GET-load measurement (vol-tree capture wave) ====\n")
			GinkgoWriter.Printf("  rest_client_requests_total{method=GET}: before=%.0f after=%.0f\n", before, after)
			GinkgoWriter.Printf("  measured window (scrape -> scrape): %s\n", windowDur.Round(time.Millisecond))
			GinkgoWriter.Printf("  capture wave (create -> first Ready=True): %s\n", waveDur.Round(time.Millisecond))
			GinkgoWriter.Printf("  Δget=%.0f  (%.2f GET/sec over the measured window)\n", deltaGet, perSec)
			GinkgoWriter.Printf("  leader pod: %s (both scrapes pinned here)\n", leaderPod)
			GinkgoWriter.Printf("  NOTE: counter is PER-PROCESS (all controllers in the leader pod). Compare the SAME\n")
			GinkgoWriter.Printf("        scenario baseline(main image) vs new(swap image); the constant background cancels.\n")
			GinkgoWriter.Printf("======================================================\n\n")
			AddReportEntry("get-load",
				fmt.Sprintf("Δget=%.0f perSec=%.2f windowDur=%s waveDur=%s before=%.0f after=%.0f", deltaGet, perSec, windowDur.Round(time.Millisecond), waveDur.Round(time.Millisecond), before, after))

			if raw := os.Getenv(getLoadMaxPerSecEnv); raw != "" {
				maxPerSec, perr := strconv.ParseFloat(raw, 64)
				Expect(perr).NotTo(HaveOccurred(), "parse %s=%q", getLoadMaxPerSecEnv, raw)
				Expect(perSec).To(BeNumerically("<=", maxPerSec),
					"GET/sec across the capture wave (%.2f) exceeded %s=%.2f", perSec, getLoadMaxPerSecEnv, maxPerSec)
			}
		})
	})
}
