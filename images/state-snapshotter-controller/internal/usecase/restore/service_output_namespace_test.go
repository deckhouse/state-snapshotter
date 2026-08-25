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

package restore

// WHERE the compiler addresses its output is a promise made to the client of
// manifests-with-data-restoration — there is no request parameter for it, so it is the namespace of the
// Snapshot addressed, always. That is pinned here rather than left to whoever reads the compiler next.
// What makes it worth a test of its own is how it fails when it breaks: the compiler
// sets the namespace of every object it emits, and unstructured.SetNamespace("") does not write an
// empty string — it REMOVES metadata.namespace. A namespaced object arriving without one is not
// refused; the API server fills in the namespace of whoever applies it, which for kubectl apply is
// whatever the current context points at, usually default. So the failure is not a rejected manifest a
// caller would notice, it is somebody else's captured namespace applied silently into the wrong one.
//
// Plain testing rather than a Ginkgo spec, matching every other test of this package: they exercise the
// compiler directly against a fake client, and the payloads are built inline instead of embedded
// because each is a two-field object asserted in the same function.

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestOutputIsAddressedIntoTheSnapshotsOwnNamespace pins WHICH of the two candidate namespaces the
// output gets. The captured object deliberately carries a namespace DIFFERENT from the
// snapshot's, because that is the case where the two answers diverge: the output is addressed into the
// namespace of the Snapshot that was asked, not into the namespace the manifest was captured from. A
// tree loaded back into a cluster is exactly that shape — its Snapshot lives where it was loaded, its
// captured manifests still name where they came from.
func TestOutputIsAddressedIntoTheSnapshotsOwnNamespace(t *testing.T) {
	const capturedNS = "captured-elsewhere-ns"
	svc := newRootNodeService(t, []map[string]interface{}{
		{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "app-config", "namespace": capturedNS},
		},
	}, nil)

	out, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "snap", SnapshotNamespace: "source-ns", Scope: ScopeNode,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	objs := decodeObjects(t, out)
	if len(objs) != 1 {
		t.Fatalf("got %d object(s), want the single seeded ConfigMap — with nothing returned the assertion below checks nothing", len(objs))
	}
	switch got := (&unstructured.Unstructured{Object: objs[0]}).GetNamespace(); got {
	case "source-ns":
	case "":
		t.Fatal("the compiled object has NO metadata.namespace — addressing into the snapshot's own namespace is gone. Such an object is not refused on apply: it lands in whatever namespace the applying client defaults to, so a captured namespace would be restored silently into the wrong one")
	case capturedNS:
		t.Fatalf("the object stayed in the namespace it was captured from (%q) instead of the addressed snapshot's own", capturedNS)
	default:
		t.Fatalf("compiled namespace = %q, want the addressed snapshot's own \"source-ns\"", got)
	}

	t.Logf("checked 1 compiled object, addressed from a capture namespace that differs from the snapshot's")
}
