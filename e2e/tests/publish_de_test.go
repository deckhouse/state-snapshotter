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
	"net/url"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// --- DataExport publish:true (ingress + tokens) spec (E2E_PUBLISH) ---------------------------------
//
// This spec proves a storage-foundation DataExport with spec.publish=true is reachable BOTH from inside
// the nested cluster (status.url, direct to the exporter pod behind the exporter's own CA) AND from
// outside through the origin kube-API ingress (status.publicURL, TLS terminated at nginx). The source is
// a live Filesystem PVC exported directly (targetRef -> PersistentVolumeClaim, publicURL path
// /<ns>/pvc/<name>/), so the exporter serves the Filesystem data-plane (/api/v1/files/: JSON listing +
// per-file download). Authentication is a SA Bearer token minted via TokenRequest; the negatives assert
// (internal) a token denied BEFORE its RoleBinding -> 403 and (external) client certs through the ingress
// -> 401/403 (nginx terminated TLS, so the cert never reaches the exporter's VerifyClientCertIfGiven).
//
// It reuses publish_helpers_test.go: auth (createServiceAccountIfNotExists / createDataDownloadRole /
// bindRoleToServiceAccount / issueServiceAccountToken), the base-cluster curl runner
// (ensureExternalCurlPod / externalCurlTarget / runCurlRequest / runCurlChecksum), the nested curl
// target (nestedCurlTarget), the CA helpers (caIssuerCommonName / writeCAToPodFile), the URL helpers
// (publishDataURL, publishFilesPath), and publishMasterIP for the external --resolve fallback.

const (
	// publishDEPVC is the source Filesystem PVC the DataExport exports directly (targetRef PVC).
	publishDEPVC = "pub-de-pvc"
	// publishDEExport is the DataExport name.
	publishDEExport = "export-pub-de"
	// publishDEWriterPod writes the source files then is deleted (RWO functional detach: a live-PVC export
	// takes over the PVC's PV, which the writer must have released first).
	publishDEWriterPod = "pub-de-writer"
	// publishDEIntPod is the in-nested curl pod for the INTERNAL (status.url) checks; publishDEIntCont its
	// container. No SA token is mounted — the Bearer token is passed per-exec via argv, like the base runner.
	publishDEIntPod  = "pub-de-int"
	publishDEIntCont = "curl"

	// publishDESA / publishDERole / publishDEBinding are the token identity granted dataexports/download in
	// the source namespace. Role and RoleBinding are split so the 403-before-binding negative can probe the
	// token while only the SA + Role exist, then bind and re-probe for 200.
	publishDESA      = publishClientSA // "publish-client" (from publish_helpers)
	publishDERole    = "publish-de-download"
	publishDEBinding = "publish-de-download"

	// publishDETTL is a generous idle-TTL: comfortably longer than this spec so the exporter never
	// idle-expires mid-run (the exporter resets its IdleTimer on each request).
	publishDETTL = "60m"

	// publishDEBigMiB is the big random file size (MiB); publishDEBigBytes its byte count.
	publishDEBigMiB   = 200
	publishDEBigBytes = publishDEBigMiB * 1024 * 1024

	// publishDECertPodFile / publishDEKeyPodFile are the in-pod paths the client-cert negative
	// materializes the (nested) admin client certificate/key to for `curl --cert/--key`.
	publishDECertPodFile = "/tmp/publish-client.crt"
	publishDEKeyPodFile  = "/tmp/publish-client.key"
)

// Source file layout on the Filesystem PVC (mounted at /mnt/<pvc>): a file in the root, a file in a
// subdir, a file in a deep subdir2/subdir/subsubdir, and a big random file in the root. The three text
// files carry known content for byte-exact external comparison; all four are sha256'd at the source.
const (
	publishDERootFile = "root.txt"
	publishDESubFile  = "subdir/nested.txt"
	publishDEDeepFile = "subdir2/subdir/subsubdir/deep.txt"
	publishDEBigFile  = "big.bin"
)

