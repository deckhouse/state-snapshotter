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
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// envControllerRestart opts this spec IN; it is OFF by default, unlike the volume-data negative specs
// around it. The reason for the asymmetry is what the spec does to the cluster: it DELETES the running
// controller Pod, which is a cluster-wide singleton (the controller Deployment runs a single replica), so
// while it restarts NOTHING in the cluster reconciles — including whatever a neighbouring spec has in
// flight. That is safe in the strictly sequential suite this file is registered into, but it is not
// something a run should do unless it was asked for. Set E2E_CONTROLLER_RESTART=true (plus the
// volume-data flow, which it needs) to run it.
const envControllerRestart = "E2E_CONTROLLER_RESTART"

// Controller-restart fixture object names. They are distinct from every other data-tree fixture so this
// spec can run in the same suite pass without colliding on source object names. The VM wires a PVC-backed
// disk, so the captured tree is three levels deep — root ns Snapshot -> DemoVirtualMachineSnapshot ->
// DemoVirtualDiskSnapshot — which is what makes both "the tree did not duplicate" assertions non-trivial:
// there are parent edges at two levels to compare, and a real data leg (VolumeCaptureRequest) to watch.
const (
	ctrlRestartRootSnapshotName = "ctrl-restart-tree"
	ctrlRestartConfigMapName    = "ctrl-restart-cm"
	ctrlRestartPVCName          = "ctrl-restart-pvc"
	ctrlRestartVMName           = "ctrl-restart-vm"
	ctrlRestartDiskName         = "ctrl-restart-disk"
	ctrlRestartProbePod         = "ctrl-restart-probe"

	// ctrlRestartNonLeafNodes is the number of snapshot nodes the captured tree must contain: the root
	// Snapshot, the DemoVirtualMachineSnapshot child and the DemoVirtualDiskSnapshot grandchild. The
	// fixture has no orphan PVC, so it produces no CSI VolumeSnapshot visibility leaf and every node is
	// backed by a SnapshotContent. Asserting the count keeps the per-node edge comparison from passing
	// vacuously on a tree that came back smaller than it went in.
	ctrlRestartNonLeafNodes = 3
)

// buildControllerRestartSource returns the fixture source: a ConfigMap (the root's own manifest leg), one
// PVC on the snapshot-capable StorageClass, a DemoVirtualMachine wiring the disk, and the PVC-backed
// DemoVirtualDisk it adopts. It deliberately owns its object names instead of reusing another spec's
// fixture builder: the two live in different namespaces but are read by different assertions, and a shared
// builder would tie this spec's shape to changes made for that one.
func buildControllerRestartSource(ns, sc string) []*unstructured.Unstructured {
	configMap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      ctrlRestartConfigMapName,
			"namespace": ns,
		},
		"data": map[string]interface{}{"ctrl": "restart"},
	}}
	pvc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":      ctrlRestartPVCName,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"accessModes":      []interface{}{"ReadWriteOnce"},
			"storageClassName": sc,
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{"storage": "500Mi"},
			},
		},
	}}
	vm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualMachine",
		"metadata": map[string]interface{}{
			"name":      ctrlRestartVMName,
			"namespace": ns,
		},
		"spec": map[string]interface{}{"virtualDiskName": ctrlRestartDiskName},
	}}
	disk := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupVersion,
		"kind":       "DemoVirtualDisk",
		"metadata": map[string]interface{}{
			"name":      ctrlRestartDiskName,
			"namespace": ns,
		},
		// size + storageClassName satisfy the scratch-provisioning guards (the disk adopts the pre-created
		// PVC ctrlRestartPVCName; the values mirror that PVC).
		"spec": map[string]interface{}{
			"persistentVolumeClaimName": ctrlRestartPVCName,
			"size":                      "500Mi",
			"storageClassName":          sc,
		},
	}}
	return []*unstructured.Unstructured{configMap, pvc, vm, disk}
}

