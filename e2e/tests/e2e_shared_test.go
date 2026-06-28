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
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	clientgokube "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/storage-e2e/pkg/cluster"
	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

// --- Suite env knobs (storage-e2e cluster knobs are read by storage-e2e itself) ---
const (
	envNSPrefix             = "E2E_SNAPSHOTTER_NS_PREFIX"
	envSnapshotReadyTO      = "E2E_SNAPSHOT_READY_TIMEOUT"
	envCaptureReadyTO       = "E2E_CAPTURE_READY_TIMEOUT"
	envModuleReadyTO        = "E2E_MODULE_READY_TIMEOUT"
	envGCTTL                = "E2E_GC_TTL"
	envVolumeData           = "E2E_VOLUME_DATA"
	envStorageClass         = "E2E_STORAGE_CLASS"
	envProbeImage           = "E2E_PROBE_IMAGE"
	envBackupClientImage    = "E2E_BACKUP_CLIENT_IMAGE"
	envKeepClusterOnFailure = "E2E_KEEP_CLUSTER_ON_FAILURE"
	envKeepCluster          = "E2E_KEEP_CLUSTER"
)

const (
	defaultNSPrefix   = "snap-e2e"
	defaultSnapshotTO = 10 * time.Minute
	// defaultCaptureTO bounds snapshot *creation* (capture): manifests and LVM volume snapshots are both
	// fast to create (copy-on-write, no data movement), so a short deadline fails fast instead of dragging
	// on the generous snapshotReadyTO. snapshotReadyTO stays reserved for the restore/data-upload path,
	// where DataImport actually streams bytes back.
	defaultCaptureTO         = 30 * time.Second
	defaultModuleTO          = 15 * time.Minute
	defaultGCTTL             = "60s"
	defaultStorageClass      = "e2e-thin"
	defaultProbeImage        = "busybox:1.36"
	defaultBackupClientImage = "curlimages/curl:8.11.1"

	moduleName = "state-snapshotter"
	// The demo domain ships two flat CSDs (one snapshot kind per object): the structural VM snapshot
	// and the data-backed disk snapshot. Both must reach RBACReady before specs run.
	demoVMCSDName   = "demo-virtual-machine"
	demoDiskCSDName = "demo-virtual-disk"
	d8ModuleNS      = "d8-state-snapshotter"
	// d8DataManagerNS is the namespace of the DataExport/DataImport controller. The feature was absorbed
	// from the former storage-volume-data-manager module into storage-foundation (d8-storage-foundation).
	d8DataManagerNS = "d8-storage-foundation"
)

// phase5ImportNS is set by the phase-5 restore spec while it runs; diagnostics use it on failure.
var phase5ImportNS string

// Aggregated subresource API groups (C8/C9). The core group serves the generic and core-Snapshot
// subresources; the demo group is the domain controller's own aggregated apiserver; the VS connector
// group is the generic-PVC extended VolumeSnapshot read surface.
const (
	coreSubresGroup   = "subresources.state-snapshotter.deckhouse.io"
	coreSubresVersion = "v1alpha1"
	demoSubresGroup   = "subresources.demo.state-snapshotter.deckhouse.io"
	demoSubresVersion = "v1alpha1"
	vsConnectorGroup  = "subresources.snapshot.storage.k8s.io"
	vsConnectorVer    = "v1"

	subManifestsDownload = "manifests-download"
	subManifestsRestore  = "manifests-with-data-restoration"
	subManifestsUpload   = "manifests-and-children-refs-upload"
)

// Condition types. The Ready / ChildrenSnapshotReady contract constants come from api/storage; the leg
// conditions (ManifestsReady / VolumesReady / ChildrenReady) live in the controller image's pkg/snapshot
// and are mirrored here as the stable public contract to keep the e2e module dependency-light.
const (
	condReady                 = storagev1alpha1.ConditionReady
	condChildrenSnapshotReady = storagev1alpha1.ConditionChildrenSnapshotReady
	condManifestsReady        = "ManifestsReady"
	condVolumesReady          = "VolumesReady"
	condChildrenReady         = "ChildrenReady"
)

