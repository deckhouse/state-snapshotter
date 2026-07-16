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
	"sort"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// readyFlapRootSnapshot is the root Snapshot name for the flap-detector run. It reuses the shared vol-tree
// fixture wiring (buildVolumeSource) but lives in its own namespace, so the name does not collide with the
// phase-3 volume-data run that captures the same fixture.
const readyFlapRootSnapshot = "vol-tree-flap"

// --- d1: Ready-transition recorder (watch-based, monotonicity assertion) ---

// stateSample is one observed, deduplicated state of a watched object. status carries the primary signal
// monotonicity is checked against (the Ready condition status: "True"/"False"/""), while desc is the full
// human-readable line printed in the ledger on failure.
type stateSample struct {
	status string
	desc   string
	at     time.Time
}

// objStateRecorder consumes a watch stream and records the in-order, deduplicated state transitions of the
// single object that satisfies its match predicate. It is the e2e flap detector's core: every status write
// on a Kubernetes object is a separate resourceVersion, so a watch (client-go applies backpressure, it does
// not drop) sees the full True->False->True ledger that an interval poll would coalesce away. The recorder
// is safe for concurrent use: the watch goroutine appends while the spec goroutine reads.
type objStateRecorder struct {
	mu      sync.Mutex
	samples []stateSample

	first sync.Once
	ready chan struct{} // closed on the FIRST sample whose readyTrue is true
}

// observe records state when it differs from the last recorded sample (dedup by full desc, so a reason flip
// within the same status is still captured) and, on the first ready-true observation, fires the signal.
func (r *objStateRecorder) observe(status, desc string, readyTrue bool) {
	r.mu.Lock()
	n := len(r.samples)
	if n == 0 || r.samples[n-1].desc != desc {
		r.samples = append(r.samples, stateSample{status: status, desc: desc, at: time.Now()})
	}
	r.mu.Unlock()
	if readyTrue {
		r.first.Do(func() { close(r.ready) })
	}
}

// ledger returns a copy of the recorded samples for printing/assertion.
func (r *objStateRecorder) ledger() []stateSample {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]stateSample, len(r.samples))
	copy(out, r.samples)
	return out
}

