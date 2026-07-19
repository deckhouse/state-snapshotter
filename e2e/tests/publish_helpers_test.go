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
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	clientgokube "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

// --- Publish (ingress + tokens) helpers (E2E_PUBLISH) ----------------------------------------------
//
// These helpers back the DataExport/DataImport/manifests publish specs. They cover four concerns the
// publish scenarios share:
//
//  1. auth   — SA + Role + RoleBinding (download / manifests-download) in the NESTED cluster and a Bearer
//     token minted via the TokenRequest API (the programmatic form of `kubectl create token`). The token
//     is the only credential the external path carries.
//  2. masterIP — the nested master IP for the external `--resolve` fallback. Primary: the IP embedded in
//     the sslip.io publish domain (already parsed into suitePublishInfra.masterIP by the BeforeSuite
//     sanity-check). Fallback: discovery from the base-cluster VirtualMachine CRs.
//  3. curl-runner — a curlimages/curl pod on the PARENT (base) cluster, in the nested VMs' namespace
//     (TEST_CLUSTER_NAMESPACE) so it shares the DVP network with the nested nodes. The nested Bearer token
//     is passed per-exec as an argv value (`$1`), so no nested credential is ever persisted in the pod.
//  4. CA — status.ca parsing (Issuer assertion) and materialization into a file inside a pod for `curl
//     --cacert` (instead of `-k`).
//
// The external path runs on the base cluster because the test runner reaches the nested cluster only via
// an SSH tunnel to its API (6445); the nested workers' 443 is not forwarded, whereas a base-cluster pod is
// on the same DVP network as the nested VMs and can hit api.<domain>:443 directly.

const (
	// publishClientSA is the default ServiceAccount name the publish specs mint tokens for (nested cluster).
	publishClientSA = "publish-client"

	// publishExtCurlPod / publishExtCurlCont name the external curl-runner pod on the BASE cluster.
	publishExtCurlPod  = "publish-ext-curl"
	publishExtCurlCont = "curl"

	// publishTokenTTL is the default lifetime of a minted Bearer token — comfortably longer than a single
	// publish spec while still short-lived.
	publishTokenTTL = 30 * time.Minute

	// publishExtBodyFile is the in-pod scratch path curl writes a response body to (so the exec can return
	// both the HTTP status code and the body). It lives on the pod's writable rootfs/tmpfs, never on the
	// test-runner disk.
	publishExtBodyFile = "/tmp/publish-body"

	// publishCAPodFile is the in-pod path writeCAToPodFile materializes status.ca to for `curl --cacert`.
	publishCAPodFile = "/tmp/publish-ca.pem"

	// sha256Empty is the SHA-256 of a zero-byte stream. The streaming-checksum helper uses it as a
	// transport-failure sentinel: `curl -f | sha256sum` on a failed request hashes an empty body, so a
	// result equal to this constant means the direct request did not deliver bytes and the `--resolve`
	// fallback should be tried. Safe here because the published payloads under test are never empty.
	sha256Empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// Data-plane endpoints served under a DataExport/DataImport base URL (status.url or status.publicURL).
// Mirrors the suffixes the existing backup_download / backup_restore helpers build off status.url.
const (
	publishFilesPath    = "/api/v1/files/" // Filesystem mode: directory listing + per-file download
	publishBlockPath    = "/api/v1/block"  // Block mode: raw device stream (GET download / PUT upload)
	publishFinishedPath = "/api/v1/finished"
)

// virtualMachineGVR is the DVP VirtualMachine resource on the BASE cluster; each nested node is one such
// CR in TEST_CLUSTER_NAMESPACE, exposing its IP at status.ipAddress
// (github.com/deckhouse/virtualization api core v1alpha2, group virtualization.deckhouse.io).
var virtualMachineGVR = schema.GroupVersionResource{
	Group: "virtualization.deckhouse.io", Version: "v1alpha2", Resource: "virtualmachines",
}

// --- base-cluster clients --------------------------------------------------

// baseClusterClientsetCache / baseClusterDynamicCache memoize the parent-cluster clients built from the
// storage-e2e BaseKubeconfig. The external publish path needs base-cluster API access (pod create/exec,
// VirtualMachine discovery) separate from the nested suiteClientset/suiteDyn.
var (
	baseClusterClientsetCache *clientgokube.Clientset
	baseClusterDynamicCache   dynamic.Interface
)

// baseClusterKubeconfig returns the parent (base) cluster rest.Config the storage-e2e framework connected
// to in its Step-4 base-cluster attach (the same handle phase-3 uses for the runtime VirtualDisk).
func baseClusterKubeconfig() (*rest.Config, error) {
	if suiteClusterResources == nil || suiteClusterResources.BaseKubeconfig == nil {
		return nil, fmt.Errorf("base-cluster kubeconfig unavailable (suiteClusterResources.BaseKubeconfig is nil); the external publish path needs the storage-e2e base-cluster handle")
	}
	return suiteClusterResources.BaseKubeconfig, nil
}

func baseClusterClientset() (*clientgokube.Clientset, error) {
	if baseClusterClientsetCache != nil {
		return baseClusterClientsetCache, nil
	}
	cfg, err := baseClusterKubeconfig()
	if err != nil {
		return nil, err
	}
	cs, err := clientgokube.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build base-cluster clientset: %w", err)
	}
	baseClusterClientsetCache = cs
	return cs, nil
}

