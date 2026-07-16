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
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// getLoadRootSnapshot is the base name of the root Snapshot for the GET-load run. Each iteration appends its
// index ("-1", "-2", ...). It reuses the shared vol-tree fixture (buildVolumeSource) but lives in its own
// namespace/name so it does not collide with the phase-3 or flap-detector runs over the same fixture.
const getLoadRootSnapshot = "vol-tree-getload"

// controllerLeaseName is the controller-runtime leader-election Lease (LeaderElectionID = "controller")
// in the module namespace. Its holderIdentity is "<podName>_<uuid>", which identifies the single pod that
// actually runs reconciliation.
const controllerLeaseName = "controller"

// getLoadMaxPerSecEnv optionally enforces an upper bound on the MEAN GET/sec across the measured waves. It is
// left UNSET for the baseline run (log only) and set to the baseline figure for the new run, so the same spec
// hard-asserts the improvement only when the operator opts in (the counter is process-wide; see the spec).
const getLoadMaxPerSecEnv = "E2E_GET_LOAD_MAX_PER_SEC"

// getLoadIterationsEnv / getLoadWarmupEnv tune the repeat-and-average protocol; see getLoadSpecs.
//   - ITERATIONS: how many capture waves to run back-to-back over the shared source (default 5).
//   - WARMUP: how many leading waves to MEASURE but EXCLUDE from the mean (default 1). The first wave after a
//     deploy is dearer (cold informer caches, lazy first LIST), so dropping it removes that bias.
const (
	getLoadIterationsEnv     = "E2E_GET_LOAD_ITERATIONS"
	getLoadWarmupEnv         = "E2E_GET_LOAD_WARMUP"
	getLoadDefaultIterations = 5
	getLoadDefaultWarmup     = 1
)

// getLoadInterIterationSettle is a brief quiesce between iterations so the next wave's BEFORE scrape is not
// polluted by the tail GETs of the wave that just finished.
const getLoadInterIterationSettle = 10 * time.Second

// errGetLoadSampleInvalid marks a wave whose per-pod GET delta cannot be trusted (leader election moved to a
// different pod, or the leader process restarted mid-wave, so the pinned counter is discontinuous). Such a
// wave is skipped rather than failing the whole run.
var errGetLoadSampleInvalid = errors.New("GET-load sample invalid (leader moved or process restarted mid-wave)")

// getLoadSample is one capture wave's GET-load measurement.
type getLoadSample struct {
	iter      int
	deltaGet  float64
	windowDur time.Duration
	waveDur   time.Duration
	perSec    float64
	warmup    bool
}

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

// controllerMetricsPortName is the deployment container port exposing controller-runtime's default
// plain-HTTP metrics server. It is resolved to a NUMERIC port for the proxy path (see controllerMetricsPort).
const controllerMetricsPortName = "metrics"

// controllerMetricsPort resolves the NUMERIC container port serving /metrics on the given controller pod.
// The apiserver pod-proxy must address this port by number: a named-port proxy path
// (pods/<pod>:metrics/proxy/...) resets the stream with an http2 INTERNAL_ERROR against the plain-HTTP
// metrics server, whereas the numeric form (pods/<pod>:8080/proxy/...) works. We read the number from the
// pod spec rather than hardcoding it so a deployment port change surfaces as a clear error, not a silent miss.
func controllerMetricsPort(ctx context.Context, pod string) (int32, error) {
	p, err := suiteClientset.CoreV1().Pods(d8ModuleNS).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("get controller pod %s/%s: %w", d8ModuleNS, pod, err)
	}
	for _, c := range p.Spec.Containers {
		for _, cp := range c.Ports {
			if cp.Name == controllerMetricsPortName {
				return cp.ContainerPort, nil
			}
		}
	}
	return 0, fmt.Errorf("controller pod %s/%s exposes no container port named %q", d8ModuleNS, pod, controllerMetricsPortName)
}

// podMetricsPath is the apiserver pod-proxy path to one controller pod's plain-HTTP /metrics endpoint
// (controller-runtime's default metrics server). The port is addressed by NUMBER, not by name: a named-port
// proxy path resets the stream against the plain-HTTP server (see controllerMetricsPort). No Prometheus/RBAC
// setup is needed: the cluster-admin suite kubeconfig proxies it.
func podMetricsPath(pod string, port int32) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:%d/proxy/metrics", d8ModuleNS, pod, port)
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