// waitReadyTrue blocks until the first ready-true sample is observed, the timeout elapses, or ctx is
// cancelled (so a suite interrupt / FailFast does not hang for the whole timeout).
func (r *objStateRecorder) waitReadyTrue(ctx context.Context, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-r.ready:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// extractFn pulls the (status, desc, readyTrue) triple from a watched object. status is the value
// monotonicity is asserted against; desc is the printed line; readyTrue fires the first-ready signal.
type extractFn func(obj *unstructured.Unstructured) (status, desc string, readyTrue bool)

// matchFn selects the single object a recorder tracks (Snapshot by name; SnapshotContent by back-reference,
// since the content name is derived from the Snapshot UID, not its name).
type matchFn func(obj *unstructured.Unstructured) bool

// snapshotReadyExtract reports the Snapshot Ready condition for the recorder. readyTrue is set only when
// Ready=True, so the recorder signals exactly the first Ready=True a consumer would act on.
func snapshotReadyExtract(obj *unstructured.Unstructured) (string, string, bool) {
	st, reason, found := conditionStatus(obj, condReady)
	if !found {
		return "", "Ready=<absent>", false
	}
	return st, fmt.Sprintf("Ready=%s/%s", st, reason), st == "True"
}

// contentDiagExtract reports a SnapshotContent's Ready and ChildrenReady conditions (status + reason). It
// never fires the ready signal (diagnostic recorder only); the ChildrenReady reason makes the fail-closed
// orphan-link gate (ChildrenLinkPending -> satisfied) visible in the printed ledger on a flap, replacing
// the removed residualVolumeCapture.phase latch.
func contentDiagExtract(obj *unstructured.Unstructured) (string, string, bool) {
	st, reason, _ := conditionStatus(obj, condReady)
	childStatus, childReason, _ := conditionStatus(obj, condChildrenReady)
	desc := fmt.Sprintf("Ready=%s/%s ChildrenReady=%s/%s", st, reason, childStatus, childReason)
	return st, desc, false
}

// contentChildRefsSet reads status.childrenSnapshotContentRefs from a SnapshotContent and returns the
// deterministic, order-independent set signature (sorted, comma-joined child names; "" when empty/absent).
// It is the observable for the Block 4 frozen-set detector: every distinct value is one recorded transition.
func contentChildRefsSet(obj *unstructured.Unstructured) string {
	refs, _, _ := unstructured.NestedSlice(obj.Object, "status", "childrenSnapshotContentRefs")
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		m, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		if n, ok := m["name"].(string); ok && n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// contentChildRefsExtract reports a SnapshotContent's status.childrenSnapshotContentRefs set for the frozen
// detector. status IS the set signature (so the recorder dedups per distinct set and the frozen assertion
// runs against it); desc prints the members; it never fires the ready signal (diagnostic recorder only).
func contentChildRefsExtract(obj *unstructured.Unstructured) (string, string, bool) {
	set := contentChildRefsSet(obj)
	return set, fmt.Sprintf("childrenSnapshotContentRefs=[%s]", set), false
}

// assertChildrenRefsFrozen fails the spec if status.childrenSnapshotContentRefs ever changed after it first
// became non-empty (Block 4, INV-CONTENT-CHILDREN-2: the set is frozen once populated). The ONLY allowed
// transition is empty -> complete: a first non-empty set latches "frozen"; any later sample that differs
// from it — a grow, a shrink (including back to empty), a reorder-as-different-membership, or a replace — is
// a violation. This is the on-cluster counterpart to the CEL admission test: it proves the SOLE writer (the
// aggregator, all-or-nothing) never even attempts a non-monotonic edge write during a real capture, so the
// set is published exactly once and stays stable through the whole Ready convergence.
func assertChildrenRefsFrozen(label string, samples []stateSample) {
	GinkgoHelper()
	frozen := ""
	frozenAt := -1
	for i, s := range samples {
		set := s.status
		if frozen == "" {
			if set != "" {
				frozen = set
				frozenAt = i
			}
			continue
		}
		if set != frozen {
			Fail(fmt.Sprintf("%s childrenSnapshotContentRefs changed after freezing: latched [%s] at transition %d, then observed [%s] at transition %d (the complete child set is immutable once written)\n%s",
				label, frozen, frozenAt, set, i, formatLedger(label, samples)))
		}
	}
}

// rootContentMatch matches the namespace-root SnapshotContent of the given root Snapshot via its immutable
// spec.snapshotRef back-reference (the content name itself is ns-<uid>, unknown before creation).
func rootContentMatch(snapNS, snapName string) matchFn {
	return func(obj *unstructured.Unstructured) bool {
		kind, _, _ := unstructured.NestedString(obj.Object, "spec", "snapshotRef", "kind")
		name, _, _ := unstructured.NestedString(obj.Object, "spec", "snapshotRef", "name")
		ns, _, _ := unstructured.NestedString(obj.Object, "spec", "snapshotRef", "namespace")
		return kind == "Snapshot" && name == snapName && ns == snapNS
	}
}

// startObjStateRecorder opens a watch (ns="" addresses cluster-scoped kinds) and records the state
// transitions of the object satisfying match until stop is called. It MUST be started BEFORE the action
// that drives the transitions (e.g. creating the root Snapshot), so the very first Ready=True — and any
// subsequent flap — is observed. The watch is re-established on server-side close so a capture longer than
// the apiserver watch timeout is still fully recorded; on reconnect it resyncs to the current state (a
// sub-interval edge lost across a reconnect is backstopped by detector B). The caller must always invoke
// stop to release the watch.
func startObjStateRecorder(parentCtx context.Context, gvr schema.GroupVersionResource, ns string, match matchFn, extract extractFn) (*objStateRecorder, func(), error) {
	ctx, cancel := context.WithCancel(parentCtx)
	rec := &objStateRecorder{ready: make(chan struct{})}

	var resourceClient watchStarter = suiteDyn.Resource(gvr)
	if ns != "" {
		resourceClient = suiteDyn.Resource(gvr).Namespace(ns)
	}

	// Confirm the initial watch opens before returning, so a setup error surfaces to the caller rather
	// than silently in the background goroutine.
	w, err := resourceClient.Watch(ctx, metav1.ListOptions{})
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("watch %s for recorder: %w", gvr.Resource, err)
	}

	go func() {
		defer cancel()
		for {
			rec.consume(w, match, extract)
			w.Stop()
			if ctx.Err() != nil {
				return
			}
			// Server closed the watch (timeout/compaction): re-establish from the current state. A small
			// unconditional backoff prevents a tight reconnect spin if a re-opened watch closes immediately.
			if !sleepCtx(ctx, 500*time.Millisecond) {
				return
			}
			next, rerr := resourceClient.Watch(ctx, metav1.ListOptions{})
			if rerr != nil {
				if !sleepCtx(ctx, time.Second) {
					return
				}
				continue
			}
			w = next
		}
	}()

	return rec, cancel, nil
}

// watchStarter is the subset of the dynamic client used by the recorder (namespaced or cluster-scoped).
type watchStarter interface {
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
}

// consume drains one watch until its channel closes, recording matching ADDED/MODIFIED events.
func (r *objStateRecorder) consume(w watch.Interface, match matchFn, extract extractFn) {
	for ev := range w.ResultChan() {
		if ev.Type == watch.Error {
			return
		}
		if ev.Type != watch.Added && ev.Type != watch.Modified {
			continue
		}
		obj, ok := ev.Object.(*unstructured.Unstructured)
		if !ok || !match(obj) {
			continue
		}
		status, desc, readyTrue := extract(obj)
		r.observe(status, desc, readyTrue)
	}
}

// formatLedger renders a recorder ledger as a multi-line block for failure diagnostics.
func formatLedger(label string, samples []stateSample) string {
	if len(samples) == 0 {
		return label + ": <no samples recorded>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s ledger (%d transitions):\n", label, len(samples))
	start := samples[0].at
	for i, s := range samples {
		fmt.Fprintf(&b, "  [%2d] +%-10s %s\n", i, s.at.Sub(start).Round(time.Millisecond), s.desc)
	}
	return b.String()
}

// assertReadyMonotonic fails the spec if the recorded Ready status ever regresses away from True (the flap
// this feature removes). A False -> True (first readiness) is allowed; once True is observed, any later
// non-True sample (False or Unknown) is a violation.
func assertReadyMonotonic(label string, samples []stateSample) {
	GinkgoHelper()
	sawTrue := false
	for i, s := range samples {
		if s.status == "True" {
			sawTrue = true
			continue
		}
		if sawTrue {
			Fail(fmt.Sprintf("%s Ready regressed from True to %q at transition %d (%s)\n%s",
				label, s.status, i, s.desc, formatLedger(label, samples)))
		}
	}
}

// --- d2: Ready-flap detector spec on the mixed orphan+domain vol-tree fixture ---

// readyFlapSpecs registers the Ready-flap detector (env-gated by E2E_VOLUME_DATA). It captures the mixed
// vol-tree fixture (domain children vm-1/disk-vm/disk-standalone + orphan PVC demo-pvc) that historically
// triggered the Ready True->False->True flap, and asserts two complementary detectors:
//
//	A. Monotonicity: a watch opened BEFORE the root Snapshot exists records every Ready transition; after
//	   the first Ready=True there is never a False.
//	B. Restore-on-first-Ready: the instant the watch signals the first Ready=True, the consumer's restore
//	   read (manifests-with-data-restoration) is issued; it must NOT 409 (the original flap symptom) and the
//	   captured content must carry data bindings for all three source PVCs.
func readyFlapSpecs() {
	Context("Ready-flap detector (mixed orphan+domain tree)", func() {
		var (
			srcNS     string
			restoreNS string
			sc        string
		)

		BeforeAll(func() {
			if !suiteCfg.volumeData {
				Skip("E2E_VOLUME_DATA not set: skipping the Ready-flap detector (needs real volume data)")
			}
			sc = suiteCfg.storageClass
			srcNS = uniqueNS("flap")

			// SC provisioning + module enablement is the slow part; mirror the phase-3 setup. The default
			// StorageClass + VolumeSnapshotClass wiring is idempotent, so it is safe even when the phase-3
			// volume-data run already provisioned the same class earlier in the suite.
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

			By("Pre-creating the restore namespace (so detector B can read restore manifests without setup latency)")
			restoreNS = uniqueNS("flap-restore")
			Expect(ensureNamespace(ctx, restoreNS)).To(Succeed())

			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, srcNS)
				deleteNamespace(cctx, restoreNS)
			})

			By("Starting the source probe Pod and waiting for it to run (binds all three PVCs)")
			_, err = suiteClientset.CoreV1().Pods(srcNS).Create(ctx, probePodSpec(srcNS, vdProbePod, []string{vdPVCRoot, vdPVCDisk, vdPVCStandalone}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create source probe pod")
			Expect(waitPodRunning(ctx, srcNS, vdProbePod, 10*time.Minute)).To(Succeed())
		})

		It("never flaps Ready True->False and restores cleanly on the first Ready=True", func() {
			// Budget: capture (LVM snapshot creation) is fast, but the gated first Ready=True waits for the
			// residual orphan-PVC wave. The parent ctx must cover the SEQUENTIAL sub-budgets below (first-True
			// wait 3X+5m, then content-ready 2X+5m, then dataRefs 2X+5m, plus the settle window) so a slow or
			// regressing controller surfaces each step's own timeout instead of a generic deadline.
			ctx, cancel := context.WithTimeout(context.Background(), 9*suiteCfg.captureReadyTO+20*time.Minute)
			defer cancel()

			// Background capture timeline: surfaces where the volume-data snapshot creation spends time.
			tl := startCaptureTimeline(srcNS)
			defer tl.stop()

			By("Opening Ready recorders BEFORE the root Snapshot exists (watch backpressure captures every transition)")
			snapRec, snapStop, err := startObjStateRecorder(ctx, snapshotGVR, srcNS,
				func(o *unstructured.Unstructured) bool { return o.GetName() == readyFlapRootSnapshot },
				snapshotReadyExtract)
			Expect(err).NotTo(HaveOccurred(), "start Snapshot Ready recorder")
			defer snapStop()

			// Diagnostic recorder on the cluster-scoped root content (matched by its immutable back-reference
			// to this Snapshot, since the content name is derived from the Snapshot UID). It records Ready +
			// ChildrenReady (status/reason) for the failure ledger and never fires a signal.
			contentRec, contentStop, err := startObjStateRecorder(ctx, snapshotContentGVR, "",
				rootContentMatch(srcNS, readyFlapRootSnapshot), contentDiagExtract)
			Expect(err).NotTo(HaveOccurred(), "start SnapshotContent diagnostic recorder")
			defer contentStop()

			// Block 4 frozen-set detector: record every distinct childrenSnapshotContentRefs value the root
			// content passes through (empty -> complete is the only legal transition; the aggregator is the
			// sole, all-or-nothing edge writer). Opened before the content exists so the very first write is
			// captured, and asserted at the settle step to prove the set never flapped/shrank/grew.
			childRefsRec, childRefsStop, err := startObjStateRecorder(ctx, snapshotContentGVR, "",
				rootContentMatch(srcNS, readyFlapRootSnapshot), contentChildRefsExtract)
			Expect(err).NotTo(HaveOccurred(), "start SnapshotContent childrenSnapshotContentRefs recorder")
			defer childRefsStop()

			By("Creating the root Snapshot over the mixed PVC tree")
			Expect(createRootSnapshot(ctx, srcNS, readyFlapRootSnapshot)).To(Succeed())

			By("Detector B: waiting for the FIRST Ready=True, then immediately issuing the restore read")
			Expect(snapRec.waitReadyTrue(ctx, 3*suiteCfg.captureReadyTO+5*time.Minute)).To(BeTrue(),
				"root Snapshot never reached Ready=True\n%s\n%s",
				formatLedger("Snapshot", snapRec.ledger()), formatLedger("SnapshotContent", contentRec.ledger()))

			// Fire the consumer's restore read the instant the first Ready=True is observed (event-driven,
			// not a poll), to land inside the flap window on the unfixed controller.
			restorePath := coreSnapshotSubPath(srcNS, readyFlapRootSnapshot, subManifestsRestore)
			body, restoreErr := aggGet(ctx, restorePath, map[string]string{"targetNamespace": restoreNS})
			Expect(apierrors.IsConflict(restoreErr)).To(BeFalse(),
				"restore-with-data on the FIRST Ready=True returned 409 Conflict — the Ready-flap symptom (Ready=True observed before the residual wave finished)\nerr=%v\n%s\n%s",
				restoreErr, formatLedger("Snapshot", snapRec.ledger()), formatLedger("SnapshotContent", contentRec.ledger()))
			Expect(restoreErr).NotTo(HaveOccurred(), "restore-with-data GET on first Ready=True; body=%s", truncate(body, 1024))

			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			Expect(objs).NotTo(BeEmpty(), "restore payload must not be empty on the first Ready=True")

			By("Resolving the bound SnapshotContent and waiting for the full Ready tree to settle")
			content, err := waitSnapshotReady(ctx, srcNS, readyFlapRootSnapshot, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, content, 2*suiteCfg.captureReadyTO+5*time.Minute)).To(Succeed())

			By("Asserting data bindings for all three captured PVCs are published")
			bindings, err := waitContentDataRefs(ctx, content, []string{vdPVCRoot, vdPVCDisk, vdPVCStandalone}, 2*suiteCfg.captureReadyTO+5*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"all three PVC dataRefs must be present once Ready=True is final\n%s",
				formatLedger("SnapshotContent", contentRec.ledger()))
			for _, b := range bindings {
				GinkgoWriter.Printf("  dataRef: pvc=%s vsc=%s\n", b.pvc, b.vsc)
			}

			By("Settling, then asserting detector A: Ready never flapped True->False")
			// Give the controller a window to expose a late flap (an orphan child linked after a premature
			// Ready=True would drop ChildrenReady) before reading the final ledger.
			sleepCtx(ctx, 30*time.Second)
			snapLedger := snapRec.ledger()
			GinkgoWriter.Printf("%s\n", formatLedger("Snapshot "+srcNS+"/"+readyFlapRootSnapshot, snapLedger))
			GinkgoWriter.Printf("%s\n", formatLedger("SnapshotContent "+content, contentRec.ledger()))
			assertReadyMonotonic("Snapshot "+srcNS+"/"+readyFlapRootSnapshot, snapLedger)

			By("Asserting Block 4 frozen set: childrenSnapshotContentRefs was written once and never flapped/shrank")
			childRefsLedger := childRefsRec.ledger()
			GinkgoWriter.Printf("%s\n", formatLedger("SnapshotContent "+content+" childrenSnapshotContentRefs", childRefsLedger))
			assertChildrenRefsFrozen("SnapshotContent "+content, childRefsLedger)
		})
	})
}
