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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

// Extra GVRs needed by the capture timeline but not declared in e2e_shared_test.go.
var (
	tlManifestCaptureRequestGVR = schema.GroupVersionResource{
		Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "manifestcapturerequests",
	}
	tlVolumeSnapshotContentGVR = schema.GroupVersionResource{
		Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotcontents",
	}
)

// tlTarget is one watched resource type. namespaced targets are scoped to the capture namespace; cluster
// targets are watched cluster-wide and filtered by creationTimestamp (see captureTimeline.record).
type tlTarget struct {
	gvr        schema.GroupVersionResource
	namespaced bool
}

// captureTimelineTargets is the full object chain involved in a snapshot capture, ordered roughly by the
// flow: root Snapshot -> SnapshotContent -> MCR -> ManifestCheckpoint; domain child snapshots/contents;
// and the volume-data leg (VolumeSnapshot/Content + DataExport/Import).
func captureTimelineTargets() []tlTarget {
	return []tlTarget{
		{snapshotGVR, true},
		{tlManifestCaptureRequestGVR, true},
		{demoVMSnapshotGVR, true},
		{demoDiskSnapshotGVR, true},
		{volumeSnapshotGVR, true},
		{dataExportGVR, true},
		{dataImportGVR, true},
		{snapshotContentGVR, false},
		{manifestCheckpointGVR, false},
		{tlVolumeSnapshotContentGVR, false},
	}
}

// objSummary is the comparable projection of an object's status used to detect transitions.
type objSummary struct {
	conds   map[string]string // condition type -> "Status/Reason"
	scalars map[string]string // well-known status scalar -> value
}

// tlObjState is the last recorded summary for a single object plus timing bookkeeping.
type tlObjState struct {
	conds    map[string]string
	scalars  map[string]string
	lastSeen time.Time // time of the last recorded transition (for dwell)
	created  time.Time // object creationTimestamp
}

// captureTimeline watches the capture object chain in the background and prints every state transition
// with a timestamp relative to start (+N.NNNs) and the dwell time spent in the previous state. It is a
// pure observability aid: it never asserts and never mutates cluster state.
//
// Output fan-out (every line goes to all of these):
//   - GinkgoWriter (streamed with `ginkgo -v`, always dumped on failure);
//   - a combined "_timeline.log" file;
//   - a per-kind "<kind>.log" file (e.g. snapshot.log, snapshotcontent.log, manifestcheckpoint.log),
//     so each object kind can be analyzed in isolation after the run.
//
// Files live under $E2E_TIMELINE_DIR/<ns>/ (default <tmp>/e2e/timeline/<ns>/). The directory is printed
// in the START/STOP markers.
type captureTimeline struct {
	t0     time.Time
	ns     string
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	states map[string]*tlObjState

	logMu    sync.Mutex
	dir      string
	combined *os.File
	files    map[string]*os.File // kind -> open file (nil entry = open failed, do not retry)
}

// startCaptureTimeline begins watching the capture chain in namespace ns. Call stop() (typically via
// defer) to end the watches, flush the per-kind files and print the closing marker.
func startCaptureTimeline(ns string) *captureTimeline {
	ctx, cancel := context.WithCancel(context.Background())
	base := os.Getenv("E2E_TIMELINE_DIR")
	if base == "" {
		base = filepath.Join(os.TempDir(), "e2e", "timeline")
	}
	tl := &captureTimeline{
		t0:     time.Now(),
		ns:     ns,
		cancel: cancel,
		states: map[string]*tlObjState{},
		dir:    filepath.Join(base, ns),
		files:  map[string]*os.File{},
	}
	if err := os.MkdirAll(tl.dir, 0o755); err != nil {
		tl.dir = "" // disable file sink; GinkgoWriter still works
	} else if f, err := os.Create(filepath.Join(tl.dir, "_timeline.log")); err == nil {
		tl.combined = f
	}
	tl.logf("START ns=%s dir=%s — watching capture state transitions (relative time, dwell per state)", ns, tl.dir)
	for _, t := range captureTimelineTargets() {
		tl.wg.Add(1)
		go tl.watchLoop(ctx, t)
	}
	return tl
}