func baseClusterDynamic() (dynamic.Interface, error) {
	if baseClusterDynamicCache != nil {
		return baseClusterDynamicCache, nil
	}
	cfg, err := baseClusterKubeconfig()
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build base-cluster dynamic client: %w", err)
	}
	baseClusterDynamicCache = dyn
	return dyn, nil
}

// --- (1) auth: SA / Role / RoleBinding / TokenRequest ----------------------

// createServiceAccountIfNotExists creates a ServiceAccount in the nested cluster (idempotent).
func createServiceAccountIfNotExists(ctx context.Context, ns, name string) error {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if _, err := suiteClientset.CoreV1().ServiceAccounts(ns).Create(ctx, sa, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create ServiceAccount %s/%s: %w", ns, name, err)
	}
	return nil
}

// createDataDownloadRole creates a Role granting `create` on <resource>/download in the storage-foundation
// API group. resource is dataExportGVR.Resource ("dataexports") for DataExport downloads or
// dataImportGVR.Resource ("dataimports") for DataImport uploads: the data-importer authorizes uploads via
// a SubjectAccessReview on the SAME "download" subresource — confirmed in storage-foundation
// images/data-exporter/internal/repository/k8s.go AuthorizeUser, which switches only the resource
// (dataexports/dataimports) and always uses Subresource:"download", Verb:"create". The granted group MUST
// equal the DataExport/DataImport API group or the SAR denies and the server returns 403.
func createDataDownloadRole(ctx context.Context, ns, roleName, resource string) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: ns},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{dataExportGVR.Group}, // storage-foundation.deckhouse.io (same for DE and DI)
			Resources: []string{resource + "/download"},
			Verbs:     []string{"create"},
		}},
	}
	if _, err := suiteClientset.RbacV1().Roles(ns).Create(ctx, role, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create download Role %s/%s: %w", ns, roleName, err)
	}
	return nil
}

// createManifestsDownloadRole creates a Role granting `get` on snapshots/manifests-download in the
// aggregated subresources.state-snapshotter.deckhouse.io group. The aggregated apiserver runs with
// delegated authorization (SubjectAccessReview per request — internal/api/server.go), and registers the
// subresource as "snapshots/manifests-download" with verb "get" (internal/api/archive_handler.go), so this
// namespaced Role is what a token needs to read published manifests through the same origin ingress host.
func createManifestsDownloadRole(ctx context.Context, ns, roleName string) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: ns},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{coreSubresGroup}, // subresources.state-snapshotter.deckhouse.io
			Resources: []string{"snapshots/" + subManifestsDownload},
			Verbs:     []string{"get"},
		}},
	}
	if _, err := suiteClientset.RbacV1().Roles(ns).Create(ctx, role, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create manifests-download Role %s/%s: %w", ns, roleName, err)
	}
	return nil
}

