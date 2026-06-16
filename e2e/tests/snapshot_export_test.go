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
	"encoding/hex"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// Object names created by the suite.
const (
	csdName            = "demo-live-vm-disk"
	vmName             = "vm-1"
	ownedDiskName      = "disk-vm"
	standaloneDiskName = "disk-standalone"
	appPVCName         = "app-pvc"
	appPodName         = "app-pod"
	snapshotName       = "demo-tree"
	snapshotExportName = "demo-export"
	subtreeExportName  = "demo-subtree-export"

	// Import round-trip: re-root the uploaded parent bundle at the standalone disk's child snapshot
	// and recreate it under a fresh target name (no collision with the still-present source).
	importName       = "demo-import"
	importTargetName = "disk-imported"

	demoAPIVersion = "demo.state-snapshotter.deckhouse.io/v1alpha1"

	// SnapshotImport Ready reasons asserted by the round-trip.
	reasonImported = "Imported"

	// Block-mode workload: a raw volumeDevices PVC with a known 16-byte signature dd'd at offset 0,
	// captured by the same namespace Snapshot and exercised over /api/v1/block.
	blockPVCName    = "block-pvc"
	blockPodName    = "block-pod"
	blockDevicePath = "/dev/xdata"
	blockMagic      = "SNAPE2EBLOCK0001"
	blockPVCSize    = "64Mi"

	appLabelKey      = "app"
	appLabelValue    = "snap-e2e-app"
	appLabelSelector = appLabelKey + "=" + appLabelValue
)

// Step timeouts. CreateDefaultStorageClass internally enables modules, labels
// nodes, attaches disks and waits for LVGs, so it gets the largest budget.
const (
	scCreateTimeout      = 40 * time.Minute
	nsCreateTimeout      = 2 * time.Minute
	csdEligibleTimeout   = 5 * time.Minute
	pvcBindTimeout       = 5 * time.Minute
	podReadyTimeout      = 5 * time.Minute
	applyTimeout         = 2 * time.Minute
	snapshotReadyTimeout = 10 * time.Minute
	exportReadyTimeout   = 20 * time.Minute

	pvcBindAttempts = 60
	pvcBindInterval = 5 * time.Second
)

