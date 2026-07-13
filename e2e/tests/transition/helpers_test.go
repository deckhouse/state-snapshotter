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

package transition

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	clientgokube "k8s.io/client-go/kubernetes"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// --- shared clients (initialized in BeforeSuite) ---------------------------

var (
	suiteClientset *clientgokube.Clientset
	suiteDyn       dynamic.Interface
)

const pollInterval = 3 * time.Second

// progressLogInterval throttles the "still waiting …" progress lines the long waits emit while they
// poll every pollInterval. The actual readiness check runs every pollInterval; only the logging is
// throttled, so a multi-minute wait produces a readable trail instead of hundreds of lines.
const progressLogInterval = 15 * time.Second

// logf writes a timestamped progress line to the Ginkgo output (shown with -v and always on failure).
// Used by the long waits so it is clear which step is running and what the cluster currently looks
// like — instead of an opaque hang followed by a bare timeout.
func logf(format string, args ...interface{}) {
	fmt.Fprintf(GinkgoWriter, "[transition %s] "+format+"\n",
		append([]interface{}{time.Now().Format("15:04:05")}, args...)...)
}

// pollUntil polls done() every pollInterval until it returns true or timeout elapses. It logs the
// current state (describe()) immediately, then every progressLogInterval, and once more on success;
// on timeout it fails with the final description. This makes every long wait self-narrating.
func pollUntil(ctx context.Context, what string, timeout time.Duration, done func() bool, describe func() string) {
	GinkgoHelper()
	start := time.Now()
	deadline := start.Add(timeout)
	logf("waiting for %s (timeout %s, polling every %s) — %s", what, timeout, pollInterval, describe())
	lastLog := start
	for {
		if done() {
			logf("%s: ready after %s", what, time.Since(start).Round(time.Second))
			return
		}
		if time.Now().After(deadline) {
			Fail(fmt.Sprintf("timeout after %s waiting for %s — last state: %s", timeout, what, describe()))
		}
		if time.Since(lastLog) >= progressLogInterval {
			logf("still waiting for %s (%s/%s) — %s", what, time.Since(start).Round(time.Second), timeout, describe())
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			Fail(fmt.Sprintf("context cancelled while waiting for %s: %v — last state: %s", what, ctx.Err(), describe()))
		case <-time.After(pollInterval):
		}
	}
}

// --- GVRs ------------------------------------------------------------------

var (
	pvcGVR = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	nsGVR  = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

	volumeSnapshotGVR = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}

	// Deckhouse surfaces every firing Prometheus alert as a cluster-scoped ClusterAlert; the
	// deprecation assertions read this instead of scraping PrometheusRule specs.
	clusterAlertGVR = schema.GroupVersionResource{Group: "deckhouse.io", Version: "v1alpha1", Resource: "clusteralerts"}

	// DataExport/DataImport live under the LEGACY group in phase B (svdm pre-D1) and under the
	// unified group from phase C onward (svdm D1). dataExportGVR(group)/dataImportGVR(group) build
	// the right GVR per phase.
	legacyGroup  = "storage.deckhouse.io"
	unifiedGroup = "storage-foundation.deckhouse.io"
)

func dataExportGVR(group string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: "v1alpha1", Resource: "dataexports"}
}

func dataImportGVR(group string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: "v1alpha1", Resource: "dataimports"}
}

// module namespaces, indexed by module name.
var moduleNamespace = map[string]string{
	modSnapshotController: "d8-snapshot-controller",
	modSvdm:               "d8-storage-volume-data-manager",
	modStateSnapshotter:   "d8-state-snapshotter",
	modStorageFoundation:  "d8-storage-foundation",
}

// localCSIDriver / annStorageClassVSC mirror the main suite: the local CSI provisioner, and the
// StorageClass annotation the state-snapshotter capture path resolves the VolumeSnapshotClass through
// (it never falls back to the cluster default).
const (
	localCSIDriver     = "local.csi.storage.deckhouse.io"
	annStorageClassVSC = "storage.deckhouse.io/volumesnapshotclass"
)