// Demo domain API group (the CRs and their snapshot kinds).
var demoGroupVersion = demov1alpha1.SchemeGroupVersion.String()

// GVRs used across the suite (all CRD access goes through the dynamic client).
var (
	snapshotGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "snapshots",
	}
	snapshotContentGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "snapshotcontents",
	}
	demoVMGVR = schema.GroupVersionResource{
		Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualmachines",
	}
	demoDiskGVR = schema.GroupVersionResource{
		Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualdisks",
	}
	demoVMSnapshotGVR = schema.GroupVersionResource{
		Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualmachinesnapshots",
	}
	demoDiskSnapshotGVR = schema.GroupVersionResource{
		Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualdisksnapshots",
	}
	csdGVR = schema.GroupVersionResource{
		Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "customsnapshotdefinitions",
	}
	manifestCheckpointGVR = schema.GroupVersionResource{
		Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "manifestcheckpoints",
	}
	moduleConfigGVR = schema.GroupVersionResource{
		Group: "deckhouse.io", Version: "v1alpha1", Resource: "moduleconfigs",
	}
	configMapGVR = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "configmaps",
	}
	objectKeeperGVR = schema.GroupVersionResource{
		Group: "deckhouse.io", Version: "v1alpha1", Resource: "objectkeepers",
	}
	// Phase-3 (volume-data) GVRs: the real CSI VolumeSnapshot (resolved by the VS connector) and the
	// storage-foundation VolumeRestoreRequest used to materialize restored PVCs from data artifacts.
	volumeSnapshotGVR = schema.GroupVersionResource{
		Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots",
	}
	volumeRestoreRequestGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "volumerestorerequests",
	}
	dataExportGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "dataexports",
	}
	dataImportGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "dataimports",
	}
)

// backupFixture holds phase-4 capture/download state shared with phase-5 restore/import.
// Populated incrementally by backupDownloadSpecs(); phase 5 skips when ready is false.
type backupFixture struct {
	ready       bool
	srcNS       string
	sc          string
	rootSnap    string
	rootContent string
	checksums   map[string]string // source PVC name -> sha256
	dataDir     string            // in-cluster path on the backup-client pod (emptyDir mount)

	vmSnapName    string
	diskASnapName string
	diskBSnapName string
	orphanVSName  string
	leafToPVC     map[string]string // snapshot leaf name -> source PVC name
}

var backup backupFixture

const pollInterval = 5 * time.Second

type e2eConfig struct {
	nsPrefix          string
	snapshotReadyTO   time.Duration
	captureReadyTO    time.Duration
	moduleReadyTO     time.Duration
	gcTTL             string
	volumeData        bool
	storageClass      string
	probeImage        string
	backupClientImage string
	keepOnFailure     bool
	keepAlways        bool

	// vmNamespace / baseStorageClass drive the phase-3 runtime VirtualDisk attach on the base cluster.
	vmNamespace      string
	baseStorageClass string
}

var (
	suiteCfg              e2eConfig
	suiteRestCfg          *rest.Config
	suiteClientset        *clientgokube.Clientset
	suiteDyn              dynamic.Interface
	suiteClusterResources *cluster.TestClusterResources
)