// stop ends all watches, waits briefly for the goroutines to drain, then closes the log files.
func (tl *captureTimeline) stop() {
	if tl == nil {
		return
	}
	tl.cancel()
	done := make(chan struct{})
	go func() { tl.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	tl.logf("STOP — capture timeline ended after %s (logs in %s)", dshort(time.Since(tl.t0)), tl.dir)

	tl.logMu.Lock()
	for _, f := range tl.files {
		if f != nil {
			_ = f.Close()
		}
	}
	if tl.combined != nil {
		_ = tl.combined.Close()
		tl.combined = nil
	}
	tl.logMu.Unlock()
}

func (tl *captureTimeline) watchLoop(ctx context.Context, t tlTarget) {
	defer tl.wg.Done()
	var ri dynamic.ResourceInterface = suiteDyn.Resource(t.gvr)
	if t.namespaced {
		ri = suiteDyn.Resource(t.gvr).Namespace(tl.ns)
	}
	for ctx.Err() == nil {
		w, err := ri.Watch(ctx, metav1.ListOptions{})
		if err != nil {
			// Type may be unavailable/forbidden; back off and retry until the timeline is stopped.
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		tl.drain(ctx, t, w)
	}
}

func (tl *captureTimeline) drain(ctx context.Context, t tlTarget, w watch.Interface) {
	defer w.Stop()
	ch := w.ResultChan()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return // watch closed; watchLoop re-establishes it
			}
			u, ok := ev.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			tl.record(t, ev.Type, u)
		}
	}
}

func (tl *captureTimeline) record(t tlTarget, evType watch.EventType, u *unstructured.Unstructured) {
	created := u.GetCreationTimestamp().Time
	// Cluster-scoped types are watched cluster-wide: ignore objects that predate this capture window to
	// avoid logging churn from other namespaces/tests.
	if !t.namespaced && !created.IsZero() && created.Before(tl.t0.Add(-2*time.Second)) {
		return
	}
	kind := u.GetKind()
	if kind == "" {
		kind = t.gvr.Resource
	}
	name := u.GetName()
	key := t.gvr.Resource + "/" + name
	cur := summarizeObj(u)

	tl.mu.Lock()
	prev := tl.states[key]
	now := time.Now()

	if evType == watch.Deleted {
		if prev != nil {
			delete(tl.states, key)
			tl.mu.Unlock()
			tl.logfKind(kind, "%-24s %-44s DELETED (lived %s)", kind, name, dshort(now.Sub(prev.created)))
			return
		}
		tl.mu.Unlock()
		return
	}

	if prev == nil {
		tl.states[key] = &tlObjState{conds: cur.conds, scalars: cur.scalars, lastSeen: now, created: created}
		tl.mu.Unlock()
		age := ""
		if !created.IsZero() {
			age = " age=" + dshort(now.Sub(created))
		}
		tl.logfKind(kind, "%-24s %-44s ADDED%s  %s", kind, name, age, fmtSummary(cur))
		return
	}

	changes := diffSummary(prev, cur)
	if len(changes) == 0 {
		tl.mu.Unlock()
		return
	}
	dwell := now.Sub(prev.lastSeen)
	prev.conds = cur.conds
	prev.scalars = cur.scalars
	prev.lastSeen = now
	tl.mu.Unlock()

	sort.Strings(changes)
	tl.logfKind(kind, "%-24s %-44s %s  (dwell %s)", kind, name, strings.Join(changes, "; "), dshort(dwell))
}

