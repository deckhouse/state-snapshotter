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

// restoreSpecs registers the manifest-level restore specs of the manifest-only flow: read the apply-ready
// manifests for the captured root via manifests-with-data-restoration, apply them into a fresh namespace,
// and assert the source objects are recreated.
func restoreSpecs() {
	Context("Manifest-level restore", func() {
		var restoreNS string

		BeforeAll(func() {
			Expect(captured.namespace).NotTo(BeEmpty(), "capture phase must have run first")
			restoreNS = uniqueNS("restore")

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			Expect(ensureNamespace(ctx, restoreNS)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, restoreNS)
			})
		})

		It("restores the captured manifests into a fresh namespace", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			By("Reading apply-ready manifests for the restore namespace")
			path := coreSnapshotSubPath(captured.namespace, captured.rootSnap, subManifestsRestore)
			body, err := aggGet(ctx, path, map[string]string{"targetNamespace": restoreNS})
			Expect(err).NotTo(HaveOccurred(), "GET %s", path)

			objs, err := decodeManifestArray(body)
			Expect(err).NotTo(HaveOccurred())
			Expect(objs).NotTo(BeEmpty(), "restore output must not be empty")

			By("Applying the restored manifests")
			ptrs := make([]*unstructured.Unstructured, 0, len(objs))
			for i := range objs {
				ptrs = append(ptrs, &objs[i])
			}
			Expect(applyObjects(ctx, ptrs, restoreNS)).To(Succeed())

			By("Asserting the ConfigMap was recreated with its data")
			cm, err := getResource(ctx, configMapGVR, restoreNS, srcConfigMapName)
			Expect(err).NotTo(HaveOccurred(), "ConfigMap should be recreated")
			val, _, _ := unstructured.NestedString(cm.Object, "data", "demo")
			Expect(val).To(Equal("tree"))

			By("Asserting the manifest-only DemoVirtualMachine was recreated")
			// The manifest-only tree carries no DemoVirtualDisk (dataless disks are disallowed), so only the
			// VM domain node is expected back alongside the ConfigMap manifest leg.
			_, err = getResource(ctx, demoVMGVR, restoreNS, srcVMName)
			Expect(err).NotTo(HaveOccurred(), "DemoVirtualMachine should be recreated")
		})
	})
}