func loadConfig() e2eConfig {
	cfg := e2eConfig{
		nsPrefix:          strings.TrimSpace(os.Getenv(envNSPrefix)),
		gcTTL:             strings.TrimSpace(os.Getenv(envGCTTL)),
		storageClass:      strings.TrimSpace(os.Getenv(envStorageClass)),
		probeImage:        strings.TrimSpace(os.Getenv(envProbeImage)),
		backupClientImage: strings.TrimSpace(os.Getenv(envBackupClientImage)),
		volumeData:        envBool(os.Getenv(envVolumeData)),
		keepOnFailure:     envBool(os.Getenv(envKeepClusterOnFailure)),
		keepAlways:        envBool(os.Getenv(envKeepCluster)),
		vmNamespace:       strings.TrimSpace(os.Getenv("TEST_CLUSTER_NAMESPACE")),
		baseStorageClass:  strings.TrimSpace(os.Getenv("TEST_CLUSTER_STORAGE_CLASS")),
	}
	if cfg.nsPrefix == "" {
		cfg.nsPrefix = defaultNSPrefix
	}
	if cfg.gcTTL == "" {
		cfg.gcTTL = defaultGCTTL
	}
	if cfg.storageClass == "" {
		cfg.storageClass = defaultStorageClass
	}
	if cfg.probeImage == "" {
		cfg.probeImage = defaultProbeImage
	}
	if cfg.backupClientImage == "" {
		cfg.backupClientImage = defaultBackupClientImage
	}
	cfg.snapshotReadyTO = parseDuration(os.Getenv(envSnapshotReadyTO), defaultSnapshotTO)
	cfg.captureReadyTO = parseDuration(os.Getenv(envCaptureReadyTO), defaultCaptureTO)
	cfg.moduleReadyTO = parseDuration(os.Getenv(envModuleReadyTO), defaultModuleTO)
	return cfg
}

func envBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func parseDuration(raw string, def time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return def
}

// uniqueNS returns a DNS-1123 namespace name with the configured prefix and a short run suffix.
func uniqueNS(role string) string {
	return fmt.Sprintf("%s-%s-%d", suiteCfg.nsPrefix, role, time.Now().UnixNano()%100000)
}

// --- cluster lifecycle (mirror sds-elastic) --------------------------------

func ensureNestedTestCluster() {
	if strings.TrimSpace(os.Getenv("TEST_CLUSTER_CREATE_MODE")) == "" {
		Fail("TEST_CLUSTER_CREATE_MODE must be set: this suite only supports storage-e2e nested clusters")
	}
	if suiteClusterResources != nil {
		return
	}
	suiteClusterResources = cluster.CreateOrConnectToTestCluster()
	if suiteClusterResources == nil || suiteClusterResources.Kubeconfig == nil {
		Fail("storage-e2e returned a nil cluster handle")
	}
}

func cleanupNestedTestCluster() {
	if suiteClusterResources == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := cluster.CleanupTestCluster(ctx, suiteClusterResources); err != nil {
		GinkgoWriter.Printf("  warning: nested cluster cleanup failed: %v\n", err)
	} else {
		GinkgoWriter.Printf("  nested cluster cleanup finished\n")
	}
	suiteClusterResources = nil
}

// --- module / CSD readiness ------------------------------------------------

// waitModuleAndCSDReady blocks until the state-snapshotter module is Ready and the demo CSD has reached
// RBACReady=True (the 030-domain-rbac hook signal that domain RBAC is granted and the demo graph is live).
func waitModuleAndCSDReady(ctx context.Context) error {
	if err := storagekube.WaitForModuleReady(ctx, suiteRestCfg, moduleName, suiteCfg.moduleReadyTO); err != nil {
		return fmt.Errorf("module %s not Ready: %w", moduleName, err)
	}
	for _, csd := range []string{demoVMCSDName, demoDiskCSDName} {
		if err := waitObjectCondition(ctx, csdGVR, "", csd, "RBACReady", "True", suiteCfg.moduleReadyTO); err != nil {
			return fmt.Errorf("demo CSD %s not RBACReady: %w", csd, err)
		}
	}
	return nil
}

// --- namespaces ------------------------------------------------------------

func ensureNamespace(ctx context.Context, name string) error {
	_, err := storagekube.CreateNamespaceIfNotExists(ctx, suiteRestCfg, name)
	return err
}

// cleanupSkipped reports whether per-spec resource cleanup must be skipped to preserve resources for
// live inspection. Two knobs drive it: E2E_KEEP_CLUSTER keeps resources unconditionally (pass or fail),
// while E2E_KEEP_CLUSTER_ON_FAILURE keeps them only when the current spec failed. It is safe to call
// from any DeferCleanup/destructor (which Ginkgo runs regardless of pass/fail): with neither knob set,
// or with only the on-failure knob on a passing spec, cleanup proceeds as usual.
func cleanupSkipped() bool {
	return suiteCfg.keepAlways || (suiteCfg.keepOnFailure && CurrentSpecReport().Failed())
}

