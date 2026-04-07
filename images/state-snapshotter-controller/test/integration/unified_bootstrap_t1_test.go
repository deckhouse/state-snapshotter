//go:build integration
// +build integration

/*
Copyright 2025 Flant JSC

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
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// T1 (plan): production unified GVK pairs from DefaultDesiredUnifiedSnapshotPairs are not installed
// in this envtest cluster. Controller wiring must still succeed with zero watches.
var _ = Describe("Integration T1: unified bootstrap without optional snapshot CRDs", func() {
	It("filters out missing API types and registers zero-watch unified controllers", func() {
		pairs := unifiedbootstrap.DefaultDesiredUnifiedSnapshotPairs()
		Expect(pairs).NotTo(BeEmpty())

		snapshotGVKs, snapshotContentGVKs := unifiedbootstrap.ResolveAvailableUnifiedGVKPairs(
			mgr.GetRESTMapper(),
			pairs,
			ctrl.Log,
		)
		Expect(len(snapshotGVKs)).To(Equal(len(snapshotContentGVKs)))
		Expect(snapshotGVKs).To(BeEmpty(), "envtest does not expose production unified snapshot CRDs used by bootstrap")
		Expect(snapshotContentGVKs).To(BeEmpty())

		snapshotController, err := controllers.NewSnapshotController(
			k8sClient,
			mgr.GetAPIReader(),
			scheme,
			testCfg,
			snapshotGVKs,
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(snapshotController.SetupWithManager(mgr)).To(Succeed())

		contentController, err := controllers.NewSnapshotContentController(
			k8sClient,
			mgr.GetAPIReader(),
			scheme,
			mgr.GetRESTMapper(),
			testCfg,
			snapshotContentGVKs,
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(contentController.SetupWithManager(mgr)).To(Succeed())
	})
})
