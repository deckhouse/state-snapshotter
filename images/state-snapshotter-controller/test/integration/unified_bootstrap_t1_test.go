//go:build integration
// +build integration

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

package integration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// T1 (plan): optional module/domain snapshot CRDs (demo, VolumeSnapshotContent/Class) are NOT installed in
// this envtest cluster. The two BUILT-IN pairs — the core Snapshot and the CSI VolumeSnapshot — do resolve:
// Snapshot + SnapshotContent ship in this repo's crds/, and a minimal VolumeSnapshot CRD is installed by
// BeforeSuite (integrationInstallCSISnapshotCRDs). Controller wiring must tolerate the missing optional
// pairs and succeed; unresolved pairs register no watches.
var _ = Describe("Integration T1: unified bootstrap resolves built-in pairs, tolerates missing optional CRDs", func() {
	It("resolves both built-in pairs (Snapshot + VolumeSnapshot) whose CRDs are present", func() {
		pairs := unifiedbootstrap.DefaultGraphRegistryBuiltInPairs()
		Expect(pairs).NotTo(BeEmpty())

		available := unifiedbootstrap.ResolveAvailableUnifiedPairs(
			mgr.GetRESTMapper(),
			pairs,
			ctrl.Log,
		)
		// Both built-in pairs resolve: root Snapshot (repo CRDs) + CSI VolumeSnapshot (minimal CRD from
		// BeforeSuite). Each uses the common SnapshotContent.
		Expect(available).To(HaveLen(2))
		snapKinds := map[schema.GroupVersionKind]bool{}
		for _, p := range available {
			snapKinds[p.Snapshot] = true
			Expect(p.SnapshotContent).To(Equal(schema.GroupVersionKind{
				Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "SnapshotContent",
			}))
		}
		Expect(snapKinds).To(HaveKey(schema.GroupVersionKind{
			Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "Snapshot",
		}))
		Expect(snapKinds).To(HaveKey(schema.GroupVersionKind{
			Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot",
		}))

		snapshotController, err := controllers.NewGenericSnapshotBinderController(
			k8sClient,
			mgr.GetAPIReader(),
			scheme,
			testCfg,
			[]schema.GroupVersionKind{},
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(snapshotController.SetupWithManager(mgr)).To(Succeed())

		contentController, err := controllers.NewSnapshotContentController(
			k8sClient,
			mgr.GetAPIReader(),
			scheme,
			mgr.GetRESTMapper(),
			testCfg,
			[]schema.GroupVersionKind{},
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(contentController.SetupWithManager(mgr)).To(Succeed())
	})
})