var _ = Describe("Snapshot export (state-snapshotter)", Ordered, func() {
	// testNamespace is the random namespace this run operates in; populated by
	// the namespace step and reused by all later steps.
	var testNamespace string

	It("provisions a Thin sds-local-volume StorageClass", func() {
		ctx, cancel := context.WithTimeout(context.Background(), scCreateTimeout)
		defer cancel()

		By("creating a Thin LocalStorageClass via the sds-local-volume helper")
		scName, err := testkit.CreateDefaultStorageClass(ctx, suiteRestCfg, testkit.DefaultStorageClassConfig{
			StorageClassName: suiteCfg.thinSCName,
			LVMType:          "Thin",
			ThinPoolName:     "thinpool",
			ThinPoolSize:     "90%",
			// Disk attach only happens on the storage-e2e-created path
			// (BaseKubeconfig != nil); on an existing cluster disks are
			// assumed pre-provisioned and this is a no-op.
			BaseKubeconfig:       suiteClusterResources.BaseKubeconfig,
			VMNamespace:          suiteCfg.vmNamespace,
			BaseStorageClassName: suiteCfg.baseStorageClass,
		})
		Expect(err).NotTo(HaveOccurred(), "create thin StorageClass")
		Expect(scName).To(Equal(suiteCfg.thinSCName))
	})

	It("creates a randomly-named test namespace", func() {
		ctx, cancel := context.WithTimeout(context.Background(), nsCreateTimeout)
		defer cancel()

		ns, err := newRandomNamespace(ctx)
		Expect(err).NotTo(HaveOccurred(), "create random test namespace")
		testNamespace = ns
		suiteNamespace = ns
		GinkgoWriter.Printf("test namespace: %s\n", testNamespace)
	})

	It("registers the CustomSnapshotDefinition and waits for it to be eligible", func() {
		ctx, cancel := context.WithTimeout(context.Background(), csdEligibleTimeout)
		defer cancel()

		By("applying the demo VM/Disk CustomSnapshotDefinition (cluster-scoped)")
		Expect(applyYAML(ctx, csdManifest(), "")).To(Succeed(), "apply CustomSnapshotDefinition")

		By("waiting for the CSD to become eligible (Accepted=True, RBACReady=True)")
		Expect(waitCRCondition(ctx, csdGVR, "", csdName, condTypeAccepted, condStatusTrue, csdEligibleTimeout)).
			To(Succeed(), "CSD Accepted=True")
		Expect(waitCRCondition(ctx, csdGVR, "", csdName, condTypeRBACReady, condStatusTrue, csdEligibleTimeout)).
			To(Succeed(), "CSD RBACReady=True")
	})

	It("creates the app Pod + PVC and waits for Bound + Ready", func() {
		ctx, cancel := context.WithTimeout(context.Background(), pvcBindTimeout+podReadyTimeout)
		defer cancel()

		By("applying the app PVC + Pod (the residual/orphan PVC = the export data leg)")
		Expect(applyYAML(ctx, appWorkloadManifest(testNamespace, suiteCfg.thinSCName, suiteCfg.probeImage, suiteCfg.pvcSize), testNamespace)).
			To(Succeed(), "apply app PVC + Pod")

		By("waiting for the app PVC to bind")
		Expect(storagekube.WaitForPVCsBound(ctx, suiteClientset, testNamespace, appLabelSelector, 1, pvcBindAttempts, pvcBindInterval)).
			To(Succeed(), "app PVC Bound")

		By("waiting for the app Pod to be Ready")
		Expect(storagekube.WaitForAllPodsReadyInNamespace(ctx, suiteRestCfg, testNamespace, podReadyTimeout)).
			To(Succeed(), "app Pod Ready")
	})

	It("creates the demo VirtualMachine and disks", func() {
		ctx, cancel := context.WithTimeout(context.Background(), applyTimeout)
		defer cancel()

		By("applying DemoVirtualMachine + owned DemoVirtualDisk + standalone DemoVirtualDisk")
		Expect(applyYAML(ctx, demoDomainManifest(testNamespace), testNamespace)).
			To(Succeed(), "apply demo VM + disks")
	})

	It("creates a Block-mode PVC + raw-device pod and writes a known signature", func() {
		ctx, cancel := context.WithTimeout(context.Background(), pvcBindTimeout+podReadyTimeout)
		defer cancel()

		By("relaxing the namespace Pod Security level so the raw-device pod (root) admits")
		Expect(applyYAML(ctx, namespacePSSManifest(testNamespace), "")).
			To(Succeed(), "relax namespace PSS to privileged")

		By("applying the Block PVC (volumeMode: Block) + a root pod with volumeDevices")
		Expect(applyYAML(ctx, blockWorkloadManifest(testNamespace, suiteCfg.thinSCName, suiteCfg.probeImage, blockPVCSize), testNamespace)).
			To(Succeed(), "apply block PVC + pod")

		By("waiting for the block pod to be Ready (signature dd'd to the raw device on start)")
		Expect(waitPodReady(ctx, testNamespace, blockPodName, pvcBindTimeout+podReadyTimeout)).
			To(Succeed(), "block pod Ready")
	})

	It("snapshots the namespace and waits for the Snapshot to be Ready", func() {
		ctx, cancel := context.WithTimeout(context.Background(), snapshotReadyTimeout+applyTimeout)
		defer cancel()

		By("applying the root Snapshot (spec: {} -> dynamic namespace capture)")
		Expect(applyYAML(ctx, snapshotManifest(testNamespace), testNamespace)).
			To(Succeed(), "apply Snapshot")

		By("waiting for Snapshot Ready=True")
		Expect(waitCRCondition(ctx, snapshotGVR, testNamespace, snapshotName, condTypeReady, condStatusTrue, snapshotReadyTimeout)).
			To(Succeed(), "Snapshot Ready=True")
	})

	It("exports a namespace snapshot to a SnapshotExport endpoint", func() {
		ctx, cancel := context.WithTimeout(context.Background(), exportReadyTimeout+applyTimeout)
		defer cancel()

		By("applying the SnapshotExport referencing the root Snapshot")
		Expect(applyYAML(ctx, snapshotExportManifest(testNamespace), testNamespace)).
			To(Succeed(), "apply SnapshotExport")

		By("waiting for SnapshotExport DataReady=True")
		Expect(waitCRCondition(ctx, snapshotExportGVR, testNamespace, snapshotExportName, condTypeDataReady, condStatusTrue, exportReadyTimeout)).
			To(Succeed(), "SnapshotExport DataReady=True")

		By("waiting for SnapshotExport Ready=True")
		Expect(waitCRCondition(ctx, snapshotExportGVR, testNamespace, snapshotExportName, condTypeReady, condStatusTrue, exportReadyTimeout)).
			To(Succeed(), "SnapshotExport Ready=True")

		By("asserting Ready reason is Published")
		_, reason, found, err := getCRCondition(ctx, snapshotExportGVR, testNamespace, snapshotExportName, condTypeReady)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue(), "Ready condition present")
		Expect(reason).To(Equal(reasonPublished), "Ready reason")

		By("reading the flat per-node status.snapshots[]")
		entries, err := getExportSnapshots(ctx, testNamespace, snapshotExportName)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).NotTo(BeEmpty(), "status.snapshots non-empty")

		By("following status.indexURL as an opaque blob (the suite never parses it)")
		indexURL, err := getStatusString(ctx, snapshotExportGVR, testNamespace, snapshotExportName, "indexURL")
		Expect(err).NotTo(HaveOccurred())
		Expect(indexURL).NotTo(BeEmpty(), "status.indexURL published")
		rawIndex, code, err := suiteAPI.get(ctx, indexURL)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(200), "GET indexURL")
		Expect(rawIndex).NotTo(BeEmpty(), "index blob non-empty")

		By("following each node's per-node manifestsURL (data and dataless)")
		var dataNodes []map[string]interface{}
		for _, e := range entries {
			murl, _ := e["manifestsURL"].(string)
			Expect(murl).NotTo(BeEmpty(), "every node carries a manifestsURL")
			raw, mc, merr := suiteAPI.get(ctx, murl)
			Expect(merr).NotTo(HaveOccurred(), "GET manifestsURL %s", murl)
			Expect(mc).To(Equal(200), "GET manifestsURL %s", murl)
			Expect(raw).NotTo(BeEmpty(), "manifests blob non-empty")
			if hasData, _ := e["hasData"].(bool); hasData {
				dataNodes = append(dataNodes, e)
			}
		}
		Expect(dataNodes).NotTo(BeEmpty(), "at least one data node in status.snapshots")

		By("bringing up the in-cluster downloader (SA + RBAC + curl pod)")
		Expect(ensureDataPod(ctx, testNamespace, podReadyTimeout)).To(Succeed(), "curl pod Ready")

		By("authenticating to each data node's endpoint from inside the cluster")
		for _, e := range dataNodes {
			id, _ := e["snapshotID"].(string)
			dataURL, _ := e["dataURL"].(string)
			ready, _ := e["ready"].(bool)
			volumeMode, _ := e["volumeMode"].(string)
			Expect(ready).To(BeTrue(), "data node %s ready", id)
			Expect(dataURL).NotTo(BeEmpty(), "data node %s has dataURL", id)
			apiPath := "api/v1/files/"
			if volumeMode == volumeModeBlock {
				apiPath = "api/v1/block"
			}
			Expect(dataReachable(ctx, testNamespace, dataURL, apiPath)).
				To(Succeed(), "data endpoint %s (%s) authorized + reachable", id, volumeMode)
		}

		GinkgoWriter.Printf("SnapshotExport %s/%s published %d node(s), %d with data\n",
			testNamespace, snapshotExportName, len(entries), len(dataNodes))
	})

	It("verifies the Block volume round-trips byte-exact over /api/v1/block", func() {
		ctx, cancel := context.WithTimeout(context.Background(), exportReadyTimeout)
		defer cancel()

		By("locating the single Block-mode data node in status.snapshots[]")
		entries, err := getExportSnapshots(ctx, testNamespace, snapshotExportName)
		Expect(err).NotTo(HaveOccurred())
		var block map[string]interface{}
		for _, e := range entries {
			if vm, _ := e["volumeMode"].(string); vm == volumeModeBlock {
				block = e
				break
			}
		}
		Expect(block).NotTo(BeNil(), "a Block-mode data node is present")
		dataURL, _ := block["dataURL"].(string)
		Expect(dataURL).NotTo(BeEmpty(), "block node dataURL")

		By("asserting the exporter reports a volume size of at least the requested PVC size")
		size, err := dataBlockSize(ctx, testNamespace, dataURL)
		Expect(err).NotTo(HaveOccurred())
		Expect(size).To(BeNumerically(">=", int64(64*1024*1024)), "block volume size >= 64Mi")

		By("asserting the dd-written signature at offset 0 round-trips byte-exact")
		gotHex, err := dataBlockHeadHex(ctx, testNamespace, dataURL, len(blockMagic))
		Expect(err).NotTo(HaveOccurred())
		Expect(gotHex).To(Equal(hex.EncodeToString([]byte(blockMagic))), "block signature matches")

		GinkgoWriter.Printf("Block node %v: size=%d signature=%s\n", block["snapshotID"], size, gotHex)
	})

	It("exports a child snapshot subtree via a typed snapshotRef (DemoVirtualDiskSnapshot)", func() {
		ctx, cancel := context.WithTimeout(context.Background(), exportReadyTimeout+applyTimeout)
		defer cancel()

		By("discovering the captured DemoVirtualDiskSnapshot for the standalone disk")
		diskSnapName, err := findDomainSnapshotName(ctx, demoDiskSnapshotGVR, testNamespace, standaloneDiskName)
		Expect(err).NotTo(HaveOccurred(), "find standalone disk snapshot")
		GinkgoWriter.Printf("standalone DemoVirtualDiskSnapshot: %s\n", diskSnapName)

		By("applying a SnapshotExport with a typed domain snapshotRef")
		Expect(applyYAML(ctx, subtreeExportManifest(testNamespace, diskSnapName), testNamespace)).
			To(Succeed(), "apply subtree SnapshotExport")

		By("waiting for the subtree SnapshotExport Ready=True")
		Expect(waitCRCondition(ctx, snapshotExportGVR, testNamespace, subtreeExportName, condTypeReady, condStatusTrue, exportReadyTimeout)).
			To(Succeed(), "subtree SnapshotExport Ready=True")

		By("asserting status.snapshots[] is scoped to the subtree and per-node URLs resolve")
		entries, err := getExportSnapshots(ctx, testNamespace, subtreeExportName)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).NotTo(BeEmpty(), "subtree status.snapshots non-empty")
		// A standalone disk snapshot is a leaf: its subtree is exactly itself.
		Expect(entries).To(HaveLen(1), "subtree contains only the selected leaf node")

		indexURL, err := getStatusString(ctx, snapshotExportGVR, testNamespace, subtreeExportName, "indexURL")
		Expect(err).NotTo(HaveOccurred())
		Expect(indexURL).NotTo(BeEmpty(), "subtree indexURL published")
		_, code, err := suiteAPI.get(ctx, indexURL)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(200), "GET subtree indexURL (generic domain endpoint)")

		for _, e := range entries {
			murl, _ := e["manifestsURL"].(string)
			Expect(murl).NotTo(BeEmpty(), "subtree node manifestsURL")
			_, mc, merr := suiteAPI.get(ctx, murl)
			Expect(merr).NotTo(HaveOccurred(), "GET subtree manifestsURL %s", murl)
			Expect(mc).To(Equal(200), "GET subtree manifestsURL (generic per-node endpoint)")
			if hasData, _ := e["hasData"].(bool); hasData {
				dataURL, _ := e["dataURL"].(string)
				vm, _ := e["volumeMode"].(string)
				apiPath := "api/v1/files/"
				if vm == volumeModeBlock {
					apiPath = "api/v1/block"
				}
				Expect(dataReachable(ctx, testNamespace, dataURL, apiPath)).
					To(Succeed(), "subtree data node reachable")
			}
		}
	})

	It("serves a stable /view projection for the root and a subtree", func() {
		ctx, cancel := context.WithTimeout(context.Background(), applyTimeout)
		defer cancel()

		By("GETting the root Snapshot /view (the shape d8 snapshot list renders)")
		rootView, err := getSnapshotView(ctx, "snapshots", testNamespace, snapshotName)
		Expect(err).NotTo(HaveOccurred(), "GET root /view")
		Expect(rootView.Version).To(Equal("v1"), "view version")
		Expect(rootView.Root.Kind).To(Equal("Snapshot"), "root kind")
		Expect(rootView.Root.Name).To(Equal(snapshotName), "root name")
		Expect(countViewNodes(&rootView.Root)).To(BeNumerically(">", 1), "root view is a tree")

		By("GETting a domain subtree /view rooted at the standalone DemoVirtualDiskSnapshot")
		diskSnapName, err := findDomainSnapshotName(ctx, demoDiskSnapshotGVR, testNamespace, standaloneDiskName)
		Expect(err).NotTo(HaveOccurred(), "find standalone disk snapshot")
		subView, err := getSnapshotView(ctx, demoDiskSnapshotGVR.Resource, testNamespace, diskSnapName)
		Expect(err).NotTo(HaveOccurred(), "GET subtree /view")
		Expect(subView.Version).To(Equal("v1"), "subtree view version")
		Expect(subView.Root.Kind).To(Equal("DemoVirtualDiskSnapshot"), "subtree root kind")
		Expect(subView.Root.Name).To(Equal(diskSnapName), "subtree root name")
		Expect(subView.Root.Children).To(BeEmpty(), "leaf subtree has no children")

		GinkgoWriter.Printf("root /view: %d node(s); subtree /view root: %s/%s\n",
			countViewNodes(&rootView.Root), subView.Root.Kind, subView.Root.Name)
	})

	It("round-trips a child snapshot import over plain REST (server-side re-root)", func() {
		ctx, cancel := context.WithTimeout(context.Background(), exportReadyTimeout+applyTimeout)
		defer cancel()

		By("re-discovering the standalone disk snapshot (the child to import)")
		diskSnapName, err := findDomainSnapshotName(ctx, demoDiskSnapshotGVR, testNamespace, standaloneDiskName)
		Expect(err).NotTo(HaveOccurred(), "find standalone disk snapshot")

		By("reading the parent bundle index blob + per-node manifests from the root export (download leg)")
		parentEntries, err := getExportSnapshots(ctx, testNamespace, snapshotExportName)
		Expect(err).NotTo(HaveOccurred())
		indexURL, err := getStatusString(ctx, snapshotExportGVR, testNamespace, snapshotExportName, "indexURL")
		Expect(err).NotTo(HaveOccurred())
		rawIndex, code, err := suiteAPI.get(ctx, indexURL)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(200), "GET parent indexURL")
		parentManifests := map[string][]byte{}
		for _, e := range parentEntries {
			id, _ := e["snapshotID"].(string)
			murl, _ := e["manifestsURL"].(string)
			raw, mc, merr := suiteAPI.get(ctx, murl)
			Expect(merr).NotTo(HaveOccurred(), "GET parent manifests %s", id)
			Expect(mc).To(Equal(200))
			parentManifests[id] = raw
		}

		By("creating a SnapshotImport selecting the child via spec.childSnapshot + a fresh targetName")
		Expect(applyYAML(ctx, snapshotImportManifest(testNamespace, importTargetName, "DemoVirtualDiskSnapshot", diskSnapName), testNamespace)).
			To(Succeed(), "apply SnapshotImport")

		By("uploading the full parent index blob as-is (the server re-roots it)")
		idxUploadURL, err := waitStatusString(ctx, snapshotImportGVR, testNamespace, importName, "indexUploadURL", exportReadyTimeout)
		Expect(err).NotTo(HaveOccurred())
		Expect(suiteAPI.putBlob(ctx, idxUploadURL, rawIndex, true)).To(Succeed(), "upload index blob")

		By("waiting for the server to re-root and publish the subtree status.snapshots[]")
		importEntries, err := waitImportSnapshots(ctx, testNamespace, importName, exportReadyTimeout)
		Expect(err).NotTo(HaveOccurred())
		Expect(importEntries).To(HaveLen(1), "re-root scoped to the single leaf child")

		By("uploading each subtree node's manifests (dataless subtree: no volume data upload)")
		for _, e := range importEntries {
			id, _ := e["snapshotID"].(string)
			Expect(e["uploadURL"]).To(BeNil(), "dataless subtree node has no data upload endpoint: %s", id)
			murl, _ := e["manifestsUploadURL"].(string)
			Expect(murl).NotTo(BeEmpty(), "node %s manifestsUploadURL", id)
			manifest, ok := parentManifests[id]
			Expect(ok).To(BeTrue(), "parent bundle carries manifests for re-rooted node %s", id)
			Expect(suiteAPI.putBlob(ctx, murl, manifest, false)).To(Succeed(), "upload manifests for %s", id)
		}

		By("committing the top-level manifests (flips ManifestsReceived)")
		manifestsUploadURL, err := getStatusString(ctx, snapshotImportGVR, testNamespace, importName, "manifestsUploadURL")
		Expect(err).NotTo(HaveOccurred())
		Expect(manifestsUploadURL).NotTo(BeEmpty(), "manifestsUploadURL published")
		Expect(suiteAPI.commit(ctx, manifestsUploadURL)).To(Succeed(), "commit manifests")

		By("waiting for the import to become Ready")
		Expect(waitCRCondition(ctx, snapshotImportGVR, testNamespace, importName, condTypeReady, condStatusTrue, exportReadyTimeout)).
			To(Succeed(), "SnapshotImport Ready=True")
		_, reason, found, err := getCRCondition(ctx, snapshotImportGVR, testNamespace, importName, condTypeReady)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(reason).To(Equal(reasonImported), "Ready reason is Imported (no NameConflict)")

		By("asserting only the re-rooted child was recreated under targetName")
		_, err = getResource(ctx, demoDiskSnapshotGVR, testNamespace, importTargetName)
		Expect(err).NotTo(HaveOccurred(), "recreated DemoVirtualDiskSnapshot %q exists", importTargetName)

		GinkgoWriter.Printf("import %s/%s re-rooted to %d node(s), recreated %s/%s\n",
			testNamespace, importName, len(importEntries), "DemoVirtualDiskSnapshot", importTargetName)
	})
})