// publishDataExportSpecs registers the DataExport publish:true flow (env-gated by E2E_PUBLISH). Ordered:
// the source + DataExport are built once, then internal and external checks (which share the SA/token
// RBAC granted in the internal step) run in sequence, then teardown asserts the ingress resources are
// reaped. Registered after the volume-data phases in the single root Ordered container.
func publishDataExportSpecs() {
	Context("Publish 1: DataExport publish:true (ingress + tokens)", Ordered, func() {
		var (
			srcNS    string
			srcSums  map[string]string // relative path -> source sha256
			srcText  map[string]string // relative path -> exact source content (small text files)
			token    string
			caPEM    string
			intURL   string // status.url  (internal, direct to exporter pod)
			pubURL   string // status.publicURL (external, through the origin ingress host)
			masterIP string
			ingName  string
			svcName  string
			intBig   string // internal big-file sha256 (also cross-checked against the external stream)
		)

		BeforeAll(func() {
			if !suiteCfg.publish {
				Skip("E2E_PUBLISH=false: skipping the DataExport publish (ingress + tokens) flow (it runs by default)")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
			defer cancel()

			srcNS = uniqueNS("pub-de")
			srcSums = map[string]string{}
			srcText = map[string]string{
				publishDERootFile: "publish-de root file " + srcNS,
				publishDESubFile:  "publish-de subdir file " + srcNS,
				publishDEDeepFile: "publish-de deep subsubdir file " + srcNS,
			}
			masterIP = publishMasterIP(ctx)

			By("Provisioning a thin, snapshot-capable default StorageClass via storage-e2e (" + suiteCfg.storageClass + ")")
			// Idempotent: phases 3-5 provision the same SC under E2E_VOLUME_DATA, but the publish gate is
			// independent, so provision it here too rather than depend on another phase having run.
			_, err := testkit.EnsureDefaultStorageClass(ctx, suiteRestCfg, testkit.DefaultStorageClassConfig{
				StorageClassName:     suiteCfg.storageClass,
				LVMType:              "Thin",
				ThinPoolName:         "thinpool",
				BaseKubeconfig:       suiteClusterResources.BaseKubeconfig,
				VMNamespace:          suiteCfg.vmNamespace,
				BaseStorageClassName: suiteCfg.baseStorageClass,
			})
			Expect(err).NotTo(HaveOccurred(), "provision default StorageClass")

			By("Creating the source namespace " + srcNS)
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				// deleteNamespace honors E2E_KEEP_CLUSTER*; deleting the namespace deletes the DataExport in
				// it, so the controller reaps the ingress/Service in d8-storage-foundation as a side effect.
				deleteNamespace(cctx, srcNS)
			})

			By("Ensuring the base-cluster external curl runner pod")
			Expect(ensureExternalCurlPod(ctx)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer ccancel()
				deleteExternalCurlPod(cctx)
			})

			By("Creating the source Filesystem PVC " + publishDEPVC)
			Expect(createFilesystemPVC(ctx, srcNS, publishDEPVC, suiteCfg.storageClass, "1Gi")).To(Succeed())

			By("Writing the source files (root / subdir / deep subsubdir + a 200Mi random file) and recording checksums")
			// The probe pod is the first consumer that binds the WaitForFirstConsumer PVC; waitPodRunning
			// blocks until the bind completes.
			_, err = suiteClientset.CoreV1().Pods(srcNS).Create(ctx, probePodSpec(srcNS, publishDEWriterPod, []string{publishDEPVC}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create source writer pod")
			Expect(waitPodRunning(ctx, srcNS, publishDEWriterPod, 10*time.Minute)).To(Succeed())
			sums, werr := writeFsSourceAndChecksums(ctx, srcNS, publishDEWriterPod, publishDEPVC, srcText)
			Expect(werr).NotTo(HaveOccurred(), "write source files + record checksums")
			srcSums = sums
			for _, f := range []string{publishDERootFile, publishDESubFile, publishDEDeepFile, publishDEBigFile} {
				Expect(srcSums[f]).To(HaveLen(64), "source sha256 recorded for %s", f)
			}

			// The DataExport is created in (b) but consumed by (c)/(d)/(e), so its cleanup MUST be
			// container-scoped (AfterAll), not spec-scoped: a DeferCleanup registered inside (b) would run as
			// an AfterEach right after (b) and delete the exporter before the internal/external checks. Register
			// it here in BeforeAll so it fires once at container teardown. deleteDataExport is idempotent, so
			// this is a harmless no-op after (e) deletes it explicitly; it also honors E2E_KEEP_CLUSTER*.
			DeferCleanup(func() {
				if cleanupSkipped() {
					GinkgoWriter.Printf("%s: keeping DataExport %s/%s\n", keepReason(), srcNS, publishDEExport)
					return
				}
				cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer ccancel()
				deleteDataExport(cctx, srcNS, publishDEExport)
			})

			By("Deleting the source writer pod (RWO functional detach before the live-PVC export takes over the PV)")
			// TODO(e2e-publish): verify on cluster — a live-PVC (targetRef PersistentVolumeClaim) DataExport
			// detaches the user PVC and rebinds its PV into the export PVC (data-export ensureExportPVReady),
			// so the writer must have released the RWO volume first. Confirm the exporter does NOT instead
			// require the source PVC to remain bound/co-scheduled; if it does, keep the writer pod alive and
			// co-schedule the exporter instead.
			forceDeletePod(ctx, srcNS, publishDEWriterPod)
			Expect(waitPodDeleted(ctx, srcNS, publishDEWriterPod, 3*time.Minute)).To(Succeed())
		})

		It("(b) publishes the DataExport: Ready + status.url + status.publicURL + valid CA + Ingress/Service", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.dataTransferTO+10*time.Minute)
			defer cancel()

			By("Creating DataExport " + publishDEExport + " (targetRef live PVC " + publishDEPVC + ", publish:true)")
			// NB: the DataExport is consumed by (c)/(d)/(e); its cleanup is registered container-scoped in
			// BeforeAll (AfterAll), NOT here — a spec-scoped DeferCleanup would delete the exporter right after
			// (b) and break the internal/external checks.
			Expect(createPublishDataExportPVC(ctx, srcNS, publishDEExport, publishDEPVC, publishDETTL)).To(Succeed())

			By("Waiting for the DataExport to become Ready with status.url + status.publicURL")
			u, p, ca, werr := waitDataExportPublished(ctx, srcNS, publishDEExport, suiteCfg.dataTransferTO)
			Expect(werr).NotTo(HaveOccurred(), "DataExport %s Ready + published", publishDEExport)
			intURL, pubURL, caPEM = u, p, ca
			GinkgoWriter.Printf("  DataExport %s status.url=%s status.publicURL=%s\n", publishDEExport, intURL, pubURL)

			By("Asserting status.publicURL matches https://<originIngressHost>/<ns>/pvc/<pvc>/")
			// TODO(e2e-publish): verify on cluster — confirm the exact publicURL path shape is
			// /<ns>/pvc/<pvc>/ (data-export makePublishConfigs Path = /<ns>/<targetKindShort>/<targetName>,
			// KindPVCShort="pvc"; EnsureIngressResource appends the trailing slash).
			wantPrefix := fmt.Sprintf("https://%s/%s/pvc/%s/", suitePublishInfra.originIngressHost, srcNS, publishDEPVC)
			Expect(pubURL).To(Equal(wantPrefix), "status.publicURL")
			Expect(intURL).NotTo(BeEmpty(), "status.url (internal)")

			By("Asserting status.ca is a valid PEM chain issued by data-exporter-CA")
			issuer, cerr := caIssuerCommonName(caPEM)
			Expect(cerr).NotTo(HaveOccurred(), "parse status.ca")
			Expect(issuer).To(ContainSubstring("data-exporter-CA"), "status.ca issuer CommonName")

			By("Asserting the publish Ingress + backing Service were created in " + d8DataManagerNS)
			name, svc, found, ferr := findPublishIngress(ctx, pubURL)
			Expect(ferr).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), "a publish Ingress for %s must exist in %s", pubURL, d8DataManagerNS)
			ingName, svcName = name, svc
			_, gerr := suiteClientset.CoreV1().Services(d8DataManagerNS).Get(ctx, svcName, metav1.GetOptions{})
			Expect(gerr).NotTo(HaveOccurred(), "publish Service %s/%s must exist", d8DataManagerNS, svcName)
		})

		It("(c) INTERNAL (status.url): 403 before the RoleBinding, then lists + downloads with source checksums", func() {
			Expect(intURL).NotTo(BeEmpty(), "the publish spec must have populated status.url")

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()

			By("Creating the in-nested curl pod for the internal (status.url) checks")
			_, err := suiteClientset.CoreV1().Pods(srcNS).Create(ctx, nestedCurlPodSpec(srcNS, publishDEIntPod), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create nested curl pod")
			Expect(waitPodRunning(ctx, srcNS, publishDEIntPod, 5*time.Minute)).To(Succeed())
			intTarget := nestedCurlTarget(srcNS, publishDEIntPod, publishDEIntCont)

			By("Writing status.ca into the pod so internal curl validates the exporter cert with --cacert")
			// The exporter serving cert carries the pod IP (and headless-service DNS) as SANs (data-exporter
			// certutil GenerateTLSBundle), so --cacert validates status.url without -k.
			caFile, caerr := writeCAToPodFile(ctx, intTarget, caPEM)
			Expect(caerr).NotTo(HaveOccurred())

			By("Creating the token identity (SA + dataexports/download Role) WITHOUT the RoleBinding yet")
			Expect(createServiceAccountIfNotExists(ctx, srcNS, publishDESA)).To(Succeed())
			Expect(createDataDownloadRole(ctx, srcNS, publishDERole, dataExportGVR.Resource)).To(Succeed())
			t, terr := issueServiceAccountToken(ctx, srcNS, publishDESA, publishTokenTTL)
			Expect(terr).NotTo(HaveOccurred())
			token = t

			By("NEGATIVE: the token is denied (403) before its RoleBinding exists")
			listURL := publishDataURL(intURL, publishFilesPath)
			code, _, cerr := runCurlRequest(ctx, intTarget, listURL, curlRequest{token: token, caFile: caFile})
			Expect(cerr).NotTo(HaveOccurred())
			Expect(code).To(Equal(403), "listing without a RoleBinding must be Forbidden")

			By("Granting the RoleBinding, then listing /api/v1/files/ (200 + expected paths)")
			Expect(bindRoleToServiceAccount(ctx, srcNS, publishDEBinding, publishDERole, srcNS, publishDESA)).To(Succeed())
			// The data-exporter authorizes each request via a fresh SubjectAccessReview, so the SAME token now
			// passes without re-issuing it.
			var body string
			Eventually(func() (int, error) {
				c, b, e := runCurlRequest(ctx, intTarget, listURL, curlRequest{token: token, caFile: caFile})
				body = b
				return c, e
			}).WithContext(ctx).WithTimeout(2*time.Minute).WithPolling(pollInterval).Should(Equal(200),
				"listing must succeed once the RoleBinding is in place (SAR is eventually consistent)")
			// TODO(e2e-publish): verify on cluster — the Filesystem listing is per-directory JSON
			// {"apiVersion":"v1","items":[{"name","type","uri",...}]} (data-exporter export_filesystem), so
			// the root listing shows direct children only; confirm the exact JSON shape if stricter parsing
			// is wanted (this asserts on substrings, which is robust to key ordering).
			Expect(body).To(ContainSubstring(publishDERootFile), "root listing contains the root file")
			Expect(body).To(ContainSubstring("subdir"), "root listing contains the subdir directory")
			Expect(body).To(ContainSubstring("subdir2"), "root listing contains the deep subdir root")
			Expect(body).To(ContainSubstring(publishDEBigFile), "root listing contains the big file")

			By("Downloading a file from a subdirectory and matching the source checksum")
			subSum, serr := runCurlChecksum(ctx, intTarget, publishDataURL(intURL, publishFilesPath+publishDESubFile), curlRequest{token: token, caFile: caFile})
			Expect(serr).NotTo(HaveOccurred())
			Expect(subSum).To(Equal(srcSums[publishDESubFile]), "subdir file sha256 == source")

			By("Downloading the big file (streamed to sha256sum in-pod) and matching the source checksum")
			bigSum, berr := runCurlChecksum(ctx, intTarget, publishDataURL(intURL, publishFilesPath+publishDEBigFile), curlRequest{token: token, caFile: caFile})
			Expect(berr).NotTo(HaveOccurred())
			Expect(bigSum).To(Equal(srcSums[publishDEBigFile]), "big file sha256 == source")
			intBig = bigSum
		})

		It("(d) EXTERNAL (status.publicURL): Bearer content validation from the base cluster; client-cert negative", func() {
			Expect(pubURL).NotTo(BeEmpty(), "the publish spec must have populated status.publicURL")
			Expect(token).NotTo(BeEmpty(), "the internal spec must have minted + bound the token")

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()

			extTarget, terr := externalCurlTarget()
			Expect(terr).NotTo(HaveOccurred())

			By("Listing /api/v1/files/ through the ingress (Bearer) and asserting the expected paths")
			// External TLS is terminated at nginx with the origin (self-signed) cert, NOT the exporter CA, so
			// use -k (caFile empty). masterIP enables the sslip.io --resolve fallback if the base pod's DNS
			// cannot resolve api.<domain>.
			rootList, code, lerr := runCurlRequestExpect(ctx, extTarget, publishDataURL(pubURL, publishFilesPath), curlRequest{token: token, masterIP: masterIP})
			Expect(lerr).NotTo(HaveOccurred())
			Expect(code).To(Equal(200), "external listing must succeed with the Bearer token")
			Expect(rootList).To(ContainSubstring("subdir"), "external root listing contains subdir")
			Expect(rootList).To(ContainSubstring("subdir2"), "external root listing contains subdir2")
			Expect(rootList).To(ContainSubstring(publishDERootFile), "external root listing contains the root file")

			By("Listing the deep subsubdir through the ingress and asserting it contains the deep file")
			deepDirURL := publishDataURL(pubURL, publishFilesPath+"subdir2/subdir/subsubdir/")
			deepList, dcode, derr := runCurlRequestExpect(ctx, extTarget, deepDirURL, curlRequest{token: token, masterIP: masterIP})
			Expect(derr).NotTo(HaveOccurred())
			Expect(dcode).To(Equal(200), "external deep-dir listing must succeed")
			Expect(deepList).To(ContainSubstring("deep.txt"), "deep-dir listing contains the subsubdir file")

			By("Downloading the small text files through the ingress and comparing them byte-for-byte to source")
			for _, rel := range []string{publishDERootFile, publishDESubFile, publishDEDeepFile} {
				got, gcode, gerr := runCurlRequestExpect(ctx, extTarget, publishDataURL(pubURL, publishFilesPath+rel), curlRequest{token: token, masterIP: masterIP})
				Expect(gerr).NotTo(HaveOccurred())
				Expect(gcode).To(Equal(200), "external download of %s must succeed", rel)
				Expect(got).To(Equal(srcText[rel]), "external content of %s must match source byte-for-byte", rel)
			}

			By("Streaming the big file through the ingress to sha256sum and matching internal + source")
			extBig, berr := runCurlChecksum(ctx, extTarget, publishDataURL(pubURL, publishFilesPath+publishDEBigFile), curlRequest{token: token, masterIP: masterIP})
			Expect(berr).NotTo(HaveOccurred())
			Expect(extBig).To(Equal(srcSums[publishDEBigFile]), "external big-file sha256 == source")
			Expect(extBig).To(Equal(intBig), "external big-file sha256 == internal big-file sha256")

			By("NEGATIVE: client certs through the ingress do not authenticate (TLS terminated at nginx -> 401/403)")
			certPEM, keyPEM := adminClientCertKey()
			if len(certPEM) == 0 || len(keyPEM) == 0 {
				// TODO(e2e-publish): verify on cluster — the nested kubeconfig should embed the admin client
				// cert/key (CertData/KeyData or CertFile/KeyFile). If it uses token/exec auth instead, this
				// negative cannot present a client cert; skip rather than fail.
				Skip("nested kubeconfig has no admin client certificate/key; skipping the client-cert negative")
			}
			ccCode, cerr := runExternalClientCertProbe(ctx, extTarget, publishDataURL(pubURL, publishFilesPath), certPEM, keyPEM, masterIP)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(ccCode).To(BeElementOf(401, 403), "client certs through the ingress must NOT authenticate (got %d)", ccCode)
		})

		It("(e) teardown: spec.targetRef is immutable, and deleting the DataExport reaps the Ingress + Service", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()

			By("Asserting the DataExport export identity is immutable (spec.targetRef cannot change)")
			// Only the export identity spec.targetRef is locked after creation, via a CEL transition rule
			// (crds/dataexports.yaml: spec x-kubernetes-validations pins targetRef.kind/name/group, with the
			// optional group normalized so the controller's omitempty round-trip is not seen as a change);
			// ttl/publish/publicIngress remain mutable (DataExport mirrors DataImport, which locks only
			// spec.mode + spec.snapshotRef). Patching targetRef.name must be rejected by admission; probe by
			// attempting the mutation rather than reading the CRD (Deckhouse strips vendor schema extensions).
			patch := []byte(`{"spec":{"targetRef":{"name":"immutability-probe"}}}`)
			_, perr := suiteDyn.Resource(dataExportGVR).Namespace(srcNS).Patch(ctx, publishDEExport, types.MergePatchType, patch, metav1.PatchOptions{})
			Expect(perr).To(HaveOccurred(), "changing spec.targetRef on a DataExport must be rejected")
			// Prove the rejection is immutability admission, not an incidental NotFound/Conflict: the object
			// must still exist here (finding-1 fix keeps the DE alive through (e)), and the patch must be
			// rejected as Invalid/BadRequest.
			Expect(apierrors.IsNotFound(perr)).To(BeFalse(), "the DataExport must still exist when the immutability patch is attempted; got: %v", perr)
			Expect(apierrors.IsInvalid(perr) || apierrors.IsBadRequest(perr)).To(BeTrue(), "the targetRef patch must be rejected as Invalid/BadRequest by immutability admission; got: %v", perr)

			By("Deleting the DataExport and asserting the Ingress + Service are reaped in " + d8DataManagerNS)
			GinkgoWriter.Printf("  publish Ingress %s/%s (Service %s) expected to be reaped after DataExport delete\n", d8DataManagerNS, ingName, svcName)
			deleteDataExport(ctx, srcNS, publishDEExport)
			Eventually(func() (bool, error) {
				_, _, found, ferr := findPublishIngress(ctx, pubURL)
				return found, ferr
			}).WithContext(ctx).WithTimeout(5*time.Minute).WithPolling(pollInterval).Should(BeFalse(),
				"the publish Ingress must be removed after the DataExport is deleted")
			Eventually(func() bool {
				_, gerr := suiteClientset.CoreV1().Services(d8DataManagerNS).Get(ctx, svcName, metav1.GetOptions{})
				return apierrors.IsNotFound(gerr)
			}).WithContext(ctx).WithTimeout(5*time.Minute).WithPolling(pollInterval).Should(BeTrue(),
				"the publish Service must be removed after the DataExport is deleted")
		})
	})
}