// bindRoleToServiceAccount creates a RoleBinding in roleNS tying roleName to the ServiceAccount saNS/saName
// (idempotent). Split from role creation on purpose: the negative "403 before RoleBinding" scenarios mint a
// token and probe it while only the SA + Role exist, then call this to grant access and re-probe for 200.
func bindRoleToServiceAccount(ctx context.Context, roleNS, bindingName, roleName, saNS, saName string) error {
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingName, Namespace: roleNS},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      saName,
			Namespace: saNS,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     roleName,
		},
	}
	if _, err := suiteClientset.RbacV1().RoleBindings(roleNS).Create(ctx, binding, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create RoleBinding %s/%s: %w", roleNS, bindingName, err)
	}
	return nil
}

// issueServiceAccountToken mints a Bearer token for the SA via the TokenRequest API (the programmatic
// `kubectl create token`). The token is returned as a plain string for the specs to pass to the curl
// runner; it is never written to a Secret or pod spec.
func issueServiceAccountToken(ctx context.Context, ns, saName string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = publishTokenTTL
	}
	secs := int64(ttl.Seconds())
	// The kube-apiserver enforces a minimum TokenRequest expiration (default 10m); floor to be safe.
	if secs < 600 {
		secs = 600
	}
	tr := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			ExpirationSeconds: &secs,
			// No audiences: the token defaults to the kube-apiserver audience, which is what both the
			// aggregated apiserver's delegated authn and the data-exporter/importer TokenReview validate
			// against.
			// TODO(e2e-publish): verify on cluster — confirm the default (apiserver) audience is accepted by
			// the data-exporter/importer TokenReview and the aggregated apiserver; set explicit Audiences if
			// a bound audience is required.
		},
	}
	resp, err := suiteClientset.CoreV1().ServiceAccounts(ns).CreateToken(ctx, saName, tr, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("issue TokenRequest for SA %s/%s: %w", ns, saName, err)
	}
	if resp.Status.Token == "" {
		return "", fmt.Errorf("TokenRequest for SA %s/%s returned an empty token", ns, saName)
	}
	return resp.Status.Token, nil
}

// --- (2) master IP for the external --resolve fallback ---------------------

// publishMasterIP returns the nested master IP for the external curl `--resolve` fallback, or "" if it
// cannot be determined (in which case the external path relies on public sslip.io DNS only). Primary: the
// IP embedded in the sslip.io publish domain (suitePublishInfra.masterIP). Fallback: the first
// status.ipAddress found on the base-cluster VirtualMachine CRs in the nested VMs' namespace.
func publishMasterIP(ctx context.Context) string {
	if ip := strings.TrimSpace(suitePublishInfra.masterIP); ip != "" {
		return ip
	}
	// TODO(e2e-publish): verify on cluster — confirm the nested nodes are VirtualMachine CRs
	// (virtualization.deckhouse.io/v1alpha2) in TEST_CLUSTER_NAMESPACE exposing .status.ipAddress, and that
	// the node the publish host resolves to (the ingress-serving master) is the one selected here.
	dyn, err := baseClusterDynamic()
	if err != nil {
		GinkgoWriter.Printf("publishMasterIP: base-cluster dynamic client unavailable: %v\n", err)
		return ""
	}
	ns := strings.TrimSpace(suiteCfg.vmNamespace)
	if ns == "" {
		GinkgoWriter.Printf("publishMasterIP: TEST_CLUSTER_NAMESPACE is empty; cannot discover nested VM IPs\n")
		return ""
	}
	list, err := dyn.Resource(virtualMachineGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		GinkgoWriter.Printf("publishMasterIP: list VirtualMachines in base ns %s: %v\n", ns, err)
		return ""
	}
	for i := range list.Items {
		ip, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "ipAddress")
		if strings.TrimSpace(ip) != "" {
			return strings.TrimSpace(ip)
		}
	}
	GinkgoWriter.Printf("publishMasterIP: no VirtualMachine in base ns %s exposes status.ipAddress\n", ns)
	return ""
}

// --- (3) curl-runner (base cluster) ----------------------------------------