// getLoadIntEnv reads a non-negative integer tuning knob from env, falling back to def when unset/blank. A
// malformed or negative value fails loudly rather than silently reverting to the default.
func getLoadIntEnv(name string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %s=%q: %w", name, raw, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("%s=%d must be >= 0", name, v)
	}
	return v, nil
}

// summarize returns the mean, median, sample standard deviation, min and max of xs (which must be non-empty).
func summarize(xs []float64) (mean, median, stddev, lo, hi float64) {
	n := len(xs)
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	lo, hi = sorted[0], sorted[n-1]
	var sum float64
	for _, x := range sorted {
		sum += x
	}
	mean = sum / float64(n)
	if n%2 == 1 {
		median = sorted[n/2]
	} else {
		median = (sorted[n/2-1] + sorted[n/2]) / 2
	}
	if n > 1 {
		var ss float64
		for _, x := range sorted {
			d := x - mean
			ss += d * d
		}
		stddev = math.Sqrt(ss / float64(n-1))
	}
	return
}

// measureGetLoadWave runs one capture wave (create root Snapshot -> first Ready=True) and measures the
// per-pod rest_client_requests_total{GET} delta over the scrape-to-scrape window, pinned to the current
// reconciliation leader. It returns errGetLoadSampleInvalid (a soft, skippable error) when the leader moves
// or the counter goes backwards mid-wave; any other error is hard.
func measureGetLoadWave(ctx context.Context, srcNS, snapName string, iter int) (getLoadSample, error) {
	leaderPod, err := leaderControllerPod(ctx)
	if err != nil {
		return getLoadSample{}, fmt.Errorf("resolve controller leader pod: %w", err)
	}
	metricsPort, err := controllerMetricsPort(ctx, leaderPod)
	if err != nil {
		return getLoadSample{}, fmt.Errorf("resolve controller metrics container port: %w", err)
	}
	metricsPath := podMetricsPath(leaderPod, metricsPort)
	GinkgoWriter.Printf("GET-load[iter %d]: pinning /metrics scrapes to leader pod %q (numeric metrics port %d)\n", iter, leaderPod, metricsPort)

	before, err := scrapeRestClientGETs(ctx, metricsPath)
	if err != nil {
		return getLoadSample{}, fmt.Errorf("scrape baseline rest_client_requests_total{GET}: %w", err)
	}
	// windowStart anchors the rate denominator to the SAME scrape-to-scrape window the counter delta spans,
	// so GET/sec is not skewed by GETs accrued outside the create->Ready interval.
	windowStart := time.Now()

	// Background timeline: surfaces where the capture wave spends time alongside the GET-load figure.
	tl := startCaptureTimeline(srcNS)
	defer tl.stop()

	start := time.Now()
	if err := createRootSnapshot(ctx, srcNS, snapName); err != nil {
		return getLoadSample{}, fmt.Errorf("create root Snapshot %s/%s: %w", srcNS, snapName, err)
	}
	if _, err := waitSnapshotReady(ctx, srcNS, snapName, 4*suiteCfg.captureReadyTO+10*time.Minute); err != nil {
		return getLoadSample{}, fmt.Errorf("root Snapshot %s reach Ready=True: %w", snapName, err)
	}
	waveDur := time.Since(start)

	leaderAfter, err := leaderControllerPod(ctx)
	if err != nil {
		return getLoadSample{}, fmt.Errorf("re-resolve controller leader pod: %w", err)
	}
	if leaderAfter != leaderPod {
		return getLoadSample{}, fmt.Errorf("%w: leader %q -> %q", errGetLoadSampleInvalid, leaderPod, leaderAfter)
	}

	after, err := scrapeRestClientGETs(ctx, metricsPath)
	if err != nil {
		return getLoadSample{}, fmt.Errorf("scrape post-wave rest_client_requests_total{GET}: %w", err)
	}
	windowDur := time.Since(windowStart)
	if after < before {
		return getLoadSample{}, fmt.Errorf("%w: per-pod GET counter dropped %.0f -> %.0f (leader pod %q restarted?)", errGetLoadSampleInvalid, before, after, leaderPod)
	}

	deltaGet := after - before
	perSec := 0.0
	if s := windowDur.Seconds(); s > 0 {
		perSec = deltaGet / s
	}
	return getLoadSample{
		iter:      iter,
		deltaGet:  deltaGet,
		windowDur: windowDur,
		waveDur:   waveDur,
		perSec:    perSec,
	}, nil
}