// ensureDataPlaneStorage provisions the thin, snapshot-capable StorageClass (E2E_TRANSITION_STORAGE_CLASS)
// and its VolumeSnapshotClass (E2E_TRANSITION_VS_CLASS) that the data-plane phases need, mirroring the
// main suite's phase-3 setup. Idempotent: a no-op where they already exist (existing cluster); on a
// fresh alwaysCreateNew cluster it provisions the LVM backend (attaching disks to the worker VMs via
// suiteRes.BaseKubeconfig) and creates the classes. MUST run after sds-local-volume is Ready (phase B),
// since EnsureDefaultStorageClass drives LVGs / LocalStorageClass through it.
func ensureDataPlaneStorage(ctx context.Context) {
	GinkgoHelper()
	scName := strings.TrimSpace(os.Getenv(envStorageClass))
	vscName := strings.TrimSpace(os.Getenv(envVSClass))

	// 1) StorageClass (+ LVM backend). BaseKubeconfig is set only for alwaysCreateNew; then disks are
	//    attached to the worker VMs on the base cluster. For an already-present SC this is a no-op.
	_, err := testkit.EnsureDefaultStorageClass(ctx, suiteRes.Kubeconfig, testkit.DefaultStorageClassConfig{
		StorageClassName:     scName,
		LVMType:              "Thin",
		ThinPoolName:         "thinpool",
		BaseKubeconfig:       suiteRes.BaseKubeconfig,
		VMNamespace:          strings.TrimSpace(os.Getenv("TEST_CLUSTER_NAMESPACE")),
		BaseStorageClassName: strings.TrimSpace(os.Getenv("TEST_CLUSTER_STORAGE_CLASS")),
	})
	Expect(err).NotTo(HaveOccurred(), "ensure StorageClass %s", scName)

	// 2) VolumeSnapshotClass for the local CSI driver — create it if E2E_TRANSITION_VS_CLASS is absent.
	if _, gerr := suiteDyn.Resource(storagekube.VolumeSnapshotClassGVR).Get(ctx, vscName, metav1.GetOptions{}); gerr != nil {
		Expect(storagekube.CreateVolumeSnapshotClass(ctx, suiteRes.Kubeconfig, storagekube.VolumeSnapshotClassConfig{
			Name:           vscName,
			Driver:         localCSIDriver,
			DeletionPolicy: "Delete",
		})).To(Succeed(), "create VolumeSnapshotClass %s", vscName)
	}

	// 3) Wire the SC -> VSC annotation so the domain capture path (phase D) resolves the class.
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, annStorageClassVSC, vscName))
	_, err = suiteClientset.StorageV1().StorageClasses().Patch(ctx, scName, types.MergePatchType, patch, metav1.PatchOptions{})
	Expect(err).NotTo(HaveOccurred(), "annotate StorageClass %s with %s", scName, annStorageClassVSC)
}

// deletePodAndWait deletes a pod and waits until it is fully gone. svdm's PVC export rejects a source
// PVC that is still occupied by a pod ("PVC isn't free"), so the probe pod that wrote the marker must
// be removed — and confirmed gone — before the DataExport.
func deletePodAndWait(ctx context.Context, ns, name string, timeout time.Duration) {
	GinkgoHelper()
	err := suiteClientset.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return
	}
	Expect(err).NotTo(HaveOccurred(), "delete pod %s/%s", ns, name)
	Eventually(func() bool {
		_, gerr := suiteClientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		return apierrors.IsNotFound(gerr)
	}).WithContext(ctx).WithTimeout(timeout).WithPolling(2*time.Second).Should(BeTrue(),
		"pod %s/%s must be fully deleted before the source PVC is exported", ns, name)
}

// --- low-level helpers -----------------------------------------------------

// execSh runs `sh -c script` in a pod container and returns stdout (trimmed).
func execSh(ctx context.Context, ns, pod, container, script string) (string, error) {
	stdout, stderr, err := storagekube.ExecInPod(ctx, suiteRes.Kubeconfig, ns, pod, container, []string{"sh", "-c", script})
	if err != nil {
		return "", fmt.Errorf("exec in %s/%s[%s]: %w (stderr=%q)", ns, pod, container, err, stderr)
	}
	return strings.TrimSpace(stdout), nil
}

// writeMarkerChecksum writes deterministic data to path inside the pod and returns its sha256.
func writeMarkerChecksum(ctx context.Context, ns, pod, container, path string) (string, error) {
	// Deterministic content (seeded), then checksum — so a restored/imported copy can be compared.
	script := fmt.Sprintf(
		"head -c 1048576 /dev/zero | tr '\\0' 'A' > %s && sync && sha256sum %s | awk '{print $1}'",
		path, path,
	)
	return execSh(ctx, ns, pod, container, script)
}

