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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/deckhouse/storage-e2e/pkg/cluster"
	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

// --- Suite env knobs (storage-e2e cluster knobs are read by storage-e2e itself) ---
const (
	envThinSC     = "E2E_THIN_STORAGE_CLASS"
	envProbeImage = "E2E_PROBE_IMAGE"
	envPVCSize    = "E2E_PVC_SIZE"
	envNSPrefix   = "E2E_NS_PREFIX"
)

const (
	defaultThinSC     = "e2e-thin-local"
	defaultProbeImage = "busybox:1.36"
	defaultPVCSize    = "1Gi"
	defaultNSPrefix   = "snap-e2e-"

	// defaultPoll is the polling cadence for all condition waiters.
	defaultPoll = 5 * time.Second
)

// CR condition literals shared across waiters. Kept as named constants so the
// strings the suite polls for stay in lock-step with the controller's emitted
// values and are not silently mistyped in one call site.
const (
	condStatusTrue = "True"

	condTypeReady     = "Ready"
	condTypeDataReady = "DataReady"
	condTypeAccepted  = "Accepted"
	condTypeRBACReady = "RBACReady"

	// reasonPublished is SnapshotExport's Ready reason once index, manifests and
	// all data endpoints are published (storage.deckhouse.io/v1alpha1
	// SnapshotExportReasonPublished).
	reasonPublished = "Published"
)

// CR GroupVersionResources we drive via the dynamic client. Keeping the e2e
// module free of the state-snapshotter API Go dependency means all CR access
// goes through unstructured + dynamic.
var (
	// snapshotGVR / snapshotExportGVR are namespaced (storage.deckhouse.io/v1alpha1).
	snapshotGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "snapshots",
	}
	snapshotExportGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "snapshotexports",
	}
	snapshotImportGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "snapshotimports",
	}
	// Domain snapshot GVRs (demo.state-snapshotter.deckhouse.io/v1alpha1) used to drive typed
	// subtree export (snapshotRef -> a DemoVirtualDiskSnapshot) and the server-side /view endpoint.
	demoDiskSnapshotGVR = schema.GroupVersionResource{
		Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualdisksnapshots",
	}
	// csdGVR is cluster-scoped (state-snapshotter.deckhouse.io/v1alpha1).
	csdGVR = schema.GroupVersionResource{
		Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "customsnapshotdefinitions",
	}
)

type e2eConfig struct {
	// thinSCName is the name of the Thin sds-local-volume StorageClass the
	// suite creates and binds the app PVC to.
	thinSCName string
	// probeImage is the image (with a shell) for the PVC-mounting pod.
	probeImage string
	// pvcSize is the requested size of the app PVC.
	pvcSize string
	// nsPrefix is prepended to the random test-namespace name.
	nsPrefix string

	// vmNamespace / baseStorageClass drive the optional VirtualDisk attach for
	// thin LVM (base cluster). Both come from TEST_CLUSTER_*; only used when
	// storage-e2e created the cluster (BaseKubeconfig != nil).
	vmNamespace      string
	baseStorageClass string
}

var (
	suiteCfg              e2eConfig
	suiteRestCfg          *rest.Config
	suiteClusterResources *cluster.TestClusterResources
	suiteClientset        *kubernetes.Clientset
	suiteDyn              dynamic.Interface
	suiteApply            *storagekube.ApplyClient
	suiteAPI              *apiTransport

	// suiteNamespace is the random test namespace, set by the spec once created.
	// Surfaced in the AfterSuite "left in place" banner.
	suiteNamespace string
)

func loadConfig() e2eConfig {
	cfg := e2eConfig{
		thinSCName:       strings.TrimSpace(os.Getenv(envThinSC)),
		probeImage:       strings.TrimSpace(os.Getenv(envProbeImage)),
		pvcSize:          strings.TrimSpace(os.Getenv(envPVCSize)),
		nsPrefix:         strings.TrimSpace(os.Getenv(envNSPrefix)),
		vmNamespace:      strings.TrimSpace(os.Getenv("TEST_CLUSTER_NAMESPACE")),
		baseStorageClass: strings.TrimSpace(os.Getenv("TEST_CLUSTER_STORAGE_CLASS")),
	}
	if cfg.thinSCName == "" {
		cfg.thinSCName = defaultThinSC
	}
	if cfg.probeImage == "" {
		cfg.probeImage = defaultProbeImage
	}
	if cfg.pvcSize == "" {
		cfg.pvcSize = defaultPVCSize
	}
	if cfg.nsPrefix == "" {
		cfg.nsPrefix = defaultNSPrefix
	}
	return cfg
}

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

