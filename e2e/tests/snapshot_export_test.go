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