// checksumFile returns the sha256 of a file inside the pod.
func checksumFile(ctx context.Context, ns, pod, container, path string) (string, error) {
	return execSh(ctx, ns, pod, container, fmt.Sprintf("sha256sum %s | awk '{print $1}'", path))
}

// createProbePod creates a sleeping pod mounting the given PVCs at /mnt/<pvc> and waits for Running.
// image must provide `sh`, `sha256sum` and (for the svdm HTTP steps) `curl`.
func createProbePod(ctx context.Context, ns, name, image string, pvcs ...string) {
	GinkgoHelper()
	var mounts []corev1.VolumeMount
	var volumes []corev1.Volume
	for _, p := range pvcs {
		mounts = append(mounts, corev1.VolumeMount{Name: p, MountPath: "/mnt/" + p})
		volumes = append(volumes, corev1.Volume{
			Name:         p,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: p}},
		})
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name:         "probe",
				Image:        image,
				Command:      []string{"sh", "-c", "sleep 360000"},
				VolumeMounts: mounts,
			}},
			Volumes: volumes,
		},
	}
	_, err := suiteClientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred(), "create probe pod %s/%s", ns, name)
	}
	pollUntil(ctx, fmt.Sprintf("probe pod %s/%s Running", ns, name), podRunningTimeout(),
		func() bool {
			p, gerr := suiteClientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
			return gerr == nil && p.Status.Phase == corev1.PodRunning
		},
		func() string {
			p, gerr := suiteClientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
			if gerr != nil {
				return fmt.Sprintf("get err=%v", gerr)
			}
			return fmt.Sprintf("phase=%s%s | PVCs bound? %s", p.Status.Phase, podNotReadyReason(p), pvcsBoundSummary(ctx, ns, pvcs))
		})
}

// pvcsBoundSummary renders "name=Phase" for the given PVCs — a probe pod stuck Pending is almost
// always a PVC still binding, so surface that alongside the pod phase.
func pvcsBoundSummary(ctx context.Context, ns string, pvcs []string) string {
	if len(pvcs) == 0 {
		return "n/a"
	}
	parts := make([]string, 0, len(pvcs))
	for _, p := range pvcs {
		parts = append(parts, fmt.Sprintf("%s=%s", p, pvcPhase(ctx, ns, p)))
	}
	return strings.Join(parts, ",")
}

// podRunningTimeout bounds how long a probe pod may take to reach Running. For the import probe this
// budget spans the WHOLE import completion (upload -> importer UploadFinished -> populator rebind of
// the prime volume -> target PVC Bound -> pod schedule), which can exceed a few minutes on a busy or
// slow cluster. Override with E2E_TRANSITION_PROBE_TIMEOUT (Go duration, e.g. 15m); default 10m.
func podRunningTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("E2E_TRANSITION_PROBE_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 10 * time.Minute
}

// waitImportComplete blocks until the DataImport's target PVC reaches Bound — the real import
// completion gate (the lib-volume-populator rebinds the prime volume onto the target PVC; the
// DataImport Ready condition flips True early and there is no Completed condition type, so the PVC
// phase is what actually signals "done"). While waiting it logs, every progressLogInterval, the
// DataImport conditions, the target PVC phase, any prime-* staging PVCs and the namespace's pods, so
// a hang is diagnosable from the log (is the importer up? did UploadFinished fire? is the prime PVC
// stuck Pending? is a pod unschedulable?) instead of an opaque timeout. timeout budgets the whole
// chain: upload -> importer UploadFinished -> populator rebind -> target PVC Bound.
func waitImportComplete(ctx context.Context, group, ns, diName, pvcName string, timeout time.Duration) {
	GinkgoHelper()
	gvr := dataImportGVR(group)
	describe := func() string {
		return fmt.Sprintf("DI[%s] | PVC %s=%s | %s | pods[%s]",
			crConditions(ctx, gvr, ns, diName),
			pvcName, pvcPhase(ctx, ns, pvcName),
			describePrimePVCs(ctx, ns),
			podPhasesInNS(ctx, ns))
	}
	pollUntil(ctx, fmt.Sprintf("imported PVC %s/%s Bound", ns, pvcName), timeout,
		func() bool { return pvcPhase(ctx, ns, pvcName) == string(corev1.ClaimBound) },
		describe)
}