// --- the kill window: an observed object state, never a timer ---------------

// rootPlannedNotTerminalExtract reports the root Snapshot's domain capture phase together with its Ready
// condition, and fires the recorder's signal on the FIRST sample that satisfies the kill premise: the
// domain froze the plan (phase=Planned, capture barrier 1) while the capture is still running (Ready is
// not yet True). Reusing the recorder's signal — rather than polling on a schedule — is what makes the
// kill land on an observed state transition: the watch delivers the Planned write, and the spec reacts to
// that write instead of to a sleep whose length would have to be guessed per cluster.
//
// status carries the phase, so the printed ledger doubles as the root's phase timeline on failure.
func rootPlannedNotTerminalExtract(obj *unstructured.Unstructured) (string, string, bool) {
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "captureState", "domainSpecificController", "phase")
	st, reason, _ := conditionStatus(obj, condReady)
	desc := fmt.Sprintf("phase=%q Ready=%s/%s", phase, st, reason)
	inWindow := phase == string(storagev1alpha1.SnapshotCapturePhasePlanned) && st != "True"
	return phase, desc, inWindow
}

// --- controller kill / recovery --------------------------------------------

// killControllerLeaderPod deletes the Pod currently holding the controller's leader-election Lease, with
// grace period 0 so the process is killed rather than asked to shut down. The abrupt form is deliberate:
// a graceful stop lets the manager release its lease and finish the reconcile it is in, which is the ONE
// restart shape that does not model what this spec is about (an evicted or OOM-killed node process).
// Returns the name of the killed Pod.
func killControllerLeaderPod(ctx context.Context) (string, error) {
	pod, err := leaderControllerPod(ctx)
	if err != nil {
		return "", err
	}
	grace := int64(0)
	if err := suiteClientset.CoreV1().Pods(d8ModuleNS).Delete(ctx, pod, metav1.DeleteOptions{GracePeriodSeconds: &grace}); err != nil {
		return "", fmt.Errorf("delete controller pod %s/%s: %w", d8ModuleNS, pod, err)
	}
	return pod, nil
}

// waitControllerLeaderReplaced blocks until the controller's leader-election Lease is held by a Pod OTHER
// than killed and that Pod reports phase Running, then returns its name. Holding the Lease is the evidence
// that matters: a Pod can be Running with its manager still starting, but it cannot win leader election
// before the process is up, so "a new holder" means the controller is reconciling again. The Lease keeps
// naming the killed Pod until the old lease expires, which is why this polls instead of reading once.
func waitControllerLeaderReplaced(ctx context.Context, killed string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		pod, lerr := leaderControllerPod(ctx)
		var (
			p    *corev1.Pod
			gerr error
		)
		if lerr == nil && pod != killed {
			p, gerr = suiteClientset.CoreV1().Pods(d8ModuleNS).Get(ctx, pod, metav1.GetOptions{})
		}
		switch {
		case lerr != nil:
			last = fmt.Sprintf("lease read err=%v", lerr)
		case pod == killed:
			last = fmt.Sprintf("lease still held by the killed pod %q", pod)
		case gerr != nil:
			last = fmt.Sprintf("new leader %q get err=%v", pod, gerr)
		case p.Status.Phase != corev1.PodRunning:
			last = fmt.Sprintf("new leader %q phase=%s", pod, p.Status.Phase)
		default:
			return pod, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timeout waiting for a controller leader other than %s/%s; last: %s", d8ModuleNS, killed, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return "", ctx.Err()
		}
	}
}

// --- capture-request creation recorder --------------------------------------

// requestSighting is one observed capture-request object identity. The UID is the discriminator: MCR and
// VCR names are derived from the owning snapshot's UID (api/names.ManifestCaptureRequestName /
// VolumeCaptureRequestName), so a request that is re-created for the same node comes back under the SAME
// name with a NEW UID.
type requestSighting struct {
	kind string
	name string
	uid  string
	at   time.Time
}

