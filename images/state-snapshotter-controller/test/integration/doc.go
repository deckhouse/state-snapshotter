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

// Package integration contains integration tests for unified snapshots controllers.
//
// These tests use controller-runtime test framework with envtest to test controller behavior
// through their public interfaces (Reconcile methods).
//
// Test categories:
// - GenericSnapshotBinderController: Create path, Deletion path (propagation), Consistency checks
// - SnapshotContentController: Finalizer management, Orphaning, Cascade deletion
//
// These tests verify controller behavior, not implementation details.
//
// Integration Test Rules (learned lessons):
//
// 1. Assertions MUST read objects using mgr.GetAPIReader()
//   - k8sClient uses cache and may return stale data
//   - Example: mgr.GetAPIReader().Get(ctx, key, obj) instead of k8sClient.Get(ctx, key, obj)
//
// 2. Reconcile MUST be triggered inside Eventually blocks
//   - Integration tests do not rely on watch-based reconciliation
//   - Example: Eventually(func() { _, _ = ctrl.Reconcile(ctx, req); checkState() })
//
// 3. Fresh objects MUST be created on each poll
//   - Never reuse pointers across Eventually iterations
//   - Example: freshObj := &unstructured.Unstructured{} inside Eventually
//
// 4. Deletion MUST be asserted via apierrors.IsNotFound
//   - GC may delete objects after finalizer removal (expected behavior)
//   - Example: Expect(apierrors.IsNotFound(err)).To(BeTrue())
//
// 5. Use explicit timeouts and intervals
//   - Example: Eventually(func() bool {...}, "10s", "100ms").Should(BeTrue())
package integration