// curlPodTarget identifies a pod (in either the nested or the base cluster) that runExec-style curl helpers
// exec into. externalCurlTarget() addresses the base-cluster runner; nestedCurlTarget() addresses a pod in
// the nested cluster (e.g. the backup-client pod) for the in-cluster (status.url) checks.
type curlPodTarget struct {
	kubeconfig *rest.Config
	ns         string
	pod        string
	container  string
}

func (t curlPodTarget) exec(ctx context.Context, cmd []string) (stdout, stderr string, err error) {
	return storagekube.ExecInPod(ctx, t.kubeconfig, t.ns, t.pod, t.container, cmd)
}

// externalCurlTarget addresses the base-cluster curl-runner pod. Its kubeconfig is the base cluster's, so
// exec streams run against the parent cluster.
func externalCurlTarget() (curlPodTarget, error) {
	cfg, err := baseClusterKubeconfig()
	if err != nil {
		return curlPodTarget{}, err
	}
	return curlPodTarget{kubeconfig: cfg, ns: suiteCfg.vmNamespace, pod: publishExtCurlPod, container: publishExtCurlCont}, nil
}

// nestedCurlTarget addresses a pod in the nested cluster (uses suiteRestCfg). Used for the internal
// (status.url) publish checks from an in-cluster pod.
func nestedCurlTarget(ns, pod, container string) curlPodTarget {
	return curlPodTarget{kubeconfig: suiteRestCfg, ns: ns, pod: pod, container: container}
}

// externalCurlPodSpec is the base-cluster runner pod: a long-sleeping curlimages/curl container with its
// ServiceAccount token automount DISABLED, so the only credential any request can carry is the nested
// Bearer token passed per-exec (never a base-cluster identity).
func externalCurlPodSpec(ns string) *corev1.Pod {
	automount := false
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: publishExtCurlPod, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyAlways,
			AutomountServiceAccountToken: &automount,
			Containers: []corev1.Container{{
				Name:    publishExtCurlCont,
				Image:   suiteCfg.backupClientImage,
				Command: []string{"sh", "-c", "sleep 360000"},
			}},
		},
	}
}

// ensureExternalCurlPod creates (idempotently) and waits for the base-cluster curl-runner pod. The pod
// lives in TEST_CLUSTER_NAMESPACE alongside the nested VMs so it can reach api.<domain>:443 on the DVP
// network. Respect the keep-cluster knobs in the paired teardown (deleteExternalCurlPod).
func ensureExternalCurlPod(ctx context.Context) error {
	cs, err := baseClusterClientset()
	if err != nil {
		return err
	}
	ns := strings.TrimSpace(suiteCfg.vmNamespace)
	if ns == "" {
		return fmt.Errorf("TEST_CLUSTER_NAMESPACE is empty; cannot place the external curl pod in the nested VMs' base namespace")
	}
	if _, err := cs.CoreV1().Pods(ns).Create(ctx, externalCurlPodSpec(ns), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create external curl pod %s/%s on the base cluster: %w", ns, publishExtCurlPod, err)
	}
	return waitBasePodRunning(ctx, ns, publishExtCurlPod, 5*time.Minute)
}

// deleteExternalCurlPod tears the base-cluster runner down unless a keep-cluster knob asked to preserve it.
func deleteExternalCurlPod(ctx context.Context) {
	if cleanupSkipped() {
		GinkgoWriter.Printf("%s: keeping base-cluster external curl pod %s/%s\n", keepReason(), suiteCfg.vmNamespace, publishExtCurlPod)
		return
	}
	cs, err := baseClusterClientset()
	if err != nil {
		return
	}
	_ = cs.CoreV1().Pods(suiteCfg.vmNamespace).Delete(ctx, publishExtCurlPod, metav1.DeleteOptions{})
}

