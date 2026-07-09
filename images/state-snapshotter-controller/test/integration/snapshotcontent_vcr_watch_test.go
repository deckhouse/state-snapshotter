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
)

// Phase 3 (vcr-watch-core-terminal): with the VolumeCaptureRequest CRD served by envtest, the content
// controller can dynamically register a VCR informer-watch on its already-built controller handle. This
// exercises the parts the fake-cache unit tests cannot: the real RESTMapping guard resolving the served
// GVK, and controller-runtime's source.Kind(realCache, ...) Watch succeeding on a running controller.
// The end-to-end "VCR status flip wakes the owning content" delivery is asserted behaviorally by the
// child_bridge e2e; here we assert the registration path and its idempotency.
var _ = Describe("Integration: SnapshotContentController - VolumeCaptureRequest watch", Serial, func() {
	It("registers the VCR watch against the served CRD and is idempotent", func() {
		if !integrationVolumeCaptureRequestAPIAvailable {
			Skip("VolumeCaptureRequest API not served by envtest; skipping VCR watch registration spec")
		}
		Expect(integrationContentController).NotTo(BeNil(), "content controller must be built in BeforeSuite")

		// First registration: RESTMapping guard passes (CRD served) and the watch is added to the running
		// content controller.
		Expect(integrationContentController.AddVolumeCaptureRequestWatch(mgr)).To(Succeed())
		// Repeat registration is a guarded no-op (vcrWatchAdded latch) — must not error or double-add.
		Expect(integrationContentController.AddVolumeCaptureRequestWatch(mgr)).To(Succeed())
	})
})