// newRandomNamespace creates and returns a fresh, randomly-named test namespace.
func newRandomNamespace(ctx context.Context) (string, error) {
	name := suiteCfg.nsPrefix + strings.ToLower(cluster.GenerateRandomSuffix(8))
	if _, err := storagekube.CreateNamespaceIfNotExists(ctx, suiteRestCfg, name); err != nil {
		return "", fmt.Errorf("create namespace %s: %w", name, err)
	}
	return name, nil
}

// applyYAML applies (server-side, create-or-update) one or more YAML documents.
// Cluster-scoped objects ignore the namespace argument (resolved via RESTMapper
// inside the ApplyClient).
func applyYAML(ctx context.Context, manifest, namespace string) error {
	return suiteApply.ApplyYAML(ctx, manifest, namespace)
}

// getResource fetches an unstructured CR. ns == "" addresses cluster-scoped kinds.
func getResource(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if ns == "" {
		return suiteDyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	}
	return suiteDyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
}

// getCRCondition reads status.conditions[type==condType] from a CR. found is
// false when the object exists but the condition is not present yet.
func getCRCondition(ctx context.Context, gvr schema.GroupVersionResource, ns, name, condType string) (status, reason string, found bool, err error) {
	obj, err := getResource(ctx, gvr, ns, name)
	if err != nil {
		return "", "", false, err
	}
	conds, ok, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		return "", "", false, fmt.Errorf("read status.conditions of %s/%s: %w", ns, name, err)
	}
	if !ok {
		return "", "", false, nil
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == condType {
			st, _ := m["status"].(string)
			rs, _ := m["reason"].(string)
			return st, rs, true, nil
		}
	}
	return "", "", false, nil
}

// waitCRCondition blocks until the CR's condition reaches wantStatus or the
// timeout elapses. Transient GET/missing-condition states are retried; the last
// observed state is reported on timeout.
func waitCRCondition(ctx context.Context, gvr schema.GroupVersionResource, ns, name, condType, wantStatus string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		status, reason, found, err := getCRCondition(ctx, gvr, ns, name, condType)
		if err == nil && found && status == wantStatus {
			return nil
		}
		last = fmt.Sprintf("found=%v status=%q reason=%q err=%v", found, status, reason, err)
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s %s/%s condition %s=%s; last: %s",
				gvr.Resource, ns, name, condType, wantStatus, last)
		}
		if !sleepCtx(ctx, defaultPoll) {
			return ctx.Err()
		}
	}
}

// getExportSnapshots returns the flat per-node status.snapshots[] of a SnapshotExport. Each entry
// carries the node's own manifestsURL plus, for data nodes, volumeMode/dataURL/dataCA/ready. The CLI
// (and the SRK REST client this suite emulates) never parse the opaque index: they follow these URLs.
func getExportSnapshots(ctx context.Context, ns, name string) ([]map[string]interface{}, error) {
	return getStatusSnapshots(ctx, snapshotExportGVR, ns, name)
}

// getImportSnapshots returns the flat per-node status.snapshots[] of a SnapshotImport (per-node
// manifestsUploadURL + data uploadURL/uploadCA/uploadReady/volumeMode), published after server-side
// re-root.
func getImportSnapshots(ctx context.Context, ns, name string) ([]map[string]interface{}, error) {
	return getStatusSnapshots(ctx, snapshotImportGVR, ns, name)
}

func getStatusSnapshots(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) ([]map[string]interface{}, error) {
	obj, err := getResource(ctx, gvr, ns, name)
	if err != nil {
		return nil, err
	}
	raw, ok, err := unstructured.NestedSlice(obj.Object, "status", "snapshots")
	if err != nil {
		return nil, fmt.Errorf("read status.snapshots of %s/%s: %w", ns, name, err)
	}
	if !ok {
		return nil, nil
	}
	out := make([]map[string]interface{}, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]interface{}); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// getStatusString reads a single string field from a CR's status.
func getStatusString(ctx context.Context, gvr schema.GroupVersionResource, ns, name string, field string) (string, error) {
	obj, err := getResource(ctx, gvr, ns, name)
	if err != nil {
		return "", err
	}
	s, _, err := unstructured.NestedString(obj.Object, "status", field)
	return s, err
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
