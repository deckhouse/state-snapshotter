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

package unifiedbootstrap

import "testing"

// TestIsOutOfProcessDomainSnapshotKind_excludesRootSnapshot guards the restore-delegation regression:
// since wave5 the namespace-root "Snapshot" is a domain-CAPTURE kind, but its restore is served
// in-process by the core apiserver. The restore compiler must NOT treat the root as an out-of-process
// domain node (doing so delegates the root back to core's own endpoint — a self-recursion / HTTP 500).
func TestIsOutOfProcessDomainSnapshotKind_excludesRootSnapshot(t *testing.T) {
	rootKind := DefaultSnapshotPair().Snapshot.Kind

	if IsOutOfProcessDomainSnapshotKind(rootKind) {
		t.Fatalf("root %q must NOT be an out-of-process domain restore kind (it is compiled in-process)", rootKind)
	}
	// Sanity: the root is still a domain-CAPTURE kind — the two predicates intentionally diverge on it.
	if !IsDomainCaptureSnapshotKind(rootKind) {
		t.Fatalf("root %q is expected to remain a domain-capture kind (capture planned via the SDK)", rootKind)
	}

	for _, demo := range []string{"DemoVirtualDiskSnapshot", "DemoVirtualMachineSnapshot"} {
		if !IsOutOfProcessDomainSnapshotKind(demo) {
			t.Errorf("demo kind %q must be an out-of-process domain restore kind (delegated to the domain apiserver)", demo)
		}
	}

	if IsOutOfProcessDomainSnapshotKind("ConfigMap") {
		t.Errorf("a non-snapshot kind must not be an out-of-process domain restore kind")
	}
}