// captureRequestRecorder collects every distinct capture-request object that existed in the watched
// namespace while it ran. It exists because the objects it watches are transient: the aggregator latches
// the capture leg on the snapshot and REAPS the request in the same pass, so by the time a capture is
// finished a list call sees nothing at all — and therefore cannot distinguish "the request was issued
// once" from "it was issued, reaped, and issued again while the controller was restarting". A watch sees
// each creation as it happens.
//
// Safe for concurrent use: one goroutine per watched kind appends while the spec goroutine reads.
type captureRequestRecorder struct {
	mu        sync.Mutex
	seen      map[string]bool // kind/name/uid
	sightings []requestSighting
}

// observe records an identity the recorder has not seen before.
func (r *captureRequestRecorder) observe(kind, name, uid string) {
	if name == "" || uid == "" {
		return
	}
	key := kind + "/" + name + "/" + uid
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen[key] {
		return
	}
	r.seen[key] = true
	r.sightings = append(r.sightings, requestSighting{kind: kind, name: name, uid: uid, at: time.Now()})
}

// ledger returns a copy of the recorded sightings for printing/assertion.
func (r *captureRequestRecorder) ledger() []requestSighting {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]requestSighting, len(r.sightings))
	copy(out, r.sightings)
	return out
}

// countByKind returns how many distinct objects of the given kind were observed. Printing it is what keeps
// the duplicate assertion below from being vacuously green on a run where the recorder saw nothing.
func (r *captureRequestRecorder) countByKind(kind string) int {
	n := 0
	for _, s := range r.ledger() {
		if s.kind == kind {
			n++
		}
	}
	return n
}

// reIssued returns, per "Kind/name", every UID observed under that name when there was more than one — i.e.
// the requests that were created a second time for the same owning node.
func (r *captureRequestRecorder) reIssued() map[string][]string {
	byName := map[string][]string{}
	for _, s := range r.ledger() {
		key := s.kind + "/" + s.name
		byName[key] = append(byName[key], s.uid)
	}
	out := map[string][]string{}
	for key, uids := range byName {
		if len(uids) > 1 {
			out[key] = uids
		}
	}
	return out
}

// watchedRequestKind pairs a printable kind with the resource the recorder watches for it.
type watchedRequestKind struct {
	kind string
	gvr  schema.GroupVersionResource
}

// startCaptureRequestRecorder opens one watch per kind in ns and records every object identity it sees
// until stop is called. It MUST be started BEFORE the root Snapshot is created, so the first request of
// the capture is observed. Watches are re-established on server-side close (a capture that outlives the
// apiserver watch timeout is still covered); on reconnect the apiserver replays the objects that exist at
// that moment, which the UID dedup absorbs. The caller must always invoke stop.
func startCaptureRequestRecorder(parentCtx context.Context, ns string, kinds []watchedRequestKind) (*captureRequestRecorder, func(), error) {
	ctx, cancel := context.WithCancel(parentCtx)
	rec := &captureRequestRecorder{seen: map[string]bool{}}

	for _, k := range kinds {
		var client watchStarter = suiteDyn.Resource(k.gvr).Namespace(ns)

		// Confirm the initial watch opens before returning, so a setup error surfaces to the caller rather
		// than silently in a background goroutine.
		w, err := client.Watch(ctx, metav1.ListOptions{})
		if err != nil {
			cancel()
			return nil, nil, fmt.Errorf("watch %s in %s for capture-request recorder: %w", k.gvr.Resource, ns, err)
		}

		go func(kind string, client watchStarter, w watch.Interface) {
			for {
				rec.consume(kind, w)
				w.Stop()
				if ctx.Err() != nil {
					return
				}
				// Server closed the watch (timeout/compaction): re-establish. A small unconditional backoff
				// prevents a tight reconnect spin if the re-opened watch closes immediately.
				if !sleepCtx(ctx, 500*time.Millisecond) {
					return
				}
				next, rerr := client.Watch(ctx, metav1.ListOptions{})
				if rerr != nil {
					if !sleepCtx(ctx, time.Second) {
						return
					}
					continue
				}
				w = next
			}
		}(k.kind, client, w)
	}

	return rec, cancel, nil
}