// --- spec-local helpers ----------------------------------------------------

// createFilesystemPVC creates a Filesystem-mode RWO PVC on the given StorageClass. The default e2e
// StorageClass (sds-local-volume) is WaitForFirstConsumer, so the PVC binds only once a consumer Pod is
// scheduled (the writer probe pod).
func createFilesystemPVC(ctx context.Context, ns, name, sc, size string) error {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			VolumeMode:       func() *corev1.PersistentVolumeMode { m := corev1.PersistentVolumeFilesystem; return &m }(),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
	if _, err := suiteClientset.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create Filesystem PVC %s/%s: %w", ns, name, err)
	}
	return nil
}

// nestedCurlPodSpec is the in-nested curl runner for the internal (status.url) checks: a long-sleeping
// curlimages/curl container with SA-token automount DISABLED, so the only credential a request carries is
// the Bearer token passed per-exec (never the pod's own identity), matching the external runner.
func nestedCurlPodSpec(ns, name string) *corev1.Pod {
	automount := false
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyAlways,
			AutomountServiceAccountToken: &automount,
			Containers: []corev1.Container{{
				Name:    publishDEIntCont,
				Image:   suiteCfg.backupClientImage,
				Command: []string{"sh", "-c", "sleep 360000"},
			}},
		},
	}
}

