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

// This file emulates how an SRK (backup-system) client integrates with the status-driven export/
// import protocol over plain REST, without the d8 binary:
//
//   - the index/manifests "control plane" lives on the kube-apiserver aggregated subresources and is
//     reached with a kubeconfig-authenticated HTTP client over AbsPaths copied verbatim from CR
//     status (apiTransport). The index is treated as an opaque blob: the suite never parses it.
//   - the per-node volume "data plane" lives on DataExport/DataImport pods reachable only from inside
//     the cluster. The suite runs a small busybox pod (authorized by its ServiceAccount token via the
//     data-exporter's SubjectAccessReview on create dataexports|dataimports/download) and execs
//     busybox wget against the per-node dataURL, sending the SA bearer token. busybox ships no
//     --ca-certificate, so TLS verification is skipped (--no-check-certificate): the suite validates
//     the data protocol and byte integrity, not endpoint trust.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

// subresAPIPrefix is the namespaced aggregated-subresource AbsPath prefix. For the server-side /view
// (consumed by d8 snapshot list) there is no SnapshotExport CR to publish the URL, so the client
// builds it itself: <prefix>/<ns>/<resource>/<name>/view.
const subresAPIPrefix = "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces"

// snapshotView mirrors the server's stable SnapshotView projection (usecase/restore/view.go). Unlike
// the opaque index, the view IS meant to be parsed by clients to render the tree.
type snapshotView struct {
	Version string           `json:"version"`
	Root    snapshotViewNode `json:"root"`
}

type snapshotViewNode struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Namespace  string             `json:"namespace"`
	Name       string             `json:"name"`
	HasData    bool               `json:"hasData"`
	VolumeMode string             `json:"volumeMode,omitempty"`
	SizeBytes  int64              `json:"sizeBytes,omitempty"`
	Children   []snapshotViewNode `json:"children,omitempty"`
}

func viewAbsPath(ns, resource, name string) string {
	return fmt.Sprintf("%s/%s/%s/%s/view", subresAPIPrefix, ns, resource, name)
}

// getSnapshotView GETs and parses the server-side view for a snapshot identified by its plural
// resource (snapshots for the namespace root, or a domain plural for a subtree).
func getSnapshotView(ctx context.Context, resource, ns, name string) (*snapshotView, error) {
	raw, code, err := suiteAPI.get(ctx, viewAbsPath(ns, resource, name))
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("GET view %s/%s/%s: status %d (%s)", ns, resource, name, code, truncate(raw))
	}
	v := &snapshotView{}
	if err := json.Unmarshal(raw, v); err != nil {
		return nil, fmt.Errorf("parse view %s/%s/%s: %w", ns, resource, name, err)
	}
	return v, nil
}

// countViewNodes returns the total number of nodes in a view tree.
func countViewNodes(n *snapshotViewNode) int {
	total := 1
	for i := range n.Children {
		total += countViewNodes(&n.Children[i])
	}
	return total
}

func truncate(b []byte) string {
	const max = 256
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}

const (
	// dataPodContainer is the single container of the in-cluster data pod.
	dataPodContainer = "data"
	// dataPodSAName / dataPodName are the downloader identity and pod created per test ns.
	dataPodSAName = "snap-e2e-data"
	dataPodName   = "snap-e2e-data"
	// saTokenPath is where the kubelet projects the pod's ServiceAccount token; the data-exporter
	// TokenReviews + SubjectAccessReviews this bearer token.
	saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	// bundleDir is the data pod's scratch space for downloaded volume images.
	bundleDir = "/tmp/bundle"

	// envDataImage overrides the data pod image; it defaults to the suite probe image (busybox),
	// reusing the only image dependency the suite already has.
	envDataImage = "E2E_DATA_IMAGE"

	volumeModeBlock      = "Block"
	volumeModeFilesystem = "Filesystem"
)

// apiTransport is a kubeconfig-authenticated HTTP client for kube-apiserver AbsPaths. It mirrors the
// thin-client contract: GET an opaque index/manifests blob, PUT it back on import. Auth + cluster TLS
// come from the rest.Config, so no per-call CA handling is needed (unlike the data plane).
type apiTransport struct {
	hc   *http.Client
	base string
}

func newAPITransport(cfg *rest.Config) (*apiTransport, error) {
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("build apiserver HTTP client: %w", err)
	}
	return &apiTransport{hc: hc, base: strings.TrimSuffix(cfg.Host, "/")}, nil
}

func (a *apiTransport) do(ctx context.Context, method, absPath string, body []byte, headers map[string]string) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+absPath, rdr)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return respBody, resp.StatusCode, nil
}

// get returns the response body and status code for a GET on an apiserver AbsPath. Compression is
// disabled so an opaque blob is returned verbatim.
func (a *apiTransport) get(ctx context.Context, absPath string) ([]byte, int, error) {
	return a.do(ctx, http.MethodGet, absPath, nil, map[string]string{
		"Accept":          "application/json",
		"Accept-Encoding": "identity",
	})
}

// putBlob uploads body in a single chunk at offset 0 to an upload AbsPath; with finalize it appends
// ?finalize=true to commit. e2e index/manifests blobs are well under the server's 512 KiB chunk cap,
// so a single PUT suffices (the resumable multi-chunk path is exercised by d8 unit tests).
func (a *apiTransport) putBlob(ctx context.Context, absPath string, body []byte, finalize bool) error {
	path := absPath
	if finalize {
		path = withParam(path, "finalize", "true")
	}
	_, code, err := a.do(ctx, http.MethodPut, path, body, map[string]string{
		"Content-Type": "application/octet-stream",
		"X-Offset":     "0",
	})
	if err != nil {
		return err
	}
	if code != http.StatusOK && code != http.StatusAccepted {
		return fmt.Errorf("PUT %s: unexpected status %d", absPath, code)
	}
	return nil
}