// waitBasePodRunning is waitPodRunning against the BASE cluster clientset (waitPodRunning uses the nested
// suiteClientset).
func waitBasePodRunning(ctx context.Context, ns, name string, timeout time.Duration) error {
	cs, err := baseClusterClientset()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var last string
	for {
		pod, gerr := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if gerr == nil {
			if pod.Status.Phase == corev1.PodRunning {
				return nil
			}
			last = fmt.Sprintf("phase=%s", pod.Status.Phase)
		} else {
			last = fmt.Sprintf("get err=%v", gerr)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for base-cluster pod %s/%s Running; last: %s", ns, name, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// curlRequest describes one HTTP request the curl runner performs against a DataExport/DataImport/manifests
// URL.
type curlRequest struct {
	method   string            // GET (default), PUT, POST
	token    string            // Bearer token; "" issues NO Authorization header (the "no token" negative)
	headers  map[string]string // extra headers (e.g. X-Content-Length / X-Offset for a Block PUT)
	dataFile string            // in-pod path for `--data-binary @<file>` (Block upload); "" for none
	caFile   string            // in-pod CA path for `--cacert`; "" uses `-k` (skip TLS verify)
	// masterIP enables the `--resolve api.<host>:443:<masterIP>` fallback when the direct request yields no
	// HTTP response (transport/DNS failure, curl code 000). "" disables the fallback (internal status.url
	// requests, which resolve via in-cluster DNS, leave this empty).
	masterIP string
}

// curlFlagsScript renders the fixed portion of a curl invocation (everything except the token, which is
// passed as argv $1, and the URL, argv $2). All interpolated values are test/cluster-controlled structural
// strings (methods, header names, file paths, the resolve triple) and are single-quoted; the Bearer token
// and URL are NEVER interpolated. resolveArg is "" for a direct request or "api.<host>:443:<ip>".
func curlFlagsScript(r curlRequest, resolveArg string) string {
	var b strings.Builder
	b.WriteString("curl -sS")
	if r.caFile != "" {
		b.WriteString(" --cacert " + shQuote(r.caFile))
	} else {
		b.WriteString(" -k")
	}
	if resolveArg != "" {
		b.WriteString(" --resolve " + shQuote(resolveArg))
	}
	method := strings.ToUpper(strings.TrimSpace(r.method))
	if method != "" && method != "GET" {
		b.WriteString(" -X " + shQuote(method))
	}
	if r.token != "" {
		// $1 is the token (argv), grouped into one header argument by the double quotes.
		b.WriteString(` -H "Authorization: Bearer $1"`)
	}
	for _, k := range sortedKeys(r.headers) {
		b.WriteString(" -H " + shQuote(k+": "+r.headers[k]))
	}
	if r.dataFile != "" {
		b.WriteString(" --data-binary @" + shQuote(r.dataFile))
	}
	return b.String()
}

// runCurlRequest execs one curl request in the target pod and returns the HTTP status code and response
// body. It intentionally omits `-f`, so a 4xx still yields the status code (rather than an error) — the
// negative specs assert on 401/403. When the direct request produces no HTTP response (code 000) and
// r.masterIP is set, it retries once with `--resolve` (the sslip DNS fallback).
func runCurlRequest(ctx context.Context, t curlPodTarget, rawURL string, r curlRequest) (statusCode int, body string, err error) {
	code, body, err := runCurlOnce(ctx, t, rawURL, r, "")
	if err != nil {
		return 0, "", err
	}
	if code == 0 && r.masterIP != "" {
		if host := hostFromURL(rawURL); host != "" {
			resolveArg := fmt.Sprintf("%s:443:%s", host, r.masterIP)
			GinkgoWriter.Printf("runCurlRequest: direct request to %s got no HTTP response; retrying with --resolve %s\n", rawURL, resolveArg)
			code, body, err = runCurlOnce(ctx, t, rawURL, r, resolveArg)
			if err != nil {
				return 0, "", err
			}
		}
	}
	if code == 0 {
		return 0, body, fmt.Errorf("curl to %s produced no HTTP response (code 000); body/stderr=%q", rawURL, truncate([]byte(body), 512))
	}
	return code, body, nil
}

// runCurlOnce performs a single curl attempt (no fallback) and parses the status code + body out of the
// runner's stdout.
func runCurlOnce(ctx context.Context, t curlPodTarget, rawURL string, r curlRequest, resolveArg string) (int, string, error) {
	// Build: <curl flags> -o <bodyFile> -w 'HTTPCODE:%{http_code}\n' "$2"; then emit a marker and the body.
	// $0=sh (placeholder), $1=token, $2=url.
	script := curlFlagsScript(r, resolveArg) +
		" -o " + shQuote(publishExtBodyFile) +
		` -w 'HTTPCODE:%{http_code}\n' "$2"; echo '---BODY---'; cat ` + shQuote(publishExtBodyFile) + " 2>/dev/null; rm -f " + shQuote(publishExtBodyFile)
	stdout, stderr, err := t.exec(ctx, []string{"sh", "-c", script, "sh", r.token, rawURL})
	if err != nil {
		return 0, "", fmt.Errorf("exec curl in %s/%s: %w (stderr=%q)", t.ns, t.pod, err, stderr)
	}
	code, body := parseHTTPCodeBody(stdout)
	return code, body, nil
}

// runCurlChecksum streams a GET response through sha256sum INSIDE the runner pod and returns the hex
// digest, so a large payload is never pulled to the test-runner disk. It uses `-f` (fail on >=400 so an
// auth error is not hashed as data). Failure detection for the `--resolve` fallback uses the empty-stream
// SHA-256 sentinel (see sha256Empty): a failed `curl -f | sha256sum` hashes zero bytes.
func runCurlChecksum(ctx context.Context, t curlPodTarget, rawURL string, r curlRequest) (string, error) {
	sum, err := runCurlChecksumOnce(ctx, t, rawURL, r, "")
	if err != nil {
		return "", err
	}
	if sum == sha256Empty && r.masterIP != "" {
		if host := hostFromURL(rawURL); host != "" {
			resolveArg := fmt.Sprintf("%s:443:%s", host, r.masterIP)
			GinkgoWriter.Printf("runCurlChecksum: direct stream from %s delivered no bytes; retrying with --resolve %s\n", rawURL, resolveArg)
			sum, err = runCurlChecksumOnce(ctx, t, rawURL, r, resolveArg)
			if err != nil {
				return "", err
			}
		}
	}
	if len(sum) != 64 {
		return "", fmt.Errorf("unexpected checksum %q streaming %s", sum, rawURL)
	}
	// A post-fallback empty-stream hash means the request delivered zero bytes (curl -f failed on both the
	// direct and --resolve attempts, or no fallback was configured). The published payloads under test are
	// never empty, so surface this as an error rather than returning the sentinel as a real digest — a
	// consuming spec that only compares external==internal would otherwise false-PASS when both legs fail.
	if sum == sha256Empty {
		return "", fmt.Errorf("streaming %s delivered no bytes (empty-stream SHA-256): the download failed (auth/transport) or the payload is unexpectedly empty", rawURL)
	}
	return sum, nil
}

func runCurlChecksumOnce(ctx context.Context, t curlPodTarget, rawURL string, r curlRequest, resolveArg string) (string, error) {
	// -f so a >=400 response is not streamed into sha256sum. No pipefail (busybox ash portability): a failed
	// curl -f simply pipes zero bytes, which sha256sum hashes to sha256Empty — the caller's fallback
	// sentinel. sha256sum here is the busybox applet shipped by curlimages/curl (matching the source-side
	// `dd | sha256sum`).
	flags := strings.Replace(curlFlagsScript(r, resolveArg), "curl -sS", "curl -fsS", 1)
	script := flags + ` "$2" | sha256sum | awk '{print $1}'`
	stdout, stderr, err := t.exec(ctx, []string{"sh", "-c", script, "sh", r.token, rawURL})
	if err != nil {
		return "", fmt.Errorf("exec curl|sha256sum in %s/%s for %s: %w (stderr=%q)", t.ns, t.pod, rawURL, err, stderr)
	}
	return strings.TrimSpace(stdout), nil
}

// --- (4) CA (status.ca) helpers --------------------------------------------

// normalizePublishCA returns the PEM form of a DataExport/DataImport status.ca. Per the CRD schema
// (crds/dataexports.yaml / dataimports.yaml: "Base64 encoded CA certificate ...") the field carries
// base64-encoded PEM, so callers that feed it to pem.Decode / `curl --cacert` must decode it first.
// It is tolerant: a value that already starts with a PEM block (a future/other build serving raw PEM)
// is returned unchanged, and a value that is not valid base64 is returned as-is so the downstream
// PEM parse surfaces the real problem instead of a decode error here.
func normalizePublishCA(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "-----BEGIN") {
		return raw
	}
	if dec, err := base64.StdEncoding.DecodeString(raw); err == nil {
		return string(dec)
	}
	return raw
}

// caIssuerCommonName parses a PEM CA/cert bundle (status.ca) and returns the Issuer CommonName of the first
// certificate. The published TLS chain is served under the data-exporter's own CA, so the DE/DI specs
// assert this contains "data-exporter-CA" (the Go equivalent of the manual `openssl x509 -issuer` check).
func caIssuerCommonName(caPEM string) (string, error) {
	block, _ := pem.Decode([]byte(caPEM))
	if block == nil {
		return "", fmt.Errorf("status.ca is not PEM-decodable")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse status.ca certificate: %w", err)
	}
	return cert.Issuer.CommonName, nil
}

// writeCAToPodFile materializes a PEM CA bundle to a file inside a pod (nested or base) so a curl there can
// use `--cacert` instead of `-k`. The PEM is passed as an exec argv value ($1) — never persisted in a
// resource — and written verbatim with printf. Returns the in-pod path (publishCAPodFile).
func writeCAToPodFile(ctx context.Context, t curlPodTarget, caPEM string) (string, error) {
	script := `printf '%s' "$1" > ` + shQuote(publishCAPodFile)
	_, stderr, err := t.exec(ctx, []string{"sh", "-c", script, "sh", caPEM})
	if err != nil {
		return "", fmt.Errorf("write CA into %s/%s: %w (stderr=%q)", t.ns, t.pod, err, stderr)
	}
	return publishCAPodFile, nil
}

// --- URL / parsing helpers -------------------------------------------------

// publishDataURL joins a DataExport/DataImport base URL (status.url or status.publicURL, which carry a
// trailing slash) with a data-plane suffix, trimming the trailing slash so we never emit "//api/v1/..."
// (Go's ServeMux would 301-redirect a non-canonical path).
func publishDataURL(baseURL, suffix string) string {
	return strings.TrimRight(baseURL, "/") + suffix
}

// externalManifestsURL builds the external aggregated manifests-download URL for a namespaced Snapshot:
// the origin ingress host (api.<publicDomain>) plus the same aggregated path served in-cluster. Proves the
// published kube-API ingress needs no separate APIService ingress.
func externalManifestsURL(ns, snapshot string) string {
	return "https://" + suitePublishInfra.originIngressHost + coreSnapshotSubPath(ns, snapshot, subManifestsDownload)
}

// hostFromURL returns the host (no port) of a URL, for building the `--resolve api.<host>:443:<ip>` triple.
func hostFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// parseHTTPCodeBody extracts the status code and body from a runCurlOnce stdout of the form
// "HTTPCODE:<code>\n---BODY---\n<body...>". Returns code 0 when the marker is absent or unparsable
// (curl transport failure prints http_code 000).
func parseHTTPCodeBody(stdout string) (int, string) {
	const marker = "---BODY---"
	code := 0
	body := ""
	idx := strings.Index(stdout, marker)
	head := stdout
	if idx >= 0 {
		head = stdout[:idx]
		body = stdout[idx+len(marker):]
		body = strings.TrimPrefix(body, "\n")
	}
	for _, line := range strings.Split(head, "\n") {
		line = strings.TrimSpace(line)
		if v := strings.TrimPrefix(line, "HTTPCODE:"); v != line {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				code = n
			}
			break
		}
	}
	return code, body
}

// shQuote single-quotes a string for safe embedding in a /bin/sh command (POSIX single-quote escaping).
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sortedKeys returns the map keys sorted, so a rendered curl command (and thus test behavior) is
// deterministic regardless of Go map iteration order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