// writeFsSourceAndChecksums writes the source file tree on the mounted PVC (root file, subdir file, deep
// subsubdir file with the given known text contents, plus a big random file) and returns each file's
// sha256 (relative path -> hex digest), read back with sha256sum in the same pod. Contents are passed as
// exec argv values (never interpolated into the shell) and written with `printf '%s'` (no trailing
// newline) so the external byte-exact comparison matches.
func writeFsSourceAndChecksums(ctx context.Context, ns, pod, pvc string, text map[string]string) (map[string]string, error) {
	mount := "/mnt/" + pvc
	// $0=sh placeholder, $1=mount, $2=root content, $3=subdir content, $4=deep content.
	script := `set -e; cd "$1"; ` +
		`mkdir -p subdir subdir2/subdir/subsubdir; ` +
		`printf '%s' "$2" > ` + publishDERootFile + `; ` +
		`printf '%s' "$3" > ` + publishDESubFile + `; ` +
		`printf '%s' "$4" > ` + publishDEDeepFile + `; ` +
		fmt.Sprintf(`dd if=/dev/urandom of=%s bs=1M count=%d 2>/dev/null; `, publishDEBigFile, publishDEBigMiB) +
		`sync; ` +
		`sha256sum ` + publishDERootFile + ` ` + publishDESubFile + ` ` + publishDEDeepFile + ` ` + publishDEBigFile
	stdout, stderr, err := storagekube.ExecInPod(ctx, suiteRestCfg, ns, pod, vdProbeContainer, []string{
		"sh", "-c", script, "sh", mount, text[publishDERootFile], text[publishDESubFile], text[publishDEDeepFile],
	})
	if err != nil {
		return nil, fmt.Errorf("write source files in %s/%s: %w (stderr=%q)", ns, pod, err, stderr)
	}
	sums := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sums[fields[len(fields)-1]] = fields[0]
	}
	return sums, nil
}