// crConditions renders "Type=Status(Reason)" for every status.condition of a CR (compact, for logs).
func crConditions(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) string {
	obj, err := getUnstr(ctx, gvr, ns, name)
	if err != nil {
		return fmt.Sprintf("get err=%v", err)
	}
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if len(conds) == 0 {
		return "no conditions yet"
	}
	parts := make([]string, 0, len(conds))
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		t, _, _ := unstructured.NestedString(m, "type")
		s, _, _ := unstructured.NestedString(m, "status")
		r, _, _ := unstructured.NestedString(m, "reason")
		parts = append(parts, fmt.Sprintf("%s=%s(%s)", t, s, r))
	}
	return strings.Join(parts, " ")
}

// pvcPhase returns a PVC's phase, or "NotFound"/"err=…" when it cannot be read (for logs).
func pvcPhase(ctx context.Context, ns, name string) string {
	p, err := suiteClientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "NotFound"
		}
		return fmt.Sprintf("err=%v", err)
	}
	return string(p.Status.Phase)
}

// describePrimePVCs lists the staging PVCs the volume-populator creates (name prefix "prime-") with
// their phase, e.g. "prime: prime-abc=Pending" — the usual place an import stalls (WFFC/scheduling).
func describePrimePVCs(ctx context.Context, ns string) string {
	list, err := suiteClientset.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Sprintf("prime PVCs err=%v", err)
	}
	var parts []string
	for i := range list.Items {
		if strings.HasPrefix(list.Items[i].Name, "prime-") {
			parts = append(parts, fmt.Sprintf("%s=%s", list.Items[i].Name, list.Items[i].Status.Phase))
		}
	}
	if len(parts) == 0 {
		return "no prime PVCs"
	}
	return "prime: " + strings.Join(parts, ",")
}

// podPhasesInNS returns "name=Phase<reason>" for every pod in ns (importer + populator pods), where
// <reason> is a container-waiting/unschedulable hint when the pod is not yet Running.
func podPhasesInNS(ctx context.Context, ns string) string {
	pods, err := suiteClientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Sprintf("err=%v", err)
	}
	if len(pods.Items) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(pods.Items))
	for i := range pods.Items {
		parts = append(parts, fmt.Sprintf("%s=%s%s", pods.Items[i].Name, pods.Items[i].Status.Phase, podNotReadyReason(&pods.Items[i])))
	}
	return strings.Join(parts, ",")
}

