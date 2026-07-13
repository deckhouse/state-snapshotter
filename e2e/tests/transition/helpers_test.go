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

// --- GVRs ------------------------------------------------------------------

var (
	pvcGVR = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	nsGVR  = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

	volumeSnapshotGVR = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}

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
	Eventually(func() (corev1.PodPhase, error) {
		p, gerr := suiteClientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return "", gerr
		}
		return p.Status.Phase, nil
	}, 5*time.Minute, pollInterval).Should(Equal(corev1.PodRunning), "probe pod %s/%s Running", ns, name)
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
	deadline := time.Now().Add(timeout)
	path := append([]string{"status"}, fields...)
	var last string
	for {
		obj, err := getUnstr(ctx, gvr, ns, name)
		if err == nil {
			if v, found, _ := unstructured.NestedString(obj.Object, path...); found && v != "" {
				return v, nil
			}
			last = "field empty"
		} else {
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timeout waiting for %s %s/%s status.%s; last: %s", gvr.Resource, ns, name, strings.Join(fields, "."), last)
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

// waitCRConditionTrue polls status.conditions until any of condTypes has status "True".
func waitCRConditionTrue(ctx context.Context, gvr schema.GroupVersionResource, ns, name string, condTypes []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	want := map[string]bool{}
	for _, t := range condTypes {
		want[t] = true
	}
	var last string
	for {
		obj, err := getUnstr(ctx, gvr, ns, name)
		if err == nil {
			conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
			for _, c := range conds {
				m, ok := c.(map[string]interface{})
				if !ok {
					continue
				}
				ct, _ := m["type"].(string)
				st, _ := m["status"].(string)
				if want[ct] && st == "True" {
					return nil
				}
				last = fmt.Sprintf("%s=%s", ct, st)
			}
		} else {
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s %s/%s condition in %v; last: %s", gvr.Resource, ns, name, condTypes, last)
		}
		time.Sleep(pollInterval)
	}
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