// keepReason names the env knob that caused cleanup to be skipped, for accurate log lines.
func keepReason() string {
	if suiteCfg.keepAlways {
		return envKeepCluster
	}
	return envKeepClusterOnFailure
}

func deleteNamespace(ctx context.Context, name string) {
	if cleanupSkipped() {
		GinkgoWriter.Printf("%s: keeping namespace %q\n", keepReason(), name)
		return
	}
	cs := suiteClientset
	if cs == nil {
		return
	}
	_ = cs.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
}

// --- aggregated --raw helpers ----------------------------------------------

// aggGet performs an aggregated-apiserver GET against an absolute API path, returning the raw body.
func aggGet(ctx context.Context, path string, params map[string]string) ([]byte, error) {
	req := suiteClientset.Discovery().RESTClient().Get().AbsPath(path)
	for k, v := range params {
		req = req.Param(k, v)
	}
	return req.DoRaw(ctx)
}

// aggPost performs an aggregated-apiserver POST (JSON body) against an absolute API path.
func aggPost(ctx context.Context, path string, body []byte) ([]byte, error) {
	resp, err := suiteClientset.Discovery().RESTClient().Post().
		AbsPath(path).
		SetHeader("Content-Type", "application/json").
		Body(body).
		DoRaw(ctx)
	if err != nil {
		// DoRaw collapses any non-2xx into a generic error (e.g. POST+409 -> "the server reported a
		// conflict" with reason AlreadyExists) and does not decode the body, which hides the aggregated
		// apiserver's real Status message. Append the raw response body so failures are actionable.
		return resp, fmt.Errorf("%w (response body: %s)", err, truncate(resp, 1024))
	}
	return resp, nil
}

func coreSnapshotSubPath(ns, name, sub string) string {
	return fmt.Sprintf("/apis/%s/%s/namespaces/%s/snapshots/%s/%s", coreSubresGroup, coreSubresVersion, ns, name, sub)
}

func coreGenericSubPath(ns, resource, name, sub string) string {
	return fmt.Sprintf("/apis/%s/%s/namespaces/%s/%s/%s/%s", coreSubresGroup, coreSubresVersion, ns, resource, name, sub)
}

func coreContentDownloadPath(name string) string {
	return fmt.Sprintf("/apis/%s/%s/snapshotcontents/%s/%s", coreSubresGroup, coreSubresVersion, name, subManifestsDownload)
}

func demoSubPath(ns, resource, name, sub string) string {
	return fmt.Sprintf("/apis/%s/%s/namespaces/%s/%s/%s/%s", demoSubresGroup, demoSubresVersion, ns, resource, name, sub)
}

// decodeManifestArray parses a JSON array of Kubernetes objects (the manifests-download /
// manifests-with-data-restoration payload) into unstructured objects.
func decodeManifestArray(data []byte) ([]unstructured.Unstructured, error) {
	var raw []map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode manifest array: %w (body: %s)", err, truncate(data, 512))
	}
	out := make([]unstructured.Unstructured, 0, len(raw))
	for _, m := range raw {
		out = append(out, unstructured.Unstructured{Object: m})
	}
	return out, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// --- condition waiters -----------------------------------------------------

// conditionStatus returns the status of a status.conditions[type==condType] entry.
func conditionStatus(obj *unstructured.Unstructured, condType string) (status, reason string, found bool) {
	conds, ok, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !ok {
		return "", "", false
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(m, "type"); t != condType {
			continue
		}
		status, _, _ = unstructured.NestedString(m, "status")
		reason, _, _ = unstructured.NestedString(m, "reason")
		return status, reason, true
	}
	return "", "", false
}