// consume drains one watch until its channel closes, recording every object identity it carries. Deleted
// events count too: they still prove the object existed under that UID, which is exactly what the
// re-issue assertion needs from an object that is reaped as soon as its leg latches.
func (r *captureRequestRecorder) consume(kind string, w watch.Interface) {
	for ev := range w.ResultChan() {
		if ev.Type == watch.Error {
			return
		}
		obj, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		r.observe(kind, obj.GetName(), string(obj.GetUID()))
	}
}

// formatRequestLedger renders the recorded sightings as a multi-line block for failure diagnostics.
func formatRequestLedger(ns string, sightings []requestSighting) string {
	if len(sightings) == 0 {
		return "capture requests in " + ns + ": <none observed>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "capture requests in %s (%d distinct object(s)):\n", ns, len(sightings))
	start := sightings[0].at
	for i, s := range sightings {
		fmt.Fprintf(&b, "  [%2d] +%-10s %s/%s uid=%s\n", i, s.at.Sub(start).Round(time.Millisecond), s.kind, s.name, s.uid)
	}
	return b.String()
}

// formatReIssued renders the re-issued requests (name -> the several UIDs it was created under) as sorted
// lines, so the failure log names exactly which node's request came back a second time.
func formatReIssued(reIssued map[string][]string) string {
	keys := make([]string, 0, len(reIssued))
	for k := range reIssued {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		uids := append([]string(nil), reIssued[k]...)
		sort.Strings(uids)
		fmt.Fprintf(&b, "  %s created under %d UIDs: %s\n", k, len(uids), strings.Join(uids, ", "))
	}
	return b.String()
}

// firstReadyTrueAt returns the time the recorder first observed Ready=True in a ledger, and whether it saw
// one at all.
func firstReadyTrueAt(samples []stateSample) (time.Time, bool) {
	for _, s := range samples {
		if s.status == "True" {
			return s.at, true
		}
	}
	return time.Time{}, false
}

// --- the spec ---------------------------------------------------------------

// controllerRestartSpecs registers the controller-liveness spec: the controller Pod is killed in the
// middle of a volume-data capture — after the root froze its plan, before the tree went terminal — and the
// capture must finish BY ITSELF, without duplicating the tree and without taking Ready back once it is
// given. Every other spec in this suite exercises the controller while it stays up; this is the level no
// unit or envtest can stand in for, because the failure it models is the process disappearing mid-flight
// (module rollout, eviction, OOM), not a code path.
//
// What each assertion is here to catch, stated as the defect that turns it red:
//
//   - the capture stalls after the restart (a reconcile that only ever ran off an in-memory watch event
//     and is never re-derived from cluster state): the root never reaches Ready and the wait times out.
//     Nothing is patched, annotated or re-created between the kill and that wait, so a green result means
//     the controller resumed on its own;
//   - the tree duplicates (the resumed capture re-plans children it already has): a node's
//     childrenSnapshotContentRefs stops matching its declared children exactly, or an edge appears twice;
//   - work is redone (a capture request re-issued for a node whose leg already latched): the same
//     request name is observed under a second UID;
//   - Ready flaps (the resumed capture publishes Ready=True, then a later pass takes it back): the
//     monotonicity ledger records a non-True sample after a True one.
//
// Gate: opt-in E2E_CONTROLLER_RESTART=true, on top of the volume-data flow (E2E_VOLUME_DATA), because it
// kills the single-replica controller Deployment's Pod — see envControllerRestart.
func controllerRestartSpecs() {
	Context("Controller restart mid-capture (resume without duplication)", func() {
		var (
			srcNS string
			sc    string
		)

		BeforeAll(func() {
			if !suiteCfg.volumeData || !envBool(os.Getenv(envControllerRestart)) {
				Skip("controller-restart spec disabled: it is opt-in; set " + envControllerRestart + "=true (and keep E2E_VOLUME_DATA on) to run it")
			}
			sc = suiteCfg.storageClass

			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
			defer cancel()

			By("Ensuring a thin, snapshot-capable StorageClass (" + sc + ")")
			Expect(ensureSnapshotStorageClass(ctx, sc)).To(Succeed())

			By("Wiring the StorageClass to a VolumeSnapshotClass for the local CSI driver")
			Expect(ensureStorageClassVolumeSnapshotClass(ctx, sc)).To(Succeed())

			srcNS = uniqueNS("p3-ctrl-restart")
			By("Creating the source namespace " + srcNS + " and applying the VM+disk source")
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			Expect(applyObjects(ctx, buildControllerRestartSource(srcNS, sc), srcNS)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, srcNS)
			})

			By("Starting the probe Pod so the PVC binds (WaitForFirstConsumer)")
			_, err := suiteClientset.CoreV1().Pods(srcNS).Create(ctx, probePodSpec(srcNS, ctrlRestartProbePod, []string{ctrlRestartPVCName}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create probe pod")
			Expect(waitPodRunning(ctx, srcNS, ctrlRestartProbePod, 10*time.Minute)).To(Succeed())
		})

		It("finishes the capture by itself after the controller Pod is killed mid-flight, without duplicating the tree or regressing Ready", func() {
			// Budget. Every bounded wait below runs SEQUENTIALLY and carries its own deadline, so a stall
			// surfaces at the step that stalled rather than as a generic context deadline. That only holds if
			// the parent outlives their SUM, so the sum is spelled out here and the parent is sized above it.
			// With X = suiteCfg.captureReadyTO, in execution order:
			//
			//	kill window            3X + 10m
			//	leader recovery         0X + 10m
			//	root Ready             3X + 10m
			//	content Ready          2X + 10m
			//	descendant tree walk    X +  5m
			//	children Ready          X +  5m
			//	content edges          3X        (X PER NODE, over the tree's 3 nodes — the per-node
			//	                                  Eventually is inside assertContentChildEdgesExact's loop)
			//	settle window                30s
			//	                    -------------
			//	worst case            13X + 50.5m
			//
			// The parent is 14X+60m. It exceeds that sum by X+9.5m — a margin that grows with X rather than
			// shrinking, so the guarantee holds for ANY value of the operator-set capture timeout and not
			// merely for its default. The margin also absorbs the few unbounded single API calls in between
			// (the pre-kill GET and the Pod delete). The three waits that run AFTER the tree is fully Ready
			// (tree walk, children Ready, content edges) are deliberately tighter than the convergence waits
			// above them: they read state that has already settled, so a generous budget there would buy
			// nothing and only push the parent up.
			ctx, cancel := context.WithTimeout(context.Background(), 14*suiteCfg.captureReadyTO+60*time.Minute)
			defer cancel()

			// Background capture timeline: on failure it shows where the resumed capture spent its time.
			tl := startCaptureTimeline(srcNS)
			defer tl.stop()

			matchRoot := func(o *unstructured.Unstructured) bool { return o.GetName() == ctrlRestartRootSnapshotName }

			By("Opening the recorders BEFORE the root Snapshot exists (watch backpressure captures every transition)")
			// Ready-transition recorder — the shared flap detector, not a private one: the monotonicity
			// question this spec asks about a restart is the same question ready_flap_test.go asks about a
			// mixed tree, and a second implementation of it would drift from that one.
			snapRec, snapStop, err := startObjStateRecorder(ctx, snapshotGVR, srcNS, matchRoot, snapshotReadyExtract)
			Expect(err).NotTo(HaveOccurred(), "start Snapshot Ready recorder")
			defer snapStop()

			// Kill-window recorder: fires the instant the root is observed at phase=Planned while still not
			// Ready. Its ledger is the root's phase timeline for the failure log.
			windowRec, windowStop, err := startObjStateRecorder(ctx, snapshotGVR, srcNS, matchRoot, rootPlannedNotTerminalExtract)
			Expect(err).NotTo(HaveOccurred(), "start Snapshot capture-phase recorder")
			defer windowStop()

			// Frozen-set recorder on the root content: every distinct childrenSnapshotContentRefs value the
			// root passes through. Empty -> complete is the only legal transition, and a restart is precisely
			// where a re-planned tree would show up as a second, different set.
			childRefsRec, childRefsStop, err := startObjStateRecorder(ctx, snapshotContentGVR, "",
				rootContentMatch(srcNS, ctrlRestartRootSnapshotName), contentChildRefsExtract)
			Expect(err).NotTo(HaveOccurred(), "start SnapshotContent childrenSnapshotContentRefs recorder")
			defer childRefsStop()

			// Capture-request recorder: transient MCR/VCR objects are reaped as soon as their leg latches,
			// so only a watch can tell "issued once" from "issued again after the restart".
			reqRec, reqStop, err := startCaptureRequestRecorder(ctx, srcNS, []watchedRequestKind{
				{kind: "ManifestCaptureRequest", gvr: manifestCaptureRequestGVR},
				{kind: "VolumeCaptureRequest", gvr: volumeCaptureRequestGVR},
			})
			Expect(err).NotTo(HaveOccurred(), "start capture-request recorder")
			defer reqStop()

			By("Creating the root Snapshot over the data-backed VM tree")
			Expect(createRootSnapshot(ctx, srcNS, ctrlRestartRootSnapshotName)).To(Succeed())

			By("Waiting for the kill window ON THE OBJECT: the root froze its plan (phase=Planned) and is not Ready yet")
			// waitReadyTrue is the recorder's generic "first flagged sample" signal; what counts as flagged is
			// decided by the extractor, and for THIS recorder the flag is the kill premise, not readiness. The
			// method carries the name its first caller gave it (see rootPlannedNotTerminalExtract).
			Expect(windowRec.waitReadyTrue(ctx, 3*suiteCfg.captureReadyTO+10*time.Minute)).To(BeTrue(),
				"the root Snapshot never showed phase=%s while still not Ready — there was no window to kill the controller in\n%s",
				storagev1alpha1.SnapshotCapturePhasePlanned, formatLedger("Snapshot "+srcNS+"/"+ctrlRestartRootSnapshotName, windowRec.ledger()))

			By("Re-reading the live root to confirm the window is still open")
			// LIMIT: this closes the window from one side only. Between this read and the apiserver
			// processing the delete below, the capture could still finish; the "first Ready=True came after
			// the kill" assertion at the end closes it from the other side (itself limited to when the test
			// process OBSERVED the transition, not when the apiserver wrote it).
			root, err := getResource(ctx, snapshotGVR, srcNS, ctrlRestartRootSnapshotName)
			Expect(err).NotTo(HaveOccurred(), "re-read the root Snapshot before killing the controller")
			readyStatus, readyReason, _ := conditionStatus(root, condReady)
			Expect(readyStatus).NotTo(Equal("True"),
				"the capture reached Ready=True/%s before the controller could be killed: this run did not exercise a mid-flight restart at all. The fixture must stay slow enough to leave a window — a data-backed tree, not a manifest-only one\n%s",
				readyReason, formatLedger("Snapshot "+srcNS+"/"+ctrlRestartRootSnapshotName, windowRec.ledger()))

			By("Killing the controller leader Pod (grace period 0) in " + d8ModuleNS)
			killed, err := killControllerLeaderPod(ctx)
			killedAt := time.Now()
			Expect(err).NotTo(HaveOccurred(), "kill the controller leader pod")
			GinkgoWriter.Printf("killed controller pod %s/%s at %s (root at Ready=%q/%q)\n",
				d8ModuleNS, killed, killedAt.Format(time.RFC3339Nano), readyStatus, readyReason)

			// The controller is a cluster-wide singleton every later spec depends on. Register the recovery
			// wait as a cleanup right after the kill — BEFORE the assertions below — so that a failure
			// anywhere later still ends with the suite either holding a live, leading controller or failing
			// loudly about it, instead of quietly leaving the next spec to time out against a dead one.
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Minute)
				defer ccancel()
				pod, cerr := waitControllerLeaderReplaced(cctx, killed, 10*time.Minute)
				Expect(cerr).NotTo(HaveOccurred(), "the controller must be leading again before the suite moves on")
				GinkgoWriter.Printf("controller leader after restart: %s/%s\n", d8ModuleNS, pod)
			})

			By("Waiting for the controller to come back and win leader election on another Pod")
			newPod, err := waitControllerLeaderReplaced(ctx, killed, 10*time.Minute)
			Expect(err).NotTo(HaveOccurred(), "the controller Deployment must bring the process back after the kill")
			GinkgoWriter.Printf("controller leader moved %s -> %s (+%s after the kill)\n", killed, newPod, time.Since(killedAt).Round(time.Second))

			By("Letting the capture finish ON ITS OWN — nothing below patches, annotates or re-creates any object of the tree")
			content, err := waitSnapshotReady(ctx, srcNS, ctrlRestartRootSnapshotName, 3*suiteCfg.captureReadyTO+10*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"the capture must resume and complete without any manual nudge after the controller restart\n%s\n%s",
				formatLedger("Snapshot "+srcNS+"/"+ctrlRestartRootSnapshotName, snapRec.ledger()),
				formatRequestLedger(srcNS, reqRec.ledger()))
			Expect(waitSnapshotContentReady(ctx, content, 2*suiteCfg.captureReadyTO+10*time.Minute)).To(Succeed())

			By("Waiting for every descendant node of the tree to reach Ready")
			var nodes []childRef
			Eventually(func(g Gomega) {
				var werr error
				nodes, werr = walkSnapshotTree(ctx, srcNS, ctrlRestartRootSnapshotName)
				g.Expect(werr).NotTo(HaveOccurred())
				kinds := map[string]int{}
				for _, n := range nodes {
					kinds[n.kind]++
				}
				g.Expect(kinds["DemoVirtualMachineSnapshot"]).To(Equal(1), "expected exactly one DemoVirtualMachineSnapshot child, got %v", kinds)
				g.Expect(kinds["DemoVirtualDiskSnapshot"]).To(Equal(1), "expected exactly one DemoVirtualDiskSnapshot grandchild, got %v", kinds)
			}).WithContext(ctx).WithTimeout(suiteCfg.captureReadyTO + 5*time.Minute).WithPolling(pollInterval).Should(Succeed())
			Expect(waitChildrenReady(ctx, srcNS, nodes, suiteCfg.captureReadyTO+5*time.Minute)).To(Succeed())

			By("Asserting the FIRST Ready=True was observed after the kill (the tree really finished on the resumed controller)")
			snapLedger := snapRec.ledger()
			GinkgoWriter.Printf("%s\n", formatLedger("Snapshot "+srcNS+"/"+ctrlRestartRootSnapshotName, snapLedger))
			firstTrue, sawTrue := firstReadyTrueAt(snapLedger)
			Expect(sawTrue).To(BeTrue(), "the Ready recorder never observed Ready=True although the wait above succeeded (recorder or watch problem, not a product one)")
			Expect(firstTrue.After(killedAt)).To(BeTrue(),
				"the root was already Ready=True %s before the controller was killed: this run asserted nothing about resuming a half-finished capture",
				killedAt.Sub(firstTrue).Round(time.Millisecond))

			By("Asserting each node's content edges equal its declared children exactly (no duplicated subtree)")
			// LIMIT: compares the nodes reachable from the root at assertion time. A child that was created
			// and then wrongly removed before the walk is outside what this can see; the node-count assertion
			// below is what keeps a tree that came back SMALLER from passing.
			checked := assertContentChildEdgesExact(ctx, srcNS, ctrlRestartRootSnapshotName, suiteCfg.captureReadyTO)
			GinkgoWriter.Printf("content child edges checked on %d snapshot node(s)\n", checked)
			Expect(checked).To(Equal(ctrlRestartNonLeafNodes),
				"the resumed capture must rebuild exactly the fixture's %d-node tree (root + VM child + disk grandchild); %d node(s) were compared",
				ctrlRestartNonLeafNodes, checked)

			By("Settling, then reading the final ledgers of Ready, the child set and the capture requests")
			// Give the resumed controller a window to expose a late pass before ANY ledger is read. All three
			// assertions below therefore see what happened inside this window too: a pass that drops Ready,
			// one that rewrites the child set, and one that re-issues a capture request. Reading a ledger
			// before the window would leave whatever it covers unasserted — a late re-issue that touched
			// neither Ready nor the child set would be recorded by its recorder and checked by nobody.
			sleepCtx(ctx, 30*time.Second)

			// LIMIT of the monotonicity assertion: it starts at the FIRST Ready=True. Everything before that
			// is ordinary convergence, and — since the kill lands before the tree is ready — what this proves
			// is that the RESUMED capture publishes Ready=True once and never takes it back. The flap of an
			// undisturbed capture is a different question, owned by the Ready-flap detector spec.
			finalLedger := snapRec.ledger()
			GinkgoWriter.Printf("%s\n", formatLedger("Snapshot "+srcNS+"/"+ctrlRestartRootSnapshotName+" (final)", finalLedger))
			assertReadyMonotonic("Snapshot "+srcNS+"/"+ctrlRestartRootSnapshotName, finalLedger)

			childRefsLedger := childRefsRec.ledger()
			GinkgoWriter.Printf("%s\n", formatLedger("SnapshotContent "+content+" childrenSnapshotContentRefs", childRefsLedger))
			assertChildrenRefsFrozen("SnapshotContent "+content, childRefsLedger)

			// No capture request was re-issued for a node that already had one.
			// LIMIT: the recorder only sees objects while its watch is connected, so a request created AND
			// reaped entirely inside a reconnect gap is invisible to it. It can therefore miss a re-issue,
			// never invent one — the assertion is conservative, and the counts printed here are what makes a
			// run where the recorder saw nothing visible rather than silently green.
			mcrSeen := reqRec.countByKind("ManifestCaptureRequest")
			vcrSeen := reqRec.countByKind("VolumeCaptureRequest")
			GinkgoWriter.Printf("%s\n", formatRequestLedger(srcNS, reqRec.ledger()))
			GinkgoWriter.Printf("distinct capture requests observed: %d ManifestCaptureRequest, %d VolumeCaptureRequest\n", mcrSeen, vcrSeen)
			Expect(mcrSeen).To(BeNumerically(">=", 1), "no ManifestCaptureRequest was observed at all: the re-issue assertion below would be vacuous")
			Expect(vcrSeen).To(BeNumerically(">=", 1), "no VolumeCaptureRequest was observed at all: the data leg never ran, so the re-issue assertion below would be vacuous")
			reIssued := reqRec.reIssued()
			Expect(reIssued).To(BeEmpty(),
				"a capture request was created a second time under the same name — the resumed capture redid work whose leg had already latched\n%s%s",
				formatReIssued(reIssued), formatRequestLedger(srcNS, reqRec.ledger()))
		})
	})
}
