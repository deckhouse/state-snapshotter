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
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// --- DataImport publish:true (external ingress upload) spec (E2E_PUBLISH) --------------------------
//
// This spec proves a storage-foundation DataImport with spec.publish=true can be fed its volume bytes
// from OUTSIDE the nested cluster, through the origin kube-API ingress (status.publicURL), and that the
// imported tree restores faithfully. It is the upload counterpart of publish_de_test.go and follows the
// phase-5 import shape (backup_restore_test.go): a manifest-upload + PUT /api/v1/block + POST
// /api/v1/finished round-trip — BUT the block bytes are pushed from a curl pod on the base cluster
// against status.publicURL (Bearer), instead of from an in-cluster pod against status.url.
//
// Flow:
//   (a) source: capture a real orphan Block PVC as a VolumeSnapshot leaf in a source namespace, so its
//       captured PVC manifest can be re-uploaded on import and field-compared to the live source on
//       restore (phase-4-like prep, self-contained: no dependency on E2E_VOLUME_DATA / the phase-4
//       backup fixture). The imported bytes themselves are a random file generated in the base curl pod
//       (its sha256 is the data source of truth) — the source snapshot supplies structure, the external
//       file supplies data.
//   (b) DataImport publish:true referencing an import-mode VolumeSnapshot leaf; upload manifests
//       in-cluster; wait Ready + status.url + status.publicURL + Ingress.
//   (c) EXTERNAL upload: PUT the block file + POST /finished through publicURL (Bearer) from the base
//       cluster; NEGATIVE: the same request without a token -> 401/403.
//   (d) terminal state per the CURRENT catalog (gc/status-model plan): phase=Completed +
//       completionTimestamp, and status.data.artifactRef points at a real VolumeSnapshotContent.
//   (e) two-layer validation (phase-5 style): (manifests) restore output field-compared to the live
//       source PVC via assertManifestsMatchLive; (data) restore into a PVC + probe pod -> sha256 == the
//       external source file.
//   (f) teardown: deleting the DataImport reaps the DI server infrastructure (deployment/service/ingress);
//       sf does not tear it down on the terminal Completed phase (only on idle-TTL expiry or deletion).
//
// It reuses publish_helpers_test.go (auth, base-cluster curl runner, CA helpers, URL helpers), the
// phase-4/5 helpers (block writer, import-tree materialize/upload, restore probe + checksum, the
// manifest comparator, the DataImport Completed waiter with its post-rename status.data.artifactRef +
// phase/completionTimestamp assertions), and publishMasterIP for the external --resolve fallback.

const (
	// publishDISrcPVC is the source orphan Block PVC captured into a VolumeSnapshot leaf (structure only).
	publishDISrcPVC    = "pub-di-src-pvc"
	publishDISrcWriter = "pub-di-src-writer"
	publishDISrcSize   = "500Mi"

	// publishDIImportRoot / publishDIVSLeaf are the import-mode root Snapshot and its VolumeSnapshot data
	// leaf. The DataImport is named after the leaf (phase-5 convention), so the publicURL path is
	// /<importNS>/pvc/<publishDIVSLeaf>/ (DI names generate TargetKindShort="pvc", TargetName=DataImport
	// name — storage-foundation common/names.go NewNames(KindPVC, dataImport.Name, ...)).
	publishDIImportRoot = "pub-di-import-root"
	publishDIVSLeaf     = "pub-di-vs-leaf"

	// publishDISA / publishDIRole / publishDIBinding are the token identity granted dataimports/download in
	// the import namespace (the data-importer authorizes uploads via a SAR on the SAME "download"
	// subresource as DataExport — see createDataDownloadRole).
	publishDISA      = publishClientSA // "publish-client" (from publish_helpers)
	publishDIRole    = "publish-di-upload"
	publishDIBinding = "publish-di-upload"

	// publishDITTL is a generous idle-TTL: comfortably longer than this spec so the importer never
	// idle-expires between Ready and the external upload.
	publishDITTL = "60m"

	// publishDISrcFile is the in-pod path (base curl pod) of the random block payload PUT through the
	// ingress. It reuses bkBlockMiB/bkBlockBytes so the source generation, the X-Content-Length, and the
	// restore-probe read-back (readBlockChecksum reads bkBlockMiB) all agree.
	publishDISrcFile = "/tmp/pub-di-src.bin"

	// publishDILabel prefixes the phase-5 comparator/log lines for this variant.
	publishDILabel = "DI-publish"
)