// snapshotNodeReady reports whether a snapshot tree node is ready. Demo/core snapshot kinds expose a
// Ready=True status condition; a CSI VolumeSnapshot visibility leaf (the namespace-root orphan-PVC data
// leg) has no Ready condition and signals readiness via status.readyToUse instead. detail is a
// human-readable reason when not ready.
func snapshotNodeReady(obj *unstructured.Unstructured, kind string) (ready bool, detail string) {
	if kind == "VolumeSnapshot" {
		rtu, found, _ := unstructured.NestedBool(obj.Object, "status", "readyToUse")
		if found && rtu {
			return true, ""
		}
		return false, fmt.Sprintf("readyToUse found=%v value=%v", found, rtu)
	}
	st, reason, found := conditionStatus(obj, condReady)
	if found && st == "True" {
		return true, ""
	}
	return false, fmt.Sprintf("Ready found=%v status=%q reason=%q", found, st, reason)
}

// getResource fetches a (possibly namespaced) dynamic resource. ns="" addresses cluster-scoped kinds.
func getResource(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if ns == "" {
		return suiteDyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	}
	return suiteDyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
}

// waitObjectCondition blocks until the object's status.conditions[type==condType].status == wantStatus.
func waitObjectCondition(ctx context.Context, gvr schema.GroupVersionResource, ns, name, condType, wantStatus string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		obj, err := getResource(ctx, gvr, ns, name)
		if err == nil {
			if st, reason, found := conditionStatus(obj, condType); found && st == wantStatus {
				return nil
			} else {
				last = fmt.Sprintf("found=%v status=%q reason=%q", found, st, reason)
			}
		} else {
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s %s/%s condition %s=%s; last: %s", gvr.Resource, ns, name, condType, wantStatus, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// waitDemoDiskReady waits for a restored DemoVirtualDisk to reach Ready=True, failing fast if the disk
// enters its terminal Failed phase (e.g. RestoreDenied) instead of burning the whole timeout. A Failed
// phase on the demo restore path is permanent (the controller will not retry), so polling further is
// pointless and only delays surfacing the real error.
func waitDemoDiskReady(ctx context.Context, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		obj, err := getResource(ctx, demoDiskGVR, ns, name)
		if err == nil {
			st, reason, found := conditionStatus(obj, condReady)
			if found && st == "True" {
				return nil
			}
			phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
			if phase == "Failed" {
				return fmt.Errorf("DemoVirtualDisk %s/%s entered terminal Failed phase (Ready.status=%q reason=%q)", ns, name, st, reason)
			}
			last = fmt.Sprintf("phase=%q Ready.status=%q reason=%q", phase, st, reason)
		} else {
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for DemoVirtualDisk %s/%s Ready=True; last: %s", ns, name, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// waitSnapshotReady waits for a namespaced Snapshot to reach Ready=True and returns its bound content name.
func waitSnapshotReady(ctx context.Context, ns, name string, timeout time.Duration) (string, error) {
	if err := waitObjectCondition(ctx, snapshotGVR, ns, name, condReady, "True", timeout); err != nil {
		return "", err
	}
	snap, err := getResource(ctx, snapshotGVR, ns, name)
	if err != nil {
		return "", err
	}
	content, _, _ := unstructured.NestedString(snap.Object, "status", "boundSnapshotContentName")
	if content == "" {
		return "", fmt.Errorf("Snapshot %s/%s is Ready but has empty status.boundSnapshotContentName", ns, name)
	}
	return content, nil
}

// waitSnapshotContentReady waits for a cluster-scoped SnapshotContent to have all four leg conditions
// True. The whole set shares a SINGLE timeout budget (one GET per poll checks every leg) rather than
// granting each leg its own full timeout, so the caller's context can be sized to one `timeout`.
func waitSnapshotContentReady(ctx context.Context, name string, timeout time.Duration) error {
	required := []string{condManifestsReady, condVolumesReady, condChildrenReady, condReady}
	deadline := time.Now().Add(timeout)
	var last string
	for {
		obj, err := getResource(ctx, snapshotContentGVR, "", name)
		if err == nil {
			pending := ""
			for _, ct := range required {
				if st, reason, found := conditionStatus(obj, ct); !found || st != "True" {
					pending = fmt.Sprintf("%s (found=%v status=%q reason=%q)", ct, found, st, reason)
					break
				}
			}
			if pending == "" {
				return nil
			}
			last = "pending " + pending
		} else {
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for SnapshotContent %s leg conditions; last: %s", name, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// waitChildrenReady waits for every child snapshot node to reach Ready=True under a SINGLE shared
// timeout budget (each poll checks all nodes), so a caller can size its context to one `timeout`
// regardless of how many children the tree has.
func waitChildrenReady(ctx context.Context, ns string, nodes []childRef, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		pending := ""
		for _, n := range nodes {
			gvr, ok := gvrForSnapshotKind(n.kind)
			if !ok {
				return fmt.Errorf("unknown child snapshot kind %q (%s)", n.kind, n.name)
			}
			obj, err := getResource(ctx, gvr, ns, n.name)
			if err != nil {
				pending = fmt.Sprintf("%s/%s get err=%v", n.kind, n.name, err)
				break
			}
			if ready, detail := snapshotNodeReady(obj, n.kind); !ready {
				pending = fmt.Sprintf("%s/%s %s", n.kind, n.name, detail)
				break
			}
		}
		if pending == "" {
			return nil
		}
		last = pending
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for child snapshots Ready; last: %s", last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// --- manifest apply --------------------------------------------------------

// applyObjects applies unstructured objects via storage-e2e's ApplyClient (server-side discovery +
// dynamic apply). defaultNamespace is used for namespaced objects that carry no namespace.
func applyObjects(ctx context.Context, objs []*unstructured.Unstructured, defaultNamespace string) error {
	applier, err := storagekube.NewApplyClient(suiteRestCfg)
	if err != nil {
		return fmt.Errorf("build apply client: %w", err)
	}
	for _, o := range objs {
		y, err := yaml.Marshal(o.Object)
		if err != nil {
			return fmt.Errorf("marshal %s/%s: %w", o.GetKind(), o.GetName(), err)
		}
		if err := applier.ApplyYAML(ctx, string(y), defaultNamespace); err != nil {
			return fmt.Errorf("apply %s/%s: %w", o.GetKind(), o.GetName(), err)
		}
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// --- snapshot tree walking -------------------------------------------------

// childRef is a resolved status.childrenSnapshotRefs[] entry (namespace is implicit: the whole run tree
// lives in the root Snapshot's namespace).
type childRef struct {
	apiVersion string
	kind       string
	name       string
}

// childSnapshotRefs extracts an object's status.childrenSnapshotRefs[] entries.
func childSnapshotRefs(obj *unstructured.Unstructured) []childRef {
	raw, ok, _ := unstructured.NestedSlice(obj.Object, "status", "childrenSnapshotRefs")
	if !ok {
		return nil
	}
	out := make([]childRef, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		av, _, _ := unstructured.NestedString(m, "apiVersion")
		k, _, _ := unstructured.NestedString(m, "kind")
		n, _, _ := unstructured.NestedString(m, "name")
		if n == "" || k == "" {
			continue
		}
		out = append(out, childRef{apiVersion: av, kind: k, name: n})
	}
	return out
}

// gvrForSnapshotKind maps a snapshot child ref kind to its GVR. The run tree contains the core Snapshot
// kind, the demo snapshot kinds, and the CSI VolumeSnapshot visibility leaves created by the
// namespace-root orphan-PVC data leg, so an explicit map is clearer (and safer) than guessing a plural
// from the kind.
func gvrForSnapshotKind(kind string) (schema.GroupVersionResource, bool) {
	switch kind {
	case "Snapshot":
		return snapshotGVR, true
	case "DemoVirtualMachineSnapshot":
		return demoVMSnapshotGVR, true
	case "DemoVirtualDiskSnapshot":
		return demoDiskSnapshotGVR, true
	case "VolumeSnapshot":
		return volumeSnapshotGVR, true
	default:
		return schema.GroupVersionResource{}, false
	}
}

// walkSnapshotTree performs a BFS from the root Snapshot over status.childrenSnapshotRefs, returning
// every descendant snapshot node (excluding the root). All nodes share the root's namespace.
func walkSnapshotTree(ctx context.Context, ns, rootSnapshot string) ([]childRef, error) {
	root, err := getResource(ctx, snapshotGVR, ns, rootSnapshot)
	if err != nil {
		return nil, fmt.Errorf("get root Snapshot %s/%s: %w", ns, rootSnapshot, err)
	}
	queue := childSnapshotRefs(root)
	seen := map[string]bool{}
	var out []childRef
	for len(queue) > 0 {
		ref := queue[0]
		queue = queue[1:]
		key := ref.kind + "/" + ref.name
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ref)

		gvr, ok := gvrForSnapshotKind(ref.kind)
		if !ok {
			return nil, fmt.Errorf("unknown child snapshot kind %q (%s)", ref.kind, ref.name)
		}
		node, err := getResource(ctx, gvr, ns, ref.name)
		if err != nil {
			return nil, fmt.Errorf("get child %s %s/%s: %w", ref.kind, ns, ref.name, err)
		}
		queue = append(queue, childSnapshotRefs(node)...)
	}
	return out, nil
}

// errIsNotFound reports whether err is a Kubernetes NotFound (used by GC assertions).
func errIsNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}

// startAppearWatch opens a watch for a namespaced resource and returns a blocking wait function plus a
// stop function. It MUST be opened BEFORE the action that creates the resource, so an object whose entire
// lifetime is shorter than any poll interval is still observed: a transient resource (e.g. the capture
// RoleBinding, which now lives only for the ~1s capture window between Snapshot creation and
// ManifestsArchived=True) is reliably missed by an interval poll, but a watch opened first cannot lose
// the ADDED event because client-go applies backpressure on its result channel rather than dropping it.
//
// The returned wait function first tries a direct Get (covers an already-present object), then consumes
// watch events until an ADDED/MODIFIED for the named object arrives or the timeout elapses. The caller
// must always invoke stop (e.g. via defer) to release the watch.
func startAppearWatch(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (wait func(time.Duration) (*unstructured.Unstructured, error), stop func(), err error) {
	w, err := suiteDyn.Resource(gvr).Namespace(ns).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("watch %s in namespace %s: %w", gvr.Resource, ns, err)
	}
	wait = func(timeout time.Duration) (*unstructured.Unstructured, error) {
		if obj, getErr := getResource(ctx, gvr, ns, name); getErr == nil {
			return obj, nil
		} else if !apierrors.IsNotFound(getErr) {
			return nil, fmt.Errorf("get %s %s/%s: %w", gvr.Resource, ns, name, getErr)
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		for {
			select {
			case ev, ok := <-w.ResultChan():
				if !ok {
					// Watch ended unexpectedly; a final Get covers an event delivered as it closed.
					if obj, getErr := getResource(ctx, gvr, ns, name); getErr == nil {
						return obj, nil
					}
					return nil, fmt.Errorf("watch for %s %s/%s closed before it appeared", gvr.Resource, ns, name)
				}
				if ev.Type == watch.Error {
					// Treat a watch error like a close: try a Get, otherwise surface it instead of looping.
					if obj, getErr := getResource(ctx, gvr, ns, name); getErr == nil {
						return obj, nil
					}
					return nil, fmt.Errorf("watch error for %s %s/%s: %v", gvr.Resource, ns, name, ev.Object)
				}
				if ev.Type != watch.Added && ev.Type != watch.Modified {
					continue
				}
				obj, ok := ev.Object.(*unstructured.Unstructured)
				if !ok || obj.GetName() != name {
					continue
				}
				return obj, nil
			case <-timer.C:
				return nil, fmt.Errorf("timeout after %s waiting for %s %s/%s to appear", timeout, gvr.Resource, ns, name)
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return wait, w.Stop, nil
}

// assertResourceGone blocks until the (possibly cluster-scoped) resource is NotFound, failing the spec
// if it is still present at the deadline.
func assertResourceGone(ctx context.Context, gvr schema.GroupVersionResource, ns, name string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func() error {
		_, err := getResource(ctx, gvr, ns, name)
		if err == nil {
			return fmt.Errorf("%s %s still exists", gvr.Resource, name)
		}
		if errIsNotFound(err) {
			return nil
		}
		return err
	}).WithContext(ctx).WithTimeout(timeout).WithPolling(5*time.Second).Should(Succeed(), "%s %s should be GC'd", gvr.Resource, name)
}