// commit sends an empty finalize PUT (used to flip the top-level manifests/index commit gates without
// re-sending bytes).
func (a *apiTransport) commit(ctx context.Context, absPath string) error {
	return a.putBlob(ctx, absPath, nil, true)
}

func withParam(path, key, value string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + key + "=" + value
}

// --- in-cluster data plane (busybox wget pod) ------------------------------

func dataImage() string {
	if v := strings.TrimSpace(os.Getenv(envDataImage)); v != "" {
		return v
	}
	return suiteCfg.probeImage
}

// ensureDataPod provisions the downloader identity (SA + namespaced Role/RoleBinding granting create
// on dataexports/download and dataimports/download) and a long-lived restricted busybox pod, then
// waits for it to be Ready.
func ensureDataPod(ctx context.Context, ns string, timeout time.Duration) error {
	if err := applyYAML(ctx, dataRBACManifest(ns, dataPodSAName), ns); err != nil {
		return fmt.Errorf("apply downloader RBAC: %w", err)
	}
	if err := applyYAML(ctx, dataPodManifest(ns, dataPodName, dataPodSAName, dataImage()), ns); err != nil {
		return fmt.Errorf("apply data pod: %w", err)
	}
	return waitPodReady(ctx, ns, dataPodName, timeout)
}

// dataExec runs a /bin/sh script inside the data pod and returns stdout. stderr is folded into the
// error so failing wget invocations surface their diagnostics.
func dataExec(ctx context.Context, ns, script string) (string, error) {
	stdout, stderr, err := storagekube.ExecInPod(ctx, suiteRestCfg, ns, dataPodName, dataPodContainer, []string{"sh", "-c", script})
	if err != nil {
		return stdout, fmt.Errorf("exec in data pod: %w (stderr=%s)", err, stderr)
	}
	return stdout, nil
}

// wgetPrelude exports TOKEN from the projected SA token and defines a `dl` helper: busybox wget with
// the bearer header and TLS verification disabled (busybox ships no --ca-certificate).
func wgetPrelude() string {
	return strings.Join([]string{
		"set -e",
		fmt.Sprintf("TOKEN=$(cat %s)", saTokenPath),
		`dl() { wget -q --no-check-certificate --header "Authorization: Bearer $TOKEN" "$@"; }`,
		`dls() { wget -S --no-check-certificate --header "Authorization: Bearer $TOKEN" "$@"; }`,
	}, "\n")
}

// dataReachable performs an authorized 1-byte ranged GET against apiPath on a data endpoint and
// returns an error unless the endpoint answers successfully. Used to assert a node's data endpoint is
// reachable and the pod's ServiceAccount is authorized, independent of volume mode and size.
func dataReachable(ctx context.Context, ns, dataURL, apiPath string) error {
	script := wgetPrelude() + "\n" +
		fmt.Sprintf(`dl -O /dev/null --header "Range: bytes=0-0" %q`, joinURL(dataURL, apiPath))
	_, err := dataExec(ctx, ns, script)
	return err
}

// dataBlockSize reads the total volume size from a ranged GET's Content-Range header
// (bytes 0-0/<total>), avoiding a HEAD the exporter may not implement and a full-size download.
func dataBlockSize(ctx context.Context, ns, dataURL string) (int64, error) {
	script := wgetPrelude() + "\n" +
		fmt.Sprintf(`dls -O /dev/null --header "Range: bytes=0-0" %q 2>&1 | tr -d '\r' | awk -F'/' '/[Cc]ontent-[Rr]ange/{print $2}'`,
			joinURL(dataURL, "api/v1/block"))
	out, err := dataExec(ctx, ns, script)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(out)
	if s == "" {
		return 0, fmt.Errorf("block endpoint returned no Content-Range total (out=%q)", out)
	}
	return strconv.ParseInt(s, 10, 64)
}

// dataBlockHeadHex returns the first n bytes of the block image as a lowercase hex string (text-safe;
// raw bytes never cross the exec boundary). Used to assert a known dd-written signature round-trips.
func dataBlockHeadHex(ctx context.Context, ns, dataURL string, n int) (string, error) {
	script := wgetPrelude() + "\n" +
		fmt.Sprintf(`dl -O - --header "Range: bytes=0-%d" %q | od -An -v -tx1 | tr -d ' \n'`,
			n-1, joinURL(dataURL, "api/v1/block"))
	out, err := dataExec(ctx, ns, script)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func joinURL(base, suffix string) string {
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(suffix, "/")
}

// waitPodReady polls a single pod until it reports Ready (PodReady condition True).
func waitPodReady(ctx context.Context, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		pod, err := suiteClientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil && podIsReady(pod) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for pod %s/%s Ready (err=%v)", ns, name, err)
		}
		if !sleepCtx(ctx, defaultPoll) {
			return ctx.Err()
		}
	}
}

func podIsReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func dataRBACManifest(ns, sa string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: %[1]s
  namespace: %[2]s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: %[1]s
  namespace: %[2]s
rules:
  - apiGroups: ["storage.deckhouse.io"]
    resources: ["dataexports/download", "dataimports/download"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: %[1]s
  namespace: %[2]s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: %[1]s
subjects:
  - kind: ServiceAccount
    name: %[1]s
    namespace: %[2]s
`, sa, ns)
}

func dataPodManifest(ns, name, sa, image string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  serviceAccountName: %[3]s
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: %[4]s
      image: %[5]s
      command: ["sleep", "7200"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["ALL"]
`, name, ns, sa, dataPodContainer, image)
}