// podNotReadyReason summarizes why a pod is not Running yet: container waiting reasons
// (ContainerCreating/ImagePullBackOff/…) and an unsatisfied PodScheduled condition. Empty when none.
func podNotReadyReason(p *corev1.Pod) string {
	var parts []string
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			parts = append(parts, fmt.Sprintf("%s:%s", cs.Name, cs.State.Waiting.Reason))
		}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status != corev1.ConditionTrue {
			parts = append(parts, fmt.Sprintf("PodScheduled=%s(%s)", c.Status, c.Reason))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// ensureNamespace creates a namespace if absent.
func ensureNamespace(ctx context.Context, name string) {
	GinkgoHelper()
	_, err := storagekube.CreateNamespaceIfNotExists(ctx, suiteRes.Kubeconfig, name)
	Expect(err).NotTo(HaveOccurred(), "ensure namespace %s", name)
}

// namespaceExists reports whether a namespace is present.
func namespaceExists(ctx context.Context, name string) bool {
	_, err := suiteDyn.Resource(nsGVR).Get(ctx, name, metav1.GetOptions{})
	return err == nil
}

// crdExists reports whether a CRD by full name (plural.group) is present.
func crdExists(ctx context.Context, name string) bool {
	_, err := suiteDyn.Resource(crdGVR).Get(ctx, name, metav1.GetOptions{})
	return err == nil
}

// getUnstr fetches a namespaced (ns != "") or cluster-scoped object.
func getUnstr(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if ns == "" {
		return suiteDyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	}
	return suiteDyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
}

// applyUnstr creates a namespaced unstructured object (create-only; AlreadyExists tolerated).
func applyUnstr(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj map[string]interface{}) error {
	u := &unstructured.Unstructured{Object: obj}
	_, err := suiteDyn.Resource(gvr).Namespace(ns).Create(ctx, u, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// waitCSIVolumeSnapshotReady waits until a CSI VolumeSnapshot is readyToUse AND bound to a content.
// Returns the bound VolumeSnapshotContent name.
func waitCSIVolumeSnapshotReady(ctx context.Context, ns, name string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		vs, err := getUnstr(ctx, volumeSnapshotGVR, ns, name)
		if err == nil {
			ready, _, _ := unstructured.NestedBool(vs.Object, "status", "readyToUse")
			content, _, _ := unstructured.NestedString(vs.Object, "status", "boundVolumeSnapshotContentName")
			if ready && content != "" {
				return content, nil
			}
			last = fmt.Sprintf("readyToUse=%v boundContent=%q", ready, content)
		} else {
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timeout waiting for VolumeSnapshot %s/%s ready+bound; last: %s", ns, name, last)
		}
		time.Sleep(pollInterval)
	}
}

// waitStatusString waits until a nested status string field on an object becomes non-empty and
// returns it (used for DataExport/DataImport status.url).
func waitStatusString(ctx context.Context, gvr schema.GroupVersionResource, ns, name string, timeout time.Duration, fields ...string) (string, error) {
	start := time.Now()
	deadline := start.Add(timeout)
	path := append([]string{"status"}, fields...)
	field := strings.Join(fields, ".")
	what := fmt.Sprintf("%s %s/%s status.%s", gvr.Resource, ns, name, field)
	logf("waiting for %s (timeout %s, polling every %s)", what, timeout, pollInterval)
	lastLog := start
	var last string
	for {
		obj, err := getUnstr(ctx, gvr, ns, name)
		if err == nil {
			if v, found, _ := unstructured.NestedString(obj.Object, path...); found && v != "" {
				logf("%s ready after %s", what, time.Since(start).Round(time.Second))
				return v, nil
			}
			last = "empty; conds: " + crConditions(ctx, gvr, ns, name)
		} else {
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timeout waiting for %s; last: %s", what, last)
		}
		if time.Since(lastLog) >= progressLogInterval {
			logf("still waiting for %s (%s/%s) — %s", what, time.Since(start).Round(time.Second), timeout, last)
			lastLog = time.Now()
		}
		time.Sleep(pollInterval)
	}
}

// listNames returns the names of all objects of a GVR in a namespace ("" = cluster-scoped/all-ns).
func listNames(ctx context.Context, gvr schema.GroupVersionResource, ns string) ([]string, error) {
	var l *unstructured.UnstructuredList
	var err error
	if ns == "" {
		l, err = suiteDyn.Resource(gvr).List(ctx, metav1.ListOptions{})
	} else {
		l, err = suiteDyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(l.Items))
	for i := range l.Items {
		names = append(names, l.Items[i].GetName())
	}
	return names, nil
}

// createPVC creates a ReadWriteOnce PVC of the given size on storageClass and returns nothing;
// callers wait for Bound implicitly by scheduling a pod that mounts it.
func createPVC(ctx context.Context, ns, name, storageClass, size string) {
	GinkgoHelper()
	sc := storageClass
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
	_, err := suiteClientset.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred(), "create PVC %s/%s", ns, name)
	}
}

// createPVCFromSnapshot creates a PVC whose dataSource is a CSI VolumeSnapshot (plain CSI restore).
func createPVCFromSnapshot(ctx context.Context, ns, name, storageClass, vsName, size string) {
	GinkgoHelper()
	sc := storageClass
	apiGroup := "snapshot.storage.k8s.io"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: &apiGroup,
				Kind:     "VolumeSnapshot",
				Name:     vsName,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
	_, err := suiteClientset.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred(), "create restore PVC %s/%s from snapshot %s", ns, name, vsName)
	}
}

// --- svdm HTTP export/import (run from an in-cluster curl pod) --------------
//
// status.url is an in-cluster service URL, so all HTTP calls run from a curl pod inside the
// cluster (not the test runner). The pod runs as a ServiceAccount granted the download RBAC; the
// projected SA token is sent as the Bearer credential, and the CA comes from the CR's status.ca.
// Wire details mirror storage-volume-data-manager docs/FAQ.md "HTTP API".

const (
	httpClientPod = "svdm-http-client"
	httpClientSA  = "transition-http"
)

func httpImage() string {
	if v := strings.TrimSpace(os.Getenv("E2E_TRANSITION_HTTP_IMAGE")); v != "" {
		return v
	}
	return "curlimages/curl:8.11.1"
}