// --- manifests -------------------------------------------------------------

func csdManifest() string {
	return fmt.Sprintf(`apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: CustomSnapshotDefinition
metadata:
  name: %s
spec:
  snapshotResourceMapping:
    - source:
        apiVersion: demo.state-snapshotter.deckhouse.io/v1alpha1
        kind: DemoVirtualMachine
      snapshot:
        apiVersion: demo.state-snapshotter.deckhouse.io/v1alpha1
        kind: DemoVirtualMachineSnapshot
      priority: 100
    - source:
        apiVersion: demo.state-snapshotter.deckhouse.io/v1alpha1
        kind: DemoVirtualDisk
      snapshot:
        apiVersion: demo.state-snapshotter.deckhouse.io/v1alpha1
        kind: DemoVirtualDiskSnapshot
      priority: 10
`, csdName)
}

func appWorkloadManifest(namespace, storageClass, image, size string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    %[3]s: %[4]s
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: %[5]s
  resources:
    requests:
      storage: %[6]s
---
apiVersion: v1
kind: Pod
metadata:
  name: %[7]s
  namespace: %[2]s
  labels:
    %[3]s: %[4]s
spec:
  restartPolicy: Never
  # Pod Security Standards "restricted"-compliant context so the pod admits on
  # hardened Deckhouse namespaces; fsGroup makes the freshly-provisioned volume
  # group-writable for the non-root container.
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: app
      image: %[8]s
      command: ["sh", "-c", "echo snap-e2e > /data/probe.txt && sleep 3600"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["ALL"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: %[1]s
`, appPVCName, namespace, appLabelKey, appLabelValue, storageClass, size, appPodName, image)
}

// namespacePSSManifest relabels the test namespace to the privileged Pod Security level so the
// raw-device block pod (which must run as root to open the mapped device) admits. Applied as a
// create-or-update of the existing namespace object.
func namespacePSSManifest(namespace string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
  labels:
    pod-security.kubernetes.io/enforce: privileged
    pod-security.kubernetes.io/warn: privileged
    pod-security.kubernetes.io/audit: privileged
`, namespace)
}

// blockWorkloadManifest provisions a Block-mode PVC and a root pod that maps it as a raw device and
// writes a known 16-byte signature at offset 0 (then syncs and idles). The signature lets the export
// verify a byte-exact block round-trip over /api/v1/block.
func blockWorkloadManifest(namespace, storageClass, image, size string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Block
  storageClassName: %[3]s
  resources:
    requests:
      storage: %[4]s
---
apiVersion: v1
kind: Pod
metadata:
  name: %[5]s
  namespace: %[2]s
spec:
  restartPolicy: Never
  securityContext:
    runAsUser: 0
    runAsGroup: 0
  containers:
    - name: block
      image: %[6]s
      command: ["sh", "-c", "printf '%%s' '%[7]s' | dd of=%[8]s bs=1 count=16 conv=notrunc 2>/dev/null; sync; sleep 3600"]
      volumeDevices:
        - name: data
          devicePath: %[8]s
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: %[1]s
`, blockPVCName, namespace, storageClass, size, blockPodName, image, blockMagic, blockDevicePath)
}

func demoDomainManifest(namespace string) string {
	return fmt.Sprintf(`apiVersion: demo.state-snapshotter.deckhouse.io/v1alpha1
kind: DemoVirtualMachine
metadata:
  name: %[1]s
  namespace: %[4]s
spec:
  virtualDiskName: %[2]s
---
apiVersion: demo.state-snapshotter.deckhouse.io/v1alpha1
kind: DemoVirtualDisk
metadata:
  name: %[2]s
  namespace: %[4]s
spec: {}
---
apiVersion: demo.state-snapshotter.deckhouse.io/v1alpha1
kind: DemoVirtualDisk
metadata:
  name: %[3]s
  namespace: %[4]s
spec: {}
`, vmName, ownedDiskName, standaloneDiskName, namespace)
}

// subtreeExportManifest exports a subtree rooted at a domain snapshot CR (a DemoVirtualDiskSnapshot)
// via a typed snapshotRef, rather than the namespace-root Snapshot.
func subtreeExportManifest(namespace, diskSnapshotName string) string {
	return fmt.Sprintf(`apiVersion: storage.deckhouse.io/v1alpha1
kind: SnapshotExport
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  snapshotRef:
    apiVersion: demo.state-snapshotter.deckhouse.io/v1alpha1
    kind: DemoVirtualDiskSnapshot
    name: %[3]s
  ttl: 24h
`, subtreeExportName, namespace, diskSnapshotName)
}

// snapshotImportManifest imports a single child snapshot (server-side re-root) from an uploaded parent
// bundle, recreating it under targetName. childName is the captured domain snapshot object to re-root.
func snapshotImportManifest(namespace, targetName, childKind, childName string) string {
	return fmt.Sprintf(`apiVersion: storage.deckhouse.io/v1alpha1
kind: SnapshotImport
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  targetName: %[3]s
  childSnapshot:
    apiVersion: %[4]s
    kind: %[5]s
    name: %[6]s
  ttl: 24h
`, importName, namespace, targetName, demoAPIVersion, childKind, childName)
}

func snapshotManifest(namespace string) string {
	return fmt.Sprintf(`apiVersion: storage.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: %s
  namespace: %s
spec: {}
`, snapshotName, namespace)
}

func snapshotExportManifest(namespace string) string {
	// ttl is required by the SnapshotExport CRD and is propagated verbatim to each child DataExport.
	// Use a deliberately large idle TTL: e2e never downloads from the endpoints, so a short TTL would
	// let the real SVDM DataExport idle out and flip the export to Expired before the Ready check.
	return fmt.Sprintf(`apiVersion: storage.deckhouse.io/v1alpha1
kind: SnapshotExport
metadata:
  name: %s
  namespace: %s
spec:
  snapshotRef:
    name: %s
  ttl: 24h
`, snapshotExportName, namespace, snapshotName)
}
