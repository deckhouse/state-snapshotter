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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Plural resource names of the demo snapshot kinds, as addressed on the aggregated subresource paths.
const (
	resDemoVMSnapshots   = "demovirtualmachinesnapshots"
	resDemoDiskSnapshots = "demovirtualdisksnapshots"
)

// findManifest returns the first object of the given kind/name in a decoded manifest array.
func findManifest(objs []unstructured.Unstructured, kind, name string) (*unstructured.Unstructured, bool) {
	for i := range objs {
		if objs[i].GetKind() == kind && objs[i].GetName() == name {
			return &objs[i], true
		}
	}
	return nil, false
}

// firstNodeOfKind returns the first walked child snapshot node of the requested kind.
func firstNodeOfKind(nodes []childRef, kind string) (childRef, bool) {
	for _, n := range nodes {
		if n.kind == kind {
			return n, true
		}
	}
	return childRef{}, false
}

// aggregatedApiSpecs registers the aggregated subresource read specs of the manifest-only flow: the
// per-node manifests-download surface (core + demo groups) and the apply-ready manifests-with-data-restoration.
func aggregatedApiSpecs() {
	Context("Aggregated subresource APIs", func() {
		var vmSnapshot childRef

		BeforeAll(func() {
			Expect(captured.namespace).NotTo(BeEmpty(), "capture phase must have run first")
			Expect(captured.rootContent).NotTo(BeEmpty(), "root SnapshotContent must be resolved")

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			nodes, err := walkSnapshotTree(ctx, captured.namespace, captured.rootSnap)
			Expect(err).NotTo(HaveOccurred())
			var ok bool
			vmSnapshot, ok = firstNodeOfKind(nodes, "DemoVirtualMachineSnapshot")
			Expect(ok).To(BeTrue(), "expected a DemoVirtualMachineSnapshot node")
		})

		It("serves the root Snapshot own-node manifests via manifests-download (core group)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			path := coreSnapshotSubPath(captured.namespace, captured.rootSnap, subManifestsDownload)
			body, err := aggGet(ctx, path, nil)
			Expect(err).NotTo(HaveOccurred(), "GET %s", path)

			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			_, found := findManifest(objs, "ConfigMap", srcConfigMapName)
			Expect(found).To(BeTrue(), "manifests-download should contain the source ConfigMap %s", srcConfigMapName)
		})

		It("serves a demo node's own manifests via the core generic manifests-download path", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			path := coreGenericSubPath(captured.namespace, resDemoVMSnapshots, vmSnapshot.name, subManifestsDownload)
			body, err := aggGet(ctx, path, nil)
			Expect(err).NotTo(HaveOccurred(), "GET %s", path)

			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			_, found := findManifest(objs, "DemoVirtualMachine", srcVMName)
			Expect(found).To(BeTrue(), "demo manifests-download should contain DemoVirtualMachine %s", srcVMName)
		})

		It("returns sanitized, apply-ready objects via manifests-with-data-restoration (core group)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			const targetNS = "restore-read-probe"
			path := coreSnapshotSubPath(captured.namespace, captured.rootSnap, subManifestsRestore)
			body, err := aggGet(ctx, path, map[string]string{"targetNamespace": targetNS})
			Expect(err).NotTo(HaveOccurred(), "GET %s", path)

			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())

			cm, found := findManifest(objs, "ConfigMap", srcConfigMapName)
			Expect(found).To(BeTrue(), "restore output should contain the ConfigMap")

			By("asserting the restore output is namespace-rewritten and sanitized")
			Expect(cm.GetNamespace()).To(Equal(targetNS), "namespace must be rewritten to the target")
			_, hasStatus := cm.Object["status"]
			Expect(hasStatus).To(BeFalse(), "restore output must strip status")
			_, hasManagedFields, _ := unstructured.NestedFieldNoCopy(cm.Object, "metadata", "managedFields")
			Expect(hasManagedFields).To(BeFalse(), "restore output must strip metadata.managedFields")

			By("asserting cluster-scoped objects (Namespace) are dropped from restore output")
			_, hasNS := findManifest(objs, "Namespace", captured.namespace)
			Expect(hasNS).To(BeFalse(), "cluster-scoped Namespace must not be in restore output")
		})

		It("serves a demo subtree via manifests-with-data-restoration (demo group)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			const targetNS = "restore-read-probe"
			path := demoSubPath(captured.namespace, resDemoVMSnapshots, vmSnapshot.name, subManifestsRestore)
			body, err := aggGet(ctx, path, map[string]string{"targetNamespace": targetNS})
			Expect(err).NotTo(HaveOccurred(), "GET %s", path)

			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			vm, found := findManifest(objs, "DemoVirtualMachine", srcVMName)
			Expect(found).To(BeTrue(), "demo restore subtree should contain DemoVirtualMachine %s", srcVMName)
			Expect(vm.GetNamespace()).To(Equal(targetNS), "demo restore output must be namespace-rewritten")
			// The manifest-only VM owns no DemoVirtualDisk in this tree (dataless disks are disallowed), so
			// the delegated subtree is just the VM's own manifest.
		})
	})
}