// ensureDownloadRBAC creates the ServiceAccount + ClusterRole + ClusterRoleBinding that authorize
// the "create dataexports/dataimports download" subresource the data-exporter checks via SAR.
func ensureDownloadRBAC(ctx context.Context, ns, sa string) {
	GinkgoHelper()
	_, err := suiteClientset.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: sa, Namespace: ns},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred(), "create SA %s/%s", ns, sa)
	}
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "transition-http-download"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{unifiedGroup, legacyGroup},
			Resources: []string{"dataexports/download", "dataimports/download"},
			Verbs:     []string{"create"},
		}},
	}
	_, err = suiteClientset.RbacV1().ClusterRoles().Create(ctx, cr, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred(), "create ClusterRole")
	}
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "transition-http-download"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: sa, Namespace: ns}},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "transition-http-download"},
	}
	_, err = suiteClientset.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred(), "create ClusterRoleBinding")
	}
}

// createHTTPClientPod runs a long-lived curl pod as the download SA (token auto-mounted).
func createHTTPClientPod(ctx context.Context, ns, name, sa string) {
	GinkgoHelper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			ServiceAccountName: sa,
			RestartPolicy:      corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name:    "curl",
				Image:   httpImage(),
				Command: []string{"sh", "-c", "sleep 360000"},
			}},
		},
	}
	_, err := suiteClientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred(), "create http client pod %s/%s", ns, name)
	}
	Eventually(func() (corev1.PodPhase, error) {
		p, gerr := suiteClientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return "", gerr
		}
		return p.Status.Phase, nil
	}, 5*time.Minute, pollInterval).Should(Equal(corev1.PodRunning), "http client pod Running")
}

// crStatusURLCA reads status.url and status.ca (base64) from a DataExport/DataImport CR.
func crStatusURLCA(ctx context.Context, gvr schema.GroupVersionResource, ns, name string, timeout time.Duration) (url, caB64 string, err error) {
	url, err = waitStatusString(ctx, gvr, ns, name, timeout, "url")
	if err != nil {
		return "", "", err
	}
	obj, err := getUnstr(ctx, gvr, ns, name)
	if err != nil {
		return "", "", err
	}
	caB64, _, _ = unstructured.NestedString(obj.Object, "status", "ca")
	return url, caB64, nil
}

// tokenAndCAPrelude returns the shell prelude that loads the Bearer token and writes ca.pem.
func tokenAndCAPrelude(caB64 string) string {
	return "set -e; T=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token); " +
		"echo " + caB64 + " | base64 -d > /tmp/ca.pem; "
}

// svdmDownload GETs api/v1/files/<remoteFile> from the export URL into localPath inside the client pod.
func svdmDownload(ctx context.Context, ns, url, caB64, remoteFile, localPath string) error {
	script := tokenAndCAPrelude(caB64) +
		"curl -sSf -H \"Authorization: Bearer $T\" --cacert /tmp/ca.pem " +
		"\"" + url + "api/v1/files/" + remoteFile + "\" -o " + localPath
	_, err := execSh(ctx, ns, httpClientPod, "curl", script)
	return err
}

// svdmUpload PUTs localPath to api/v1/files/<remoteFile> on the import URL, then POSTs finished.
func svdmUpload(ctx context.Context, ns, url, caB64, localPath, remoteFile string) error {
	script := tokenAndCAPrelude(caB64) +
		"SZ=$(stat -c%s " + localPath + "); " +
		"curl -sSf -H \"Authorization: Bearer $T\" --cacert /tmp/ca.pem -X PUT " +
		"\"" + url + "api/v1/files/" + remoteFile + "\" " +
		"-H \"Content-Length: $SZ\" -H \"X-Content-Length: $SZ\" " +
		"-H \"X-Attribute-Permissions: 0644\" -H \"X-Attribute-Uid: 0\" -H \"X-Attribute-Gid: 0\" -H \"X-Offset: 0\" " +
		"--data-binary @" + localPath + "; " +
		"curl -sSf -H \"Authorization: Bearer $T\" --cacert /tmp/ca.pem -X POST " +
		"\"" + url + "api/v1/finished\" -H \"Content-Length: 0\""
	_, err := execSh(ctx, ns, httpClientPod, "curl", script)
	return err
}

// createCSIVolumeSnapshot creates a snapshot.storage.k8s.io/v1 VolumeSnapshot from a source PVC.
func createCSIVolumeSnapshot(ctx context.Context, ns, name, vsClass, srcPVC string) error {
	return applyUnstr(ctx, volumeSnapshotGVR, ns, map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec": map[string]interface{}{
			"volumeSnapshotClassName": vsClass,
			"source":                  map[string]interface{}{"persistentVolumeClaimName": srcPVC},
		},
	})
}