// publishDataImportSpecs registers the DataImport publish:true flow (env-gated by E2E_PUBLISH). Ordered:
// the source snapshot + external data file are built once in BeforeAll, then the DataImport is published,
// fed externally, driven to completion, and validated, and finally its infra teardown is asserted.
// Registered after the DataExport publish spec in the single root Ordered container.
func publishDataImportSpecs() {
	Context("Publish 2: DataImport publish:true (external ingress upload)", Ordered, func() {
		var (
			srcNS      string
			importNS   string
			rootSnap   string // source root Snapshot name (its content carries the orphan-PVC dataRef)
			orphanVS   string // resolved source VolumeSnapshot leaf name
			vsManifest []byte // source VolumeSnapshot leaf manifests-download payload (the captured PVC)
			scName     string
			scSize     string
			scMode     string
			srcSum     string // sha256 of the external random block payload (the data source of truth)
			token      string
			intURL     string // status.url (internal, direct to importer pod)
			pubURL     string // status.publicURL (external, through the origin ingress host)
			caPEM      string
			masterIP   string
			ingName    string
			svcName    string
		)

		BeforeAll(func() {
			if !suiteCfg.publish {
				Skip("E2E_PUBLISH=false: skipping the DataImport publish (external ingress upload) flow (it runs by default)")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
			defer cancel()

			srcNS = uniqueNS("pub-di-src")
			importNS = uniqueNS("pub-di-imp")
			rootSnap = publishDIImportRoot + "-source"
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

			By("Wiring the StorageClass to a VolumeSnapshotClass for the local CSI driver (capture needs it)")
			Expect(ensureStorageClassVolumeSnapshotClass(ctx, suiteCfg.storageClass)).To(Succeed())

			By("Creating the source and import namespaces")
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			Expect(ensureNamespace(ctx, importNS)).To(Succeed())
			// Both namespaces are consumed across every It below, so their teardown MUST be container-scoped
			// (registered here in BeforeAll -> fires once at AfterAll), never inside an It (a DeferCleanup in
			// an It runs as an AfterEach right after that It and would delete the fixtures mid-run). Deleting
			// the root Snapshot first cascades its SnapshotContent/artifact before the namespace goes away.
			DeferCleanup(func() {
				if cleanupSkipped() {
					GinkgoWriter.Printf("%s: keeping DataImport publish source namespace %q\n", keepReason(), srcNS)
					return
				}
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				_ = suiteDyn.Resource(snapshotGVR).Namespace(srcNS).Delete(cctx, rootSnap, metav1.DeleteOptions{})
				deleteNamespace(cctx, srcNS)
			})
			DeferCleanup(func() {
				if cleanupSkipped() {
					GinkgoWriter.Printf("%s: keeping DataImport publish import namespace %q\n", keepReason(), importNS)
					return
				}
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				_ = suiteDyn.Resource(snapshotGVR).Namespace(importNS).Delete(cctx, publishDIImportRoot, metav1.DeleteOptions{})
				deleteNamespace(cctx, importNS)
			})

			By("Ensuring the base-cluster external curl runner pod")
			Expect(ensureExternalCurlPod(ctx)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer ccancel()
				deleteExternalCurlPod(cctx)
			})

			By("Generating the external random block payload in the base curl pod and recording its sha256")
			extTarget, terr := externalCurlTarget()
			Expect(terr).NotTo(HaveOccurred())
			sum, gerr := generateRandomFileInPod(ctx, extTarget, publishDISrcFile, bkBlockMiB)
			Expect(gerr).NotTo(HaveOccurred(), "generate external block payload")
			Expect(sum).To(HaveLen(64), "external block payload sha256")
			srcSum = sum

			By("Creating the source orphan Block PVC " + publishDISrcPVC + " and writing data (binds the WFFC PVC)")
			Expect(createBlockPVC(ctx, srcNS, publishDISrcPVC, suiteCfg.storageClass, publishDISrcSize)).To(Succeed())
			// The block-writer pod is the first consumer that binds the WaitForFirstConsumer PVC; it writes
			// arbitrary bytes so the captured VolumeSnapshot is real. Its data is irrelevant (the imported
			// bytes come from the external file), so its checksum is discarded.
			_, err = suiteClientset.CoreV1().Pods(srcNS).Create(ctx, blockWriterPodSpec(srcNS, publishDISrcWriter, publishDISrcPVC), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create source block writer pod")
			Expect(waitPodRunning(ctx, srcNS, publishDISrcWriter, 10*time.Minute)).To(Succeed())
			_, werr := writeBlockAndChecksum(ctx, srcNS, publishDISrcWriter, publishDISrcPVC)
			Expect(werr).NotTo(HaveOccurred(), "write source block data")
			// Functional detach: the writer must release the RWO PVC before it can be snapshotted.
			forceDeletePod(ctx, srcNS, publishDISrcWriter)
			Expect(waitPodDeleted(ctx, srcNS, publishDISrcWriter, 3*time.Minute)).To(Succeed())
		})

		It("(a) captures the source orphan PVC as a VolumeSnapshot leaf and downloads its manifests", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.captureReadyTO+10*time.Minute)
			defer cancel()

			By("Creating the source root Snapshot over the orphan Block PVC")
			Expect(createRootSnapshot(ctx, srcNS, rootSnap)).To(Succeed())

			By("Waiting for the source Snapshot + bound SnapshotContent + children to become Ready")
			content, err := waitSnapshotReady(ctx, srcNS, rootSnap, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.captureReadyTO)).To(Succeed())
			nodes, err := walkSnapshotTree(ctx, srcNS, rootSnap)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).NotTo(BeEmpty(), "the source tree must contain the orphan-PVC VolumeSnapshot leaf")
			Expect(waitChildrenReady(ctx, srcNS, nodes, suiteCfg.captureReadyTO)).To(Succeed())

			By("Resolving the source VolumeSnapshot leaf for the orphan PVC and downloading its manifests")
			vs, rerr := resolveSourceOrphanVS(ctx, srcNS, content, publishDISrcPVC)
			Expect(rerr).NotTo(HaveOccurred())
			orphanVS = vs
			GinkgoWriter.Printf("  source orphan VolumeSnapshot leaf: %s (PVC %s)\n", orphanVS, publishDISrcPVC)
			body, gerr := aggGet(ctx, vsConnectorSubPath(srcNS, orphanVS, subManifestsDownload), nil)
			Expect(gerr).NotTo(HaveOccurred(), "GET source VolumeSnapshot manifests")
			objs, derr := decodeManifestArray(body)
			Expect(derr).NotTo(HaveOccurred())
			Expect(objs).NotTo(BeEmpty(), "source VolumeSnapshot manifests-download must carry the captured PVC")
			vsManifest = body

			By("Deriving the import scratch-volume parameters from the source PVC")
			s, z, m, perr := sourcePVCScratchParams(ctx, srcNS, publishDISrcPVC)
			Expect(perr).NotTo(HaveOccurred())
			Expect(s).NotTo(BeEmpty(), "source PVC storageClassName")
			Expect(m).To(Equal("Block"), "source PVC volumeMode must be Block for the block import")
			scName, scSize, scMode = s, z, m
		})

		It("(b) publishes the DataImport: import-root + VS leaf + DataImport(publish:true), Ready + publicURL + Ingress", func() {
			Expect(orphanVS).NotTo(BeEmpty(), "the capture step must have resolved the source VolumeSnapshot leaf")

			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.dataTransferTO+15*time.Minute)
			defer cancel()

			By("Materializing the import-mode root Snapshot + VolumeSnapshot leaf in " + importNS)
			Expect(createImportRootSnapshot(ctx, importNS, publishDIImportRoot)).To(Succeed())
			rootUID, err := getSnapshotUID(ctx, importNS, publishDIImportRoot)
			Expect(err).NotTo(HaveOccurred())
			parentRef := importNodeOwnerRef("state-snapshotter.deckhouse.io/v1alpha1", "Snapshot", publishDIImportRoot, rootUID, true)
			Expect(createImportVolumeSnapshot(ctx, importNS, publishDIVSLeaf, []metav1.OwnerReference{parentRef})).To(Succeed())

			By("Creating DataImport " + publishDIVSLeaf + " (mode PopulateData, publish:true, snapshotRef the VS leaf)")
			// NB: the DataImport is consumed by (c)-(f); its teardown rides on the container-scoped importNS
			// cleanup registered in BeforeAll, NOT a spec-scoped DeferCleanup here.
			Expect(createPublishDataImport(ctx, importNS, publishDIVSLeaf, vsAPIVersion, "VolumeSnapshot", publishDIVSLeaf, scName, scSize, scMode, publishDITTL)).To(Succeed())

			By("Uploading the manifests (VS leaf + import root) via the aggregated API (in-cluster, bind-first retry)")
			vsLeaf := &importNode{
				name:       publishDIVSLeaf,
				kind:       "VolumeSnapshot",
				group:      "snapshot.storage.k8s.io",
				apiVersion: vsAPIVersion,
				manifests:  vsManifest,
				dataLeaf:   true,
				pvcName:    publishDISrcPVC,
			}
			Expect(uploadImportTree(ctx, importNS, publishDIImportRoot, emptyJSONArray(), []*importNode{vsLeaf})).To(Succeed())

			By("Waiting for the DataImport to become Ready with status.url + status.publicURL")
			u, p, ca, werr := waitDataImportPublished(ctx, importNS, publishDIVSLeaf, suiteCfg.dataTransferTO)
			Expect(werr).NotTo(HaveOccurred(), "DataImport %s Ready + published", publishDIVSLeaf)
			intURL, pubURL, caPEM = u, p, ca
			GinkgoWriter.Printf("  DataImport %s status.url=%s status.publicURL=%s\n", publishDIVSLeaf, intURL, pubURL)

			By("Asserting status.publicURL matches https://<originIngressHost>/<importNS>/pvc/<diName>/")
			// TODO(e2e-publish): verify on cluster — confirm the DI publicURL path shape is
			// /<ns>/pvc/<dataImportName>/ (data-import ensurePublish Path = /<ns>/<TargetKindShort>/<TargetName>,
			// with TargetKindShort="pvc" and TargetName=DataImport name; EnsureIngressResource appends the
			// trailing slash).
			wantPrefix := fmt.Sprintf("https://%s/%s/pvc/%s/", suitePublishInfra.originIngressHost, importNS, publishDIVSLeaf)
			Expect(pubURL).To(Equal(wantPrefix), "status.publicURL")
			Expect(intURL).NotTo(BeEmpty(), "status.url (internal)")

			By("Asserting status.ca is a valid PEM chain")
			// TODO(e2e-publish): verify on cluster — the importer serves under its own CA; pin the exact
			// Issuer CommonName (data-exporter-CA vs data-importer-CA) once observed. For now assert it parses.
			issuer, cerr := caIssuerCommonName(caPEM)
			Expect(cerr).NotTo(HaveOccurred(), "parse status.ca")
			GinkgoWriter.Printf("  DataImport %s status.ca issuer CommonName=%q\n", publishDIVSLeaf, issuer)

			By("Asserting the publish Ingress + backing Service were created in " + d8DataManagerNS)
			name, svc, found, ferr := findPublishIngress(ctx, pubURL)
			Expect(ferr).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), "a publish Ingress for %s must exist in %s", pubURL, d8DataManagerNS)
			ingName, svcName = name, svc
			_, gerr := suiteClientset.CoreV1().Services(d8DataManagerNS).Get(ctx, svcName, metav1.GetOptions{})
			Expect(gerr).NotTo(HaveOccurred(), "publish Service %s/%s must exist", d8DataManagerNS, svcName)
		})

		It("(c) EXTERNAL upload (status.publicURL): no-token PUT is rejected, then Bearer PUT + finished succeed", func() {
			Expect(pubURL).NotTo(BeEmpty(), "the publish spec must have populated status.publicURL")

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()

			extTarget, terr := externalCurlTarget()
			Expect(terr).NotTo(HaveOccurred())

			By("Creating the token identity (SA + dataimports/download Role + RoleBinding) in " + importNS)
			Expect(createServiceAccountIfNotExists(ctx, importNS, publishDISA)).To(Succeed())
			Expect(createDataDownloadRole(ctx, importNS, publishDIRole, dataImportGVR.Resource)).To(Succeed())
			Expect(bindRoleToServiceAccount(ctx, importNS, publishDIBinding, publishDIRole, importNS, publishDISA)).To(Succeed())
			t, tokErr := issueServiceAccountToken(ctx, importNS, publishDISA, publishTokenTTL)
			Expect(tokErr).NotTo(HaveOccurred())
			token = t

			blockURL := publishDataURL(pubURL, publishBlockPath)
			finishedURL := publishDataURL(pubURL, publishFinishedPath)
			uploadHeaders := map[string]string{
				"X-Content-Length":        strconv.Itoa(bkBlockBytes),
				"X-Offset":                "0",
				"X-Attribute-Permissions": "0644",
				"X-Attribute-Uid":         "0",
				"X-Attribute-Gid":         "0",
			}

			By("NEGATIVE: a PUT through the ingress WITHOUT a token is rejected (401/403)")
			// Same request shape as the authorized upload minus the Authorization header. The importer
			// authorizes every request via a TokenReview/SAR, so a token-less request is rejected before the
			// body is transferred (curl's Expect: 100-continue lets the server answer 401 without receiving
			// the payload), leaving the block device untouched for the authorized PUT below.
			// TODO(e2e-publish): verify on cluster — confirm the exact status (401 unauthenticated vs 403) and
			// that the rejection precedes body transfer.
			negCode, _, nerr := runCurlRequest(ctx, extTarget, blockURL, curlRequest{
				method: "PUT", token: "", dataFile: publishDISrcFile, headers: uploadHeaders, masterIP: masterIP,
			})
			Expect(nerr).NotTo(HaveOccurred())
			Expect(negCode).To(BeElementOf(401, 403), "a token-less upload through the ingress must be rejected (got %d)", negCode)

			By("Uploading the block payload through the ingress (Bearer) with PUT /api/v1/block")
			// External TLS is terminated at nginx with the origin (self-signed) cert, so -k (caFile empty);
			// masterIP enables the sslip.io --resolve fallback if the base pod's DNS cannot resolve the host.
			// The RoleBinding above is authorized per-request via a fresh SubjectAccessReview, which is
			// eventually consistent: a PUT that races RBAC propagation can hit a stale 403, so retry until the
			// upload is accepted (matching the DE sibling's Eventually around its first authorized request).
			// TODO(e2e-publish): verify on cluster — confirm the block upload chunk protocol (single PUT with
			// X-Content-Length/X-Offset/X-Attribute-* headers) and the success status code range.
			Eventually(func() (int, error) {
				code, _, e := runCurlRequest(ctx, extTarget, blockURL, curlRequest{
					method: "PUT", token: token, dataFile: publishDISrcFile, headers: uploadHeaders, masterIP: masterIP,
				})
				return code, e
			}).WithContext(ctx).WithTimeout(2*time.Minute).WithPolling(pollInterval).Should(
				And(BeNumerically(">=", 200), BeNumerically("<", 300)),
				"block PUT must succeed once the RoleBinding propagates (SAR is eventually consistent)")

			By("Finalizing the upload through the ingress with POST /api/v1/finished")
			// TODO(e2e-publish): verify on cluster — confirm POST /api/v1/finished takes no body and returns 2xx.
			finCode, _, ferr := runCurlRequest(ctx, extTarget, finishedURL, curlRequest{
				method: "POST", token: token, masterIP: masterIP,
			})
			Expect(ferr).NotTo(HaveOccurred())
			Expect(finCode).To(And(BeNumerically(">=", 200), BeNumerically("<", 300)), "POST /finished must succeed (got %d)", finCode)
		})

		It("(d) reaches the terminal DataImport state (phase=Completed, completionTimestamp, artifactRef -> VolumeSnapshotContent)", func() {
			Expect(token).NotTo(BeEmpty(), "the external upload step must have run")

			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.dataTransferTO+10*time.Minute)
			defer cancel()

			By("Waiting for the DataImport to reach Completed (Ready + phase=Completed + completionTimestamp + artifactRef)")
			// waitDataImportCompleted pins the CURRENT catalog: Completed condition True, status.phase=Completed,
			// a non-empty status.completionTimestamp, a non-empty status.data.artifactRef (post role-3 rename),
			// and conditions within {Ready, UploadFinished, Completed}.
			Expect(waitDataImportCompleted(ctx, importNS, publishDIVSLeaf, suiteCfg.dataTransferTO)).To(Succeed())

			By("Asserting status.data.artifactRef references a real VolumeSnapshotContent")
			di, gerr := getResource(ctx, dataImportGVR, importNS, publishDIVSLeaf)
			Expect(gerr).NotTo(HaveOccurred())
			artifact, found, _ := unstructured.NestedMap(di.Object, "status", "data", "artifactRef")
			Expect(found).To(BeTrue(), "status.data.artifactRef must be set on a Completed DataImport")
			artKind, _, _ := unstructured.NestedString(artifact, "kind")
			artName, _, _ := unstructured.NestedString(artifact, "name")
			// A PopulateData block import always produces a VolumeSnapshotContent artifact (data-import
			// volume_capture artifactKindVolumeSnapshotContent).
			Expect(artKind).To(Equal("VolumeSnapshotContent"), "the block import artifact kind")
			Expect(artName).NotTo(BeEmpty(), "artifactRef.name")
			_, verr := getResource(ctx, volumeSnapshotContentGVR, "", artName)
			Expect(verr).NotTo(HaveOccurred(), "the artifact VolumeSnapshotContent %q must exist", artName)
			GinkgoWriter.Printf("  DataImport %s artifactRef -> %s/%s (exists)\n", publishDIVSLeaf, artKind, artName)
		})

		It("(e) validates the imported tree: manifests field-match the source and restored bytes match the external payload", func() {
			Expect(srcSum).To(HaveLen(64), "the external payload sha256 must have been recorded")

			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.snapshotReadyTO+15*time.Minute)
			defer cancel()

			By("Waiting for the imported tree (root Snapshot + content + children) to become Ready")
			content, err := waitSnapshotReady(ctx, importNS, publishDIImportRoot, suiteCfg.snapshotReadyTO)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitSnapshotContentReady(ctx, content, suiteCfg.snapshotReadyTO)).To(Succeed())
			nodes, err := walkSnapshotTree(ctx, importNS, publishDIImportRoot)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitChildrenReady(ctx, importNS, nodes, suiteCfg.snapshotReadyTO)).To(Succeed())

			By("Restoring the imported tree and field-comparing the manifests to the live source objects")
			restorePath := coreSnapshotSubPath(importNS, publishDIImportRoot, subManifestsRestore)
			body, gerr := aggGet(ctx, restorePath, map[string]string{"targetNamespace": importNS})
			Expect(gerr).NotTo(HaveOccurred(), "GET %s", restorePath)
			objs, derr := decodeManifestArray(body)
			Expect(derr).NotTo(HaveOccurred())
			Expect(objs).NotTo(BeEmpty(), "restore returned no manifests")
			ptrs := make([]*unstructured.Unstructured, 0, len(objs))
			for i := range objs {
				ptrs = append(ptrs, &objs[i])
			}
			Expect(applyObjects(ctx, ptrs, importNS)).To(Succeed())
			Expect(assertManifestsMatchLive(ctx, publishDILabel, srcNS, objs)).To(Succeed())

			By("Restoring the imported bytes into a PVC and matching the external payload checksum")
			probePod := restoreProbePodName(publishDISrcPVC)
			_, cerr := suiteClientset.CoreV1().Pods(importNS).Create(ctx, restoreProbePodSpec(importNS, probePod, publishDISrcPVC, bkProbeDevicePath), metav1.CreateOptions{})
			Expect(cerr).NotTo(HaveOccurred(), "create restore probe pod")
			Expect(waitPodRunning(ctx, importNS, probePod, 15*time.Minute)).To(Succeed())
			got, rerr := readBlockChecksum(ctx, importNS, probePod, bkRestoreProbeCont, bkProbeDevicePath)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(got).To(Equal(srcSum), "restored block bytes must match the externally-uploaded payload")
			GinkgoWriter.Printf("  [%s] restored PVC %s sha256=%s == external payload sha256=%s — MATCH\n", publishDILabel, publishDISrcPVC, got, srcSum)
		})

		It("(f) teardown: deleting the DataImport reaps the publish Ingress + Service", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()

			By("Deleting the DataImport and asserting the publish Ingress + Service are reaped in " + d8DataManagerNS)
			// sf does NOT tear the publish infra down on the terminal Completed phase — a completed reconcile
			// returns early (data-import data_import_resource.go). The server (deployment/service/ingress) is
			// reaped only via idle-TTL expiry (60m here) or DataImport DELETION
			// (cleanupDataImport -> teardownImportInfra). So drive teardown by deleting the DataImport, mirroring
			// the DataExport sibling (publish_de_test.go deletes the resource, then asserts the reap).
			GinkgoWriter.Printf("  deleting DataImport %s/%s; publish Ingress %s/%s (Service %s) expected to be reaped\n", importNS, publishDIVSLeaf, d8DataManagerNS, ingName, svcName)
			deleteDataImport(ctx, importNS, publishDIVSLeaf)
			Eventually(func() (bool, error) {
				_, _, found, ferr := findPublishIngress(ctx, pubURL)
				return found, ferr
			}).WithContext(ctx).WithTimeout(5*time.Minute).WithPolling(pollInterval).Should(BeFalse(),
				"the publish Ingress must be reaped after the DataImport is deleted")
			Eventually(func() bool {
				_, gerr := suiteClientset.CoreV1().Services(d8DataManagerNS).Get(ctx, svcName, metav1.GetOptions{})
				return apierrors.IsNotFound(gerr)
			}).WithContext(ctx).WithTimeout(5*time.Minute).WithPolling(pollInterval).Should(BeTrue(),
				"the publish Service must be reaped after the DataImport is deleted")
		})
	})
}