// createPublishDataExportPVC creates a DataExport with publish:true targeting a live PersistentVolumeClaim
// (core group). The exporter serves the PVC's Filesystem data-plane; publicURL path is /<ns>/pvc/<name>/.
func createPublishDataExportPVC(ctx context.Context, ns, name, pvcName, ttl string) error {
	de := &unstructured.Unstructured{Object: map[string]interface{}{
		// DataExport is served by storage-foundation: derive apiVersion from dataExportGVR so the body
		// matches the resource endpoint.
		"apiVersion": dataExportGVR.GroupVersion().String(),
		"kind":       "DataExport",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"ttl":     ttl,
			"publish": true,
			"targetRef": map[string]interface{}{
				// Live PVC target: core group ("") + PersistentVolumeClaim (snapshot_resolver categoryLivePVC).
				"group": "",
				"kind":  "PersistentVolumeClaim",
				"name":  pvcName,
			},
		},
	}}
	_, err := suiteDyn.Resource(dataExportGVR).Namespace(ns).Create(ctx, de, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// waitDataExportPublished polls a DataExport until it is Ready with BOTH status.url (internal) and
// status.publicURL (external) populated and status.phase=Ready, and returns (url, publicURL, ca). It is
// the publish counterpart of waitDataExportReady (which does not wait for the publish resources).
func waitDataExportPublished(ctx context.Context, ns, name string, timeout time.Duration) (url, publicURL, ca string, err error) {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		obj, gerr := getResource(ctx, dataExportGVR, ns, name)
		if gerr == nil {
			st, reason, found := conditionStatus(obj, "Ready")
			if found && st == "True" {
				url, _, _ = unstructured.NestedString(obj.Object, "status", "url")
				publicURL, _, _ = unstructured.NestedString(obj.Object, "status", "publicURL")
				rawCA, _, _ := unstructured.NestedString(obj.Object, "status", "ca")
				ca = normalizePublishCA(rawCA) // status.ca is base64-encoded PEM per the CRD; decode for pem.Decode / --cacert
				phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
				catalogOK, extra := conditionsWithinCatalog(obj, "Ready")
				if url != "" && publicURL != "" && phase == "Ready" && catalogOK {
					return url, publicURL, ca, nil
				}
				last = fmt.Sprintf("Ready=True but url=%q publicURL=%q phase=%q extraConditions=%v", url, publicURL, phase, extra)
			} else {
				last = fmt.Sprintf("Ready=%v reason=%q", st, reason)
			}
		} else {
			last = fmt.Sprintf("get err=%v", gerr)
		}
		if time.Now().After(deadline) {
			return "", "", "", fmt.Errorf("timeout waiting for DataExport %s/%s Ready+published; last: %s", ns, name, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return "", "", "", ctx.Err()
		}
	}
}

// findPublishIngress locates the publish Ingress in the data-manager namespace that serves publicURL,
// matching by the Ingress rule host + path (both derived from publicURL) rather than the controller's
// hash-generated resource name. Returns the Ingress name, its backing Service name, and whether it was
// found. The Ingress/Service live in d8-storage-foundation (the data-manager ControllerNamespace), not
// the DataExport's namespace.
func findPublishIngress(ctx context.Context, publicURL string) (ingressName, serviceName string, found bool, err error) {
	u, perr := url.Parse(publicURL)
	if perr != nil {
		return "", "", false, fmt.Errorf("parse publicURL %q: %w", publicURL, perr)
	}
	wantHost := u.Hostname()
	wantPath := strings.TrimRight(u.Path, "/") // makePublishConfigs Path has no trailing slash
	list, lerr := suiteClientset.NetworkingV1().Ingresses(d8DataManagerNS).List(ctx, metav1.ListOptions{})
	if lerr != nil {
		return "", "", false, fmt.Errorf("list Ingresses in %s: %w", d8DataManagerNS, lerr)
	}
	for i := range list.Items {
		ing := &list.Items[i]
		for _, rule := range ing.Spec.Rules {
			if rule.Host != wantHost || rule.HTTP == nil {
				continue
			}
			for _, p := range rule.HTTP.Paths {
				if strings.TrimRight(p.Path, "/") != wantPath {
					continue
				}
				svc := ""
				if p.Backend.Service != nil {
					svc = p.Backend.Service.Name
				}
				return ing.GetName(), svc, true, nil
			}
		}
	}
	return "", "", false, nil
}

// runCurlRequestExpect is runCurlRequest with the arguments reordered to return (body, code, err), so a
// caller that asserts on the body reads it first. Thin wrapper — the transport/fallback logic lives in
// runCurlRequest (publish_helpers).
func runCurlRequestExpect(ctx context.Context, t curlPodTarget, rawURL string, r curlRequest) (body string, code int, err error) {
	code, body, err = runCurlRequest(ctx, t, rawURL, r)
	return body, code, err
}

// adminClientCertKey returns the nested cluster's admin client certificate and key (PEM), preferring the
// embedded CertData/KeyData and falling back to CertFile/KeyFile on disk. Returns empty slices when the
// kubeconfig authenticates by token/exec instead of a client cert.
func adminClientCertKey() (certPEM, keyPEM []byte) {
	tlsCfg := suiteRestCfg.TLSClientConfig
	certPEM, keyPEM = tlsCfg.CertData, tlsCfg.KeyData
	if len(certPEM) == 0 && tlsCfg.CertFile != "" {
		certPEM, _ = os.ReadFile(tlsCfg.CertFile)
	}
	if len(keyPEM) == 0 && tlsCfg.KeyFile != "" {
		keyPEM, _ = os.ReadFile(tlsCfg.KeyFile)
	}
	return certPEM, keyPEM
}

// runExternalClientCertProbe materializes a client certificate/key into the base-cluster runner pod and
// curls rawURL with `--cert/--key` and NO Bearer token, returning the HTTP status code. It proves client
// certs do not authenticate through the ingress (nginx terminates TLS, so the cert never reaches the
// exporter). Mirrors runCurlOnce's direct-then-`--resolve` fallback for the sslip.io host.
func runExternalClientCertProbe(ctx context.Context, t curlPodTarget, rawURL string, certPEM, keyPEM []byte, masterIP string) (int, error) {
	if t.local {
		// In-process client-cert probe from the test runner: present the client cert on the TLS handshake
		// with NO Bearer token and assert the ingress does not authenticate it (see
		// publish_external_http_test.go). The host->IP override folds in the masterIP `--resolve` role.
		return localClientCertProbe(ctx, rawURL, certPEM, keyPEM)
	}
	if _, err := writePodFile(ctx, t, publishDECertPodFile, string(certPEM)); err != nil {
		return 0, err
	}
	if _, err := writePodFile(ctx, t, publishDEKeyPodFile, string(keyPEM)); err != nil {
		return 0, err
	}
	code, err := clientCertProbeOnce(ctx, t, rawURL, "")
	if err != nil {
		return 0, err
	}
	if code == 0 && masterIP != "" {
		if host := hostFromURL(rawURL); host != "" {
			code, err = clientCertProbeOnce(ctx, t, rawURL, fmt.Sprintf("%s:443:%s", host, masterIP))
			if err != nil {
				return 0, err
			}
		}
	}
	if code == 0 {
		return 0, fmt.Errorf("client-cert probe to %s produced no HTTP response (code 000)", rawURL)
	}
	return code, nil
}

func clientCertProbeOnce(ctx context.Context, t curlPodTarget, rawURL, resolveArg string) (int, error) {
	var b strings.Builder
	b.WriteString("curl -sS -k --cert " + shQuote(publishDECertPodFile) + " --key " + shQuote(publishDEKeyPodFile))
	if resolveArg != "" {
		b.WriteString(" --resolve " + shQuote(resolveArg))
	}
	b.WriteString(` -o /dev/null -w 'HTTPCODE:%{http_code}\n' "$1"`)
	// $0=sh placeholder, $1=url. No token is passed.
	stdout, stderr, err := t.exec(ctx, []string{"sh", "-c", b.String(), "sh", rawURL})
	if err != nil {
		return 0, fmt.Errorf("exec client-cert curl in %s/%s: %w (stderr=%q)", t.ns, t.pod, err, stderr)
	}
	code, _ := parseHTTPCodeBody(stdout)
	return code, nil
}

// writePodFile materializes content to an arbitrary in-pod path via an exec argv value (never persisted in
// a resource). Generalizes writeCAToPodFile (which targets the fixed CA path) for the client-cert probe.
func writePodFile(ctx context.Context, t curlPodTarget, path, content string) (string, error) {
	script := `printf '%s' "$1" > ` + shQuote(path)
	_, stderr, err := t.exec(ctx, []string{"sh", "-c", script, "sh", content})
	if err != nil {
		return "", fmt.Errorf("write %s into %s/%s: %w (stderr=%q)", path, t.ns, t.pod, err, stderr)
	}
	return path, nil
}