// getLoadSpecs registers the GET-load measurement spec. It is OPT-IN via E2E_GET_LOAD (off by default even
// when E2E_VOLUME_DATA is set, because the repeat-and-average run adds several minutes); it provisions its
// own thin StorageClass, so it does not piggyback on the phase-3 volume-data flow. It repeats the vol-tree
// capture wave (root Snapshot create -> first Ready=True) E2E_GET_LOAD_ITERATIONS times over a SHARED source
// and reports the per-second rest_client_requests_total{method="GET"} delta, averaged across the measured
// waves (the first E2E_GET_LOAD_WARMUP waves are run but excluded from the mean).
//
// Methodology (see the APIReader->cache batch plan): the controller exposes a PER-PROCESS client-go GET
// counter (every controller in the leader pod, not just SnapshotContent), so the absolute number includes a
// constant background. Both scrapes of each wave are pinned to the leader pod (reconciliation runs only
// there), so the delta is well-defined even with HA replicas. A single wave is noisy because its duration
// varies run to run; averaging several waves and reporting stddev/CV separates signal from that noise.
// Compare the MEAN across the SAME scenario for two deployed images - baseline (main) vs new (swap branch) -
// and attribute the difference to the cache swaps; the constant background (and any identical residual from
// not-yet-GC'd trees) cancels because the protocol is identical on both sides. The spec always LOGS the
// figures; it hard-asserts an upper bound on the mean only when E2E_GET_LOAD_MAX_PER_SEC is set (the new run).
func getLoadSpecs() {
	Context("GET-load measurement (vol-tree capture wave)", func() {
		var (
			srcNS string
			sc    string
		)

		BeforeAll(func() {
			if !suiteCfg.getLoad {
				Skip("E2E_GET_LOAD not set: skipping the GET-load measurement (opt-in; run with E2E_GET_LOAD=true). It provisions its own thin StorageClass, so it does NOT need E2E_VOLUME_DATA, but it does need the base-cluster knobs TEST_CLUSTER_NAMESPACE / TEST_CLUSTER_STORAGE_CLASS that the volume-data flow also uses.")
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

		It("reports mean REST GET load over N capture waves (log; optional hard bound via E2E_GET_LOAD_MAX_PER_SEC)", func() {
			iterations, err := getLoadIntEnv(getLoadIterationsEnv, getLoadDefaultIterations)
			Expect(err).NotTo(HaveOccurred())
			Expect(iterations).To(BeNumerically(">=", 1), "%s must be >= 1", getLoadIterationsEnv)
			warmup, err := getLoadIntEnv(getLoadWarmupEnv, getLoadDefaultWarmup)
			Expect(err).NotTo(HaveOccurred())
			if warmup > iterations-1 {
				// Always leave at least one measured wave (e.g. iterations=1 -> warmup=0 = old single-shot mode).
				clamped := iterations - 1
				if clamped < 0 {
					clamped = 0
				}
				GinkgoWriter.Printf("GET-load: warm-up=%d >= iterations=%d; clamping warm-up to %d so at least one wave is measured\n", warmup, iterations, clamped)
				warmup = clamped
			}

			// One generous deadline for the whole repeat loop: per-wave Ready wait cap times iterations, plus
			// setup/cleanup slack. The waves themselves finish in well under the cap; this is only a ceiling.
			perWaveCap := 4*suiteCfg.captureReadyTO + 10*time.Minute
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(iterations)*perWaveCap+10*time.Minute)
			defer cancel()

			var samples []getLoadSample
			for i := 1; i <= iterations; i++ {
				// Quiesce between waves so the next wave's BEFORE scrape is not polluted by the tail GETs of
				// the previous wave. Placed at the top (not after the body) so it applies even when the prior
				// iteration was SKIPPED via continue - a skipped wave still ran a full capture.
				if i > 1 {
					time.Sleep(getLoadInterIterationSettle)
				}

				isWarmup := i <= warmup
				label := "measured"
				if isWarmup {
					label = "warm-up, excluded from mean"
				}
				snapName := fmt.Sprintf("%s-%d", getLoadRootSnapshot, i)

				By(fmt.Sprintf("Iteration %d/%d (%s): capture wave over the shared source", i, iterations, label))
				s, werr := measureGetLoadWave(ctx, srcNS, snapName, i)

				// Best-effort cleanup: delete this iteration's root Snapshot so completed trees do not pile
				// reconcile background onto later iterations. Content GC is TTL-based and async; we do NOT wait
				// for it - the protocol is identical across baseline/new, so any residual background cancels in
				// the mean-vs-mean comparison.
				_ = suiteDyn.Resource(snapshotGVR).Namespace(srcNS).Delete(ctx, snapName, metav1.DeleteOptions{})

				if werr != nil {
					if errors.Is(werr, errGetLoadSampleInvalid) {
						GinkgoWriter.Printf("GET-load[iter %d]: SKIPPED - %v\n", i, werr)
						continue
					}
					Expect(werr).NotTo(HaveOccurred(), "iteration %d capture wave", i)
				}
				s.warmup = isWarmup
				samples = append(samples, s)
				GinkgoWriter.Printf("GET-load[iter %d]: Δget=%.0f window=%s wave=%s -> %.2f GET/sec (%s)\n",
					i, s.deltaGet, s.windowDur.Round(time.Millisecond), s.waveDur.Round(time.Millisecond), s.perSec, label)
			}

			var measuredPerSec, measuredDelta []float64
			for _, s := range samples {
				if s.warmup {
					continue
				}
				measuredPerSec = append(measuredPerSec, s.perSec)
				measuredDelta = append(measuredDelta, s.deltaGet)
			}
			Expect(measuredPerSec).NotTo(BeEmpty(),
				"no valid measured (non-warm-up) GET-load samples: ran %d iteration(s), warm-up=%d, valid total=%d (leader churn?). Re-run the measurement.", iterations, warmup, len(samples))

			meanPS, medianPS, stddevPS, minPS, maxPS := summarize(measuredPerSec)
			meanDelta, _, _, _, _ := summarize(measuredDelta)
			cv := 0.0
			if meanPS > 0 {
				cv = stddevPS / meanPS * 100
			}

			GinkgoWriter.Printf("\n==== GET-load measurement (vol-tree capture wave; %d iter, warm-up=%d) ====\n", iterations, warmup)
			for _, s := range samples {
				tag := ""
				if s.warmup {
					tag = "  (warm-up, excluded)"
				}
				GinkgoWriter.Printf("  iter %d: Δget=%.0f  window=%s  wave=%s  %.2f GET/sec%s\n",
					s.iter, s.deltaGet, s.windowDur.Round(time.Millisecond), s.waveDur.Round(time.Millisecond), s.perSec, tag)
			}
			GinkgoWriter.Printf("  ---- summary over %d measured wave(s) ----\n", len(measuredPerSec))
			GinkgoWriter.Printf("  GET/sec: mean=%.2f  median=%.2f  stddev=%.2f  CV=%.1f%%  min=%.2f  max=%.2f\n", meanPS, medianPS, stddevPS, cv, minPS, maxPS)
			GinkgoWriter.Printf("  Δget:    mean=%.0f\n", meanDelta)
			GinkgoWriter.Printf("  NOTE: counter is PER-PROCESS (all controllers in the leader pod). Compare mean(baseline,\n")
			GinkgoWriter.Printf("        main image) vs mean(new, swap image) for the SAME scenario; the constant background cancels.\n")
			GinkgoWriter.Printf("==========================================================================\n\n")
			AddReportEntry("get-load",
				fmt.Sprintf("iters=%d warmup=%d measured=%d meanPerSec=%.2f medianPerSec=%.2f stddev=%.2f cv=%.1f%% meanDelta=%.0f",
					iterations, warmup, len(measuredPerSec), meanPS, medianPS, stddevPS, cv, meanDelta))

			if raw := strings.TrimSpace(os.Getenv(getLoadMaxPerSecEnv)); raw != "" {
				maxPerSec, perr := strconv.ParseFloat(raw, 64)
				Expect(perr).NotTo(HaveOccurred(), "parse %s=%q", getLoadMaxPerSecEnv, raw)
				Expect(meanPS).To(BeNumerically("<=", maxPerSec),
					"mean GET/sec over %d measured wave(s) (%.2f) exceeded %s=%.2f", len(measuredPerSec), meanPS, getLoadMaxPerSecEnv, maxPerSec)
			}
		})
	})
}