// createLegacyDataExport creates a DataExport on the LEGACY storage.deckhouse.io group with the
// pre-D1 schema (targetRef{kind,name}). Used in phase B while svdm is the legacy image.
func createLegacyDataExport(ctx context.Context, ns, name, targetKind, targetName string) error {
	return applyUnstr(ctx, dataExportGVR(legacyGroup), ns, map[string]interface{}{
		"apiVersion": legacyGroup + "/v1alpha1",
		"kind":       "DataExport",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec": map[string]interface{}{
			"ttl":       "30m",
			"publish":   false,
			"targetRef": map[string]interface{}{"kind": targetKind, "name": targetName},
		},
	})
}

// createLegacyDataImport creates a DataImport on the LEGACY group with the pre-D1 schema
// (targetRef{kind: PersistentVolumeClaim, pvcTemplate}). Used in phase B while svdm is legacy.
func createLegacyDataImport(ctx context.Context, ns, name, pvcName, storageClass, size string) error {
	return applyUnstr(ctx, dataImportGVR(legacyGroup), ns, map[string]interface{}{
		"apiVersion": legacyGroup + "/v1alpha1",
		"kind":       "DataImport",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec": map[string]interface{}{
			"ttl":                  "30m",
			"publish":              false,
			"waitForFirstConsumer": false,
			"targetRef": map[string]interface{}{
				"kind": "PersistentVolumeClaim",
				"pvcTemplate": map[string]interface{}{
					"metadata": map[string]interface{}{"name": pvcName},
					"spec": map[string]interface{}{
						"accessModes":      []interface{}{"ReadWriteOnce"},
						"storageClassName": storageClass,
						"volumeMode":       "Filesystem",
						"resources":        map[string]interface{}{"requests": map[string]interface{}{"storage": size}},
					},
				},
			},
		},
	})
}

// pvcsWithFinalizer returns the ns/name of every PVC (all namespaces) still carrying the given
// finalizer — used to assert the migration hook swept the legacy finalizer off PVCs.
func pvcsWithFinalizer(ctx context.Context, finalizer string) ([]string, error) {
	list, err := suiteClientset.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var hits []string
	for i := range list.Items {
		for _, f := range list.Items[i].Finalizers {
			if f == finalizer {
				hits = append(hits, list.Items[i].Namespace+"/"+list.Items[i].Name)
				break
			}
		}
	}
	return hits, nil
}

// workloadResourceCount counts module-owned workload/RBAC objects in a namespace that a Helm guard
// is expected to drop (Deployments, Services, ServiceAccounts except the system default). Used to
// assert guard emptiness after the flip.
func workloadResourceCount(ctx context.Context, ns string) (int, error) {
	if !namespaceExists(ctx, ns) {
		return 0, nil
	}
	deps, err := suiteClientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	svcs, err := suiteClientset.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	return len(deps.Items) + len(svcs.Items), nil
}

// --- ClusterAlert (deprecation) helpers ------------------------------------

