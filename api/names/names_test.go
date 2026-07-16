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

package names

import (
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

// dns1123Subdomain is the k8s object-name charset (lowercase alphanumerics, '-', '.'); all generated names
// must match and stay well under the 253-char limit.
var dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

func assertDNS1123(t *testing.T, name string) {
	t.Helper()
	if !dns1123Subdomain.MatchString(name) {
		t.Fatalf("name %q is not DNS-1123 safe", name)
	}
	if len(name) > 253 {
		t.Fatalf("name %q exceeds 253 chars (%d)", name, len(name))
	}
}

func TestHashWidths(t *testing.T) {
	if got := h8("x"); len(got) != 8 {
		t.Fatalf("h8 width = %d, want 8 (%q)", len(got), got)
	}
	if got := h16("x"); len(got) != 16 {
		t.Fatalf("h16 width = %d, want 16 (%q)", len(got), got)
	}
	// h8 must be the prefix of h16 (same sha256, different truncation).
	if !strings.HasPrefix(h16("x"), h8("x")) {
		t.Fatalf("h8 must be a prefix of h16 for the same input")
	}
}

func TestGeneratorsAreDeterministicAndDNS1123(t *testing.T) {
	uid := types.UID("11111111-2222-3333-4444-555555555555")
	uid2 := types.UID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")

	cases := map[string]string{
		"child":     ChildSnapshotName(uid, uid2),
		"content":   ContentName(uid),
		"mcr":       ManifestCaptureRequestName(uid),
		"mcp":       ManifestCheckpointName(uid),
		"chunk":     ChunkName(uid, 7),
		"vs":        OrphanVolumeSnapshotName(uid, uid2),
		"vcr":       VolumeCaptureRequestName(uid),
		"ok":        ObjectKeeperName(uid),
		"import-ok": ImportManifestCheckpointObjectKeeperName(uid),
	}
	prefixes := map[string]string{
		"child":     "nss-snap-",
		"content":   "nss-content-",
		"mcr":       "nss-mcr-",
		"mcp":       "nss-mcp-",
		"chunk":     "nss-mcp-",
		"vs":        "nss-vs-",
		"vcr":       "nss-vcr-",
		"ok":        "nss-ok-",
		"import-ok": "nss-import-ok-",
	}
	for kind, name := range cases {
		assertDNS1123(t, name)
		if !strings.HasPrefix(name, prefixes[kind]) {
			t.Fatalf("%s name %q missing prefix %q", kind, name, prefixes[kind])
		}
	}

	// Determinism: same inputs -> same names.
	if ChildSnapshotName(uid, uid2) != cases["child"] {
		t.Fatalf("ChildSnapshotName not deterministic")
	}
	if ChunkName(uid, 7) != cases["chunk"] || ChunkName(uid, 8) == cases["chunk"] {
		t.Fatalf("ChunkName must depend on index")
	}
}

func TestChildSnapshotNameUniqueness(t *testing.T) {
	parentA := types.UID("parent-a")
	parentB := types.UID("parent-b")
	srcX := types.UID("source-x")
	srcY := types.UID("source-y")

	// (a) different sources under the same parent -> different names.
	if ChildSnapshotName(parentA, srcX) == ChildSnapshotName(parentA, srcY) {
		t.Fatalf("different sources must yield different child names")
	}
	// (b) same source under two parents (DAG) -> different names (h8(parentUID) differs).
	if ChildSnapshotName(parentA, srcX) == ChildSnapshotName(parentB, srcX) {
		t.Fatalf("same source under different parents must yield different child names")
	}
}

func TestImportManifestCheckpointObjectKeeperNoRootOKCollision(t *testing.T) {
	// The import-MCP ObjectKeeper and the snapshot's root ObjectKeeper are both keyed by the SAME snapshot
	// UID; they MUST NOT collide (an import root Snapshot owns both). The distinct nss-import-ok- prefix
	// guarantees it (design §10.1).
	uid := types.UID("import-root-uid")
	if ImportManifestCheckpointObjectKeeperName(uid) == ObjectKeeperName(uid) {
		t.Fatalf("import-MCP ObjectKeeper name must not collide with the root ObjectKeeper name for the same UID")
	}
	// Determinism + per-UID uniqueness.
	first := ImportManifestCheckpointObjectKeeperName(uid)
	second := ImportManifestCheckpointObjectKeeperName(uid)
	if first != second {
		t.Fatalf("ImportManifestCheckpointObjectKeeperName not deterministic: %q vs %q", first, second)
	}
	if ImportManifestCheckpointObjectKeeperName(uid) == ImportManifestCheckpointObjectKeeperName(types.UID("other-uid")) {
		t.Fatalf("distinct snapshot UIDs must yield distinct import-MCP ObjectKeeper names")
	}
}

func TestOrphanPerPVCMCRNoCollision(t *testing.T) {
	// Each orphan PVC gets its own VolumeSnapshot UID, and the per-PVC MCR is keyed by that VS UID, so two
	// orphan PVCs under the same root never collide.
	vsA := types.UID("orphan-vs-a")
	vsB := types.UID("orphan-vs-b")
	if ManifestCaptureRequestName(vsA) == ManifestCaptureRequestName(vsB) {
		t.Fatalf("per-PVC orphan MCR names must differ when keyed by distinct VS UIDs")
	}
}
