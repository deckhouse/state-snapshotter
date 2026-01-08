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
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers"
)

var _ = Describe("Integration: Controller Registration", func() {
	// PHASE 2.0: Integration: Controller Registration
	//
	// This test verifies that unified snapshots controllers can be registered
	// with the same pattern that is used in production (main.go).
	// This ensures that the registration code in main.go is correct and will work in production.
	//
	// INTERFACE: controllers.NewSnapshotController, controllers.NewSnapshotContentController
	//
	// PRECONDITIONS:
	// - Manager is running (from BeforeSuite)
	// - Test CRDs are installed (TestSnapshot, TestSnapshotContent)
	//
	// ACTIONS:
	// 1. Create SnapshotController with production-like GVKs
	// 2. Create SnapshotContentController with production-like GVKs
	// 3. Setup controllers with manager (same pattern as main.go)
	//
	// EXPECTED BEHAVIOR:
	// - Controllers are created without errors
	// - Controllers are registered with manager without errors
	// - This verifies the registration pattern from main.go works correctly

	It("should register both controllers together (as in main.go)", func() {
		// Simulate the exact registration pattern from main.go
		// This test verifies that the registration code in main.go works correctly
		// Use TestSnapshot GVK (same pattern as production, but with test CRD)
		snapshotGVKs := []schema.GroupVersionKind{
			{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "TestSnapshot"},
			// In production (main.go), these would be:
			// {Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "Snapshot"},
			// {Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "NamespaceSnapshot"},
			// {Group: "snapshot.internal.virtualization.deckhouse.io", Version: "v1alpha1", Kind: "InternalVirtualizationVirtualMachineSnapshot"},
		}
		snapshotContentGVKs := []schema.GroupVersionKind{
			{Group: "test.deckhouse.io", Version: "v1alpha1", Kind: "TestSnapshotContent"},
			// In production (main.go), these would be:
			// {Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "SnapshotContent"},
			// {Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "NamespaceSnapshotContent"},
			// {Group: "snapshot.internal.virtualization.deckhouse.io", Version: "v1alpha1", Kind: "InternalVirtualizationVirtualMachineSnapshotContent"},
		}

		// Create SnapshotController (same pattern as main.go)
		snapshotCtrl, err := controllers.NewSnapshotController(
			k8sClient,
			mgr.GetAPIReader(),
			scheme,
			testCfg,
			snapshotGVKs,
		)
		Expect(err).NotTo(HaveOccurred(), "SnapshotController should be created without errors")
		Expect(snapshotCtrl).NotTo(BeNil(), "SnapshotController should not be nil")
		err = snapshotCtrl.SetupWithManager(mgr)
		Expect(err).NotTo(HaveOccurred(), "SnapshotController should be registered with manager without errors")

		// Create SnapshotContentController (same pattern as main.go)
		contentCtrl, err := controllers.NewSnapshotContentController(
			k8sClient,
			mgr.GetAPIReader(),
			scheme,
			testCfg,
			snapshotContentGVKs,
		)
		Expect(err).NotTo(HaveOccurred(), "SnapshotContentController should be created without errors")
		Expect(contentCtrl).NotTo(BeNil(), "SnapshotContentController should not be nil")
		err = contentCtrl.SetupWithManager(mgr)
		Expect(err).NotTo(HaveOccurred(), "SnapshotContentController should be registered with manager without errors")
	})
})