// clusterAlertFiring reports whether a ClusterAlert with alert.name == alertName is firing. When
// moduleLabel != "" it additionally requires alert.labels.module to equal it (the built-in
// ModuleIsDeprecated alert carries the module name there; our custom vector(1) alerts carry no
// module label, so pass "").
func clusterAlertFiring(ctx context.Context, alertName, moduleLabel string) (bool, error) {
	list, err := suiteDyn.Resource(clusterAlertGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for i := range list.Items {
		obj := list.Items[i].Object
		if name, _, _ := unstructured.NestedString(obj, "alert", "name"); name != alertName {
			continue
		}
		if st, _, _ := unstructured.NestedString(obj, "status", "alertStatus"); st != "firing" {
			continue
		}
		if moduleLabel == "" {
			return true, nil
		}
		if m, _, _ := unstructured.NestedString(obj, "alert", "labels", "module"); m == moduleLabel {
			return true, nil
		}
	}
	return false, nil
}

// firingAlertNames lists the names of all currently-firing ClusterAlerts (for wait diagnostics).
func firingAlertNames(ctx context.Context) string {
	list, err := suiteDyn.Resource(clusterAlertGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Sprintf("list err=%v", err)
	}
	var names []string
	for i := range list.Items {
		obj := list.Items[i].Object
		if st, _, _ := unstructured.NestedString(obj, "status", "alertStatus"); st != "firing" {
			continue
		}
		n, _, _ := unstructured.NestedString(obj, "alert", "name")
		if m, _, _ := unstructured.NestedString(obj, "alert", "labels", "module"); m != "" {
			n += "{" + m + "}"
		}
		names = append(names, n)
	}
	if len(names) == 0 {
		return "no firing alerts"
	}
	return strings.Join(names, ",")
}

// expectAlertFiring blocks until a ClusterAlert (alertName, optional moduleLabel) is firing, logging
// the currently-firing set each tick. Alert evaluation lags a Prometheus scrape, so callers give it
// a generous timeout.
func expectAlertFiring(ctx context.Context, alertName, moduleLabel string, timeout time.Duration) {
	GinkgoHelper()
	what := "ClusterAlert " + alertName
	if moduleLabel != "" {
		what += "{module=" + moduleLabel + "}"
	}
	pollUntil(ctx, what+" firing", timeout,
		func() bool { ok, _ := clusterAlertFiring(ctx, alertName, moduleLabel); return ok },
		func() string { return "firing now: " + firingAlertNames(ctx) })
}

// --- unified-group (D1/storage-foundation) DataExport/DataImport -----------

// createUnifiedDataExport creates a DataExport on the unified storage-foundation.deckhouse.io group
// (targetRef{kind,name}; group omitted = core, as the migration hook emits for a PVC). Used to drive
// svdm-D1-standalone (before the flip) and storage-foundation (after the flip).
func createUnifiedDataExport(ctx context.Context, ns, name, targetKind, targetName string) error {
	return applyUnstr(ctx, dataExportGVR(unifiedGroup), ns, map[string]interface{}{
		"apiVersion": unifiedGroup + "/v1alpha1",
		"kind":       "DataExport",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec": map[string]interface{}{
			"ttl":       "30m",
			"publish":   false,
			"targetRef": map[string]interface{}{"kind": targetKind, "name": targetName},
		},
	})
}

// createUnifiedDataImport creates a DataImport on the unified group with the D1/sf schema
// (mode: CreatePVC + root pvcTemplate — no targetRef, unlike the legacy pre-D1 shape).
func createUnifiedDataImport(ctx context.Context, ns, name, pvcName, storageClass, size string) error {
	return applyUnstr(ctx, dataImportGVR(unifiedGroup), ns, map[string]interface{}{
		"apiVersion": unifiedGroup + "/v1alpha1",
		"kind":       "DataImport",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec": map[string]interface{}{
			"ttl":                  "30m",
			"publish":              false,
			"waitForFirstConsumer": false,
			"mode":                 "CreatePVC",
			"pvcTemplate": map[string]interface{}{
				"metadata": map[string]interface{}{"name": pvcName},
				"spec": map[string]interface{}{
					"accessModes":      []interface{}{"ReadWriteOnce"},
					"storageClassName": storageClass,
					"volumeMode":       "Filesystem",
					"resources":        map[string]interface{}{"requests": map[string]interface{}{"storage": size}},
				},
			},
		},
	})
}

// deleteCRAndWaitGone deletes a namespaced CR and blocks until it is fully gone (finalizers
// released). Used to tear down DataExports so the reassigned source PV is recovered.
func deleteCRAndWaitGone(ctx context.Context, gvr schema.GroupVersionResource, ns, name string, timeout time.Duration) {
	GinkgoHelper()
	err := suiteDyn.Resource(gvr).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred(), "delete %s %s/%s", gvr.Resource, ns, name)
	}
	pollUntil(ctx, fmt.Sprintf("%s %s/%s deleted", gvr.Resource, ns, name), timeout,
		func() bool {
			_, gerr := suiteDyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
			return apierrors.IsNotFound(gerr)
		},
		func() string {
			obj, gerr := suiteDyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
			if gerr != nil {
				return fmt.Sprintf("get err=%v", gerr)
			}
			return "still present, finalizers=" + fmt.Sprint(obj.GetFinalizers())
		})
}

// waitPVCPhase blocks until a PVC reaches the wanted phase (used to assert the source PVC recovers
// from Lost->Bound after the export that reassigned its PV is deleted).
func waitPVCPhase(ctx context.Context, ns, name string, want corev1.PersistentVolumeClaimPhase, timeout time.Duration) {
	GinkgoHelper()
	pollUntil(ctx, fmt.Sprintf("PVC %s/%s == %s", ns, name, want), timeout,
		func() bool { return pvcPhase(ctx, ns, name) == string(want) },
		func() string { return "phase=" + pvcPhase(ctx, ns, name) })
}
