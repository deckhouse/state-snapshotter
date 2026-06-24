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

// T1 (plan): CSI Snapshot and optional module snapshot CRDs are not installed in this envtest cluster.
// Snapshot + SnapshotContent ship with this repo and are present in crds/.
// Controller wiring must tolerate missing pairs and succeed; optional pairs register no watches.
var _ = Describe("Integration T1: unified bootstrap without optional snapshot CRDs", func() {
	It("filters out missing API types; envtest exposes Snapshot pair when repo CRDs load", func() {
		pairs := unifiedbootstrap.DefaultDesiredUnifiedSnapshotPairs()
		Expect(pairs).NotTo(BeEmpty())

		available := unifiedbootstrap.ResolveAvailableUnifiedPairs(
			mgr.GetRESTMapper(),
			pairs,
			ctrl.Log,
		)
		Expect(available).To(HaveLen(1))
		Expect(available[0].Snapshot).To(Equal(schema.GroupVersionKind{
			Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "Snapshot",
		}))
		Expect(available[0].SnapshotContent).To(Equal(schema.GroupVersionKind{
			Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "SnapshotContent",
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