// summarizeObj projects an object's status.conditions and a few well-known status scalars into a
// comparable summary.
func summarizeObj(u *unstructured.Unstructured) objSummary {
	s := objSummary{conds: map[string]string{}, scalars: map[string]string{}}
	if conds, ok, _ := unstructured.NestedSlice(u.Object, "status", "conditions"); ok {
		for _, c := range conds {
			m, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			typ, _, _ := unstructured.NestedString(m, "type")
			if typ == "" {
				continue
			}
			status, _, _ := unstructured.NestedString(m, "status")
			reason, _, _ := unstructured.NestedString(m, "reason")
			v := status
			if reason != "" {
				v += "/" + reason
			}
			s.conds[typ] = v
		}
	}
	for label, path := range map[string][]string{
		"phase":        {"status", "phase"},
		"boundContent": {"status", "boundSnapshotContentName"},
		"mcp":          {"status", "manifestCheckpointName"},
		"boundVSC":     {"status", "boundVolumeSnapshotContentName"},
	} {
		if v, ok, _ := unstructured.NestedString(u.Object, path...); ok && v != "" {
			s.scalars[label] = v
		}
	}
	if v, ok, _ := unstructured.NestedBool(u.Object, "status", "readyToUse"); ok {
		s.scalars["readyToUse"] = fmt.Sprintf("%v", v)
	}
	return s
}

// diffSummary returns human-readable "field: old -> new" entries for every condition/scalar that changed
// between the previous recorded state and the current summary.
func diffSummary(prev *tlObjState, cur objSummary) []string {
	var changes []string
	for typ, val := range cur.conds {
		if prev.conds[typ] != val {
			changes = append(changes, fmt.Sprintf("cond %s: %s -> %s", typ, orNone(prev.conds[typ]), val))
		}
	}
	for typ, val := range prev.conds {
		if _, ok := cur.conds[typ]; !ok {
			changes = append(changes, fmt.Sprintf("cond %s: %s -> <removed>", typ, val))
		}
	}
	for k, v := range cur.scalars {
		if prev.scalars[k] != v {
			changes = append(changes, fmt.Sprintf("%s: %s -> %s", k, orNone(prev.scalars[k]), v))
		}
	}
	for k, v := range prev.scalars {
		if _, ok := cur.scalars[k]; !ok {
			changes = append(changes, fmt.Sprintf("%s: %s -> <removed>", k, v))
		}
	}
	return changes
}

func fmtSummary(s objSummary) string {
	var parts []string
	for typ, v := range s.conds {
		parts = append(parts, "cond "+typ+"="+v)
	}
	for k, v := range s.scalars {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return "(no status yet)"
	}
	return strings.Join(parts, " ")
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

func dshort(d time.Duration) string {
	return d.Round(time.Millisecond).String()
}

// logf writes a general (kind-less) line: GinkgoWriter + combined file only.
func (tl *captureTimeline) logf(format string, args ...interface{}) {
	tl.emit("", fmt.Sprintf(format, args...))
}

// logfKind writes a per-object line and also fans it out to that kind's dedicated file.
func (tl *captureTimeline) logfKind(kind, format string, args ...interface{}) {
	tl.emit(kind, fmt.Sprintf(format, args...))
}

// emit fans a single line out to GinkgoWriter, the combined file and (when kind != "") the per-kind file.
func (tl *captureTimeline) emit(kind, line string) {
	full := fmt.Sprintf("[tl +%8.3fs] %s\n", time.Since(tl.t0).Seconds(), line)
	tl.logMu.Lock()
	defer tl.logMu.Unlock()
	fmt.Fprint(GinkgoWriter, full)
	if tl.combined != nil {
		fmt.Fprint(tl.combined, full)
	}
	if kind != "" {
		if f := tl.fileForLocked(kind); f != nil {
			fmt.Fprint(f, full)
		}
	}
}

// fileForLocked returns (lazily creating) the per-kind log file. Caller must hold logMu. A failed open is
// memoized as a nil entry so we do not retry on every line.
func (tl *captureTimeline) fileForLocked(kind string) *os.File {
	if f, ok := tl.files[kind]; ok {
		return f
	}
	if tl.dir == "" {
		return nil
	}
	f, err := os.Create(filepath.Join(tl.dir, sanitizeKind(kind)+".log"))
	if err != nil {
		tl.files[kind] = nil
		return nil
	}
	tl.files[kind] = f
	return f
}

// sanitizeKind lowercases a Kind and replaces any non-alphanumeric run with a single '-' for a safe,
// stable filename (e.g. "SnapshotContent" -> "snapshotcontent", "Demo/Disk" -> "demo-disk").
func sanitizeKind(kind string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(kind) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	return out
}