// --- spec-local helpers ----------------------------------------------------

// createBlockPVC creates a Block-mode RWO PVC on the given StorageClass. The default e2e StorageClass
// (sds-local-volume) is WaitForFirstConsumer, so the PVC binds only once a consumer Pod (the block
// writer) is scheduled.
func createBlockPVC(ctx context.Context, ns, name, sc, size string) error {
	mode := corev1.PersistentVolumeBlock
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			VolumeMode:       &mode,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
	if _, err := suiteClientset.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create Block PVC %s/%s: %w", ns, name, err)
	}
	return nil
}

// createPublishDataImport creates a PopulateData DataImport with publish:true targeting an import-mode
// snapshot leaf (snapshotRef). The importer provisions a scratch PVC (named after the DataImport), serves
// the block upload endpoint through the publish ingress, and on POST /finished captures the uploaded bytes
// into a durable VolumeSnapshotContent artifact. storageParams are derived from the source PVC.
func createPublishDataImport(ctx context.Context, ns, name, snapAPIVersion, snapKind, leafName, sc, size, volumeMode, ttl string) error {
	storageParams := map[string]interface{}{
		"storageClassName": sc,
		"size":             size,
	}
	if volumeMode != "" {
		storageParams["volumeMode"] = volumeMode
	}
	di := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": dataImportGVR.GroupVersion().String(),
		"kind":       "DataImport",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"ttl":     ttl,
			"mode":    "PopulateData",
			"publish": true,
			"snapshotRef": map[string]interface{}{
				"apiVersion": snapAPIVersion,
				"kind":       snapKind,
				"name":       leafName,
			},
			"storageParams": storageParams,
		},
	}}
	_, err := suiteDyn.Resource(dataImportGVR).Namespace(ns).Create(ctx, di, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// waitDataImportPublished polls a DataImport until it is Ready with BOTH status.url (internal) and
// status.publicURL (external) populated, status.volumeMode=Block, a non-empty status.ca, status.phase=Ready,
// and conditions within the DataImport catalog. It is the publish counterpart of waitDataImportReady
// (which does not wait for the publish resources).
func waitDataImportPublished(ctx context.Context, ns, name string, timeout time.Duration) (url, publicURL, ca string, err error) {
	deadline := time.Now().Add(timeout)
	var last string
	var polls int
	for {
		obj, gerr := getResource(ctx, dataImportGVR, ns, name)
		if gerr == nil {
			st, reason, found := conditionStatus(obj, "Ready")
			volMode, _, _ := unstructured.NestedString(obj.Object, "status", "volumeMode")
			if found && st == "True" {
				url, _, _ = unstructured.NestedString(obj.Object, "status", "url")
				publicURL, _, _ = unstructured.NestedString(obj.Object, "status", "publicURL")
				rawCA, _, _ := unstructured.NestedString(obj.Object, "status", "ca")
				ca = normalizePublishCA(rawCA) // status.ca is base64-encoded PEM per the CRD; decode for pem.Decode / --cacert
				phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
				catalogOK, extra := conditionsWithinCatalog(obj, "Ready", "UploadFinished", "Completed")
				if url != "" && publicURL != "" && volMode == "Block" && ca != "" && phase == "Ready" && catalogOK {
					return url, publicURL, ca, nil
				}
				last = fmt.Sprintf("Ready=True but url=%q publicURL=%q volumeMode=%q ca=%t phase=%q extraConditions=%v", url, publicURL, volMode, ca != "", phase, extra)
			} else {
				last = fmt.Sprintf("Ready=%v reason=%q volumeMode=%q", st, reason, volMode)
			}
		} else {
			last = fmt.Sprintf("get err=%v", gerr)
		}
		polls++
		if polls == 1 || polls%12 == 0 {
			GinkgoWriter.Printf("  DataImport %s/%s publish wait: %s\n", ns, name, last)
		}
		if time.Now().After(deadline) {
			dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Minute)
			dumpStuckDataImportDiagnostics(dctx, ns, name)
			dcancel()
			return "", "", "", fmt.Errorf("timeout waiting for DataImport %s/%s Ready+published; last: %s", ns, name, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return "", "", "", ctx.Err()
		}
	}
}

// resolveSourceOrphanVS waits for the root content's orphan-PVC dataRef to publish, then finds the
// VolumeSnapshot leaf whose bound SnapshotContent captured srcPVC (matching the phase-4
// resolveBackupSnapRefs orphan resolution). Returns the leaf name.
func resolveSourceOrphanVS(ctx context.Context, srcNS, rootContent, srcPVC string) (string, error) {
	if _, err := waitContentDataRefs(ctx, rootContent, []string{srcPVC}, suiteCfg.captureReadyTO); err != nil {
		return "", err
	}
	list, lerr := suiteDyn.Resource(volumeSnapshotGVR).Namespace(srcNS).List(ctx, metav1.ListOptions{})
	if lerr != nil {
		return "", fmt.Errorf("list VolumeSnapshots in %s: %w", srcNS, lerr)
	}
	for i := range list.Items {
		vs := &list.Items[i]
		contentName, _, _ := unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName")
		if contentName == "" {
			continue
		}
		content, gerr := getResource(ctx, snapshotContentGVR, "", contentName)
		if gerr != nil {
			continue
		}
		targetName, _, _ := unstructured.NestedString(content.Object, "status", "data", "sourceRef", "name")
		if targetName == srcPVC {
			return vs.GetName(), nil
		}
	}
	return "", fmt.Errorf("no VolumeSnapshot leaf found for source orphan PVC %q in %s", srcPVC, srcNS)
}

// generateRandomFileInPod writes a random file of `mib` MiB inside a pod (via an exec, using /dev/urandom)
// and returns its sha256 hex digest (busybox sha256sum, matching the source-side dd|sha256sum and the
// restore-probe read-back). The path is a test-controlled structural string (single-quoted).
func generateRandomFileInPod(ctx context.Context, t curlPodTarget, path string, mib int) (string, error) {
	if t.local {
		// No pod: generate the payload in-process on the test runner and register it under `path` so the
		// later local PUT (curlRequest.dataFile == path) sources its body from memory. Returns the same
		// sha256 the restore-probe read-back compares against (see publish_external_http_test.go).
		return generateLocalPayload(path, mib)
	}
	script := "dd if=/dev/urandom of=" + shQuote(path) + " bs=1M count=" + strconv.Itoa(mib) + " 2>/dev/null && sha256sum " + shQuote(path) + " | awk '{print $1}'"
	stdout, stderr, err := t.exec(ctx, []string{"sh", "-c", script})
	if err != nil {
		return "", fmt.Errorf("generate random file %s in %s/%s: %w (stderr=%q)", path, t.ns, t.pod, err, stderr)
	}
	sum := strings.TrimSpace(stdout)
	if len(sum) != 64 {
		return "", fmt.Errorf("unexpected sha256 for %s: %q (stderr=%q)", path, sum, stderr)
	}
	return sum, nil
}
