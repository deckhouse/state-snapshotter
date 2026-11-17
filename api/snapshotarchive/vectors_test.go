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

package snapshotarchive_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deckhouse/state-snapshotter/api/snapshotarchive"
	"github.com/deckhouse/state-snapshotter/api/snapshotarchive/snapshotarchivetest"
)

// vectorNode is one node of the vector tree as read from snapshotarchivetest.TreeFS.
type vectorNode struct {
	dir      string
	raw      []byte
	tree     *snapshotarchive.TreeNode
	children []*vectorNode
}

// loadVectorTree reads the vector tree into a TreeNode tree, recomputing every node's content
// checksum from its files. The covered-file rule below is the layout rule of the format
// ("Computing the digests" in docs/BACKUP_INTEGRATION.md), reduced to what the vector tree uses:
// manifests/*.yaml plus the single payload.
func loadVectorTree(t *testing.T, tree fs.FS, dir string) *vectorNode {
	t.Helper()

	raw, err := fs.ReadFile(tree, path.Join(dir, "snapshot.yaml"))
	if err != nil {
		t.Fatalf("read %s/snapshot.yaml: %v", dir, err)
	}

	descriptor, err := snapshotarchive.UnmarshalDescriptor(raw, snapshotarchive.ReadOptions{})
	if err != nil {
		t.Fatalf("unmarshal %s/snapshot.yaml: %v", dir, err)
	}

	var files []snapshotarchive.FileDigest

	addFile := func(rel string) {
		data, err := fs.ReadFile(tree, path.Join(dir, rel))
		if err != nil {
			t.Fatalf("read %s/%s: %v", dir, rel, err)
		}

		digest, err := snapshotarchive.ComputeFileDigest(rel, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("digest %s/%s: %v", dir, rel, err)
		}

		files = append(files, digest)
	}

	manifests, err := fs.ReadDir(tree, path.Join(dir, "manifests"))
	if err != nil {
		t.Fatalf("read %s/manifests: %v", dir, err)
	}

	for _, entry := range manifests {
		if strings.HasSuffix(entry.Name(), ".yaml") {
			addFile("manifests/" + entry.Name())
		}
	}

	node := &vectorNode{dir: dir, raw: raw, tree: &snapshotarchive.TreeNode{Descriptor: descriptor}}

	entries, err := fs.ReadDir(tree, dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	for _, entry := range entries {
		switch entry.Name() {
		case "data.bin":
			node.tree.HasBlockData = true

			addFile(entry.Name())
		case "data.tar":
			node.tree.HasFilesystemData = true

			addFile(entry.Name())
		case "snapshots":
			children, err := fs.ReadDir(tree, path.Join(dir, "snapshots"))
			if err != nil {
				t.Fatalf("read %s/snapshots: %v", dir, err)
			}

			for _, child := range children {
				childNode := loadVectorTree(t, tree, path.Join(dir, "snapshots", child.Name()))
				node.children = append(node.children, childNode)
				node.tree.Children = append(node.tree.Children, childNode.tree)
			}
		}
	}

	content, err := snapshotarchive.ComputeContentChecksum(files)
	if err != nil {
		t.Fatalf("content checksum of %s: %v", dir, err)
	}

	node.tree.ContentChecksum = &content

	return node
}

func flattenVectorTree(node *vectorNode, out map[string]*vectorNode) {
	rel := strings.TrimPrefix(strings.TrimPrefix(node.dir, snapshotarchivetest.TreeRoot), "/")
	if rel == "" {
		rel = "."
	}

	out[rel] = node

	for _, child := range node.children {
		flattenVectorTree(child, out)
	}
}

func TestGoldenVectorTreeDigests(t *testing.T) {
	expected, err := snapshotarchivetest.LoadExpected()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("version 3", func(t *testing.T) {
		checkVectorTreeDigests(t, snapshotarchivetest.TreeFS(), snapshotarchive.FormatVersionPayloadSizes, expected.Nodes)
	})
	t.Run("version 4", func(t *testing.T) {
		checkVectorTreeDigests(t, snapshotarchivetest.ChildMetadataTreeFS(), snapshotarchive.FormatVersionChildMetadata,
			expected.ChildMetadataNodes)
	})
}

// checkVectorTreeDigests validates the vector tree in tree and compares every node's recorded and
// recomputed digests, and its format version, with want.
func checkVectorTreeDigests(t *testing.T, tree fs.FS, version int, want []snapshotarchivetest.ExpectedNode) {
	t.Helper()

	root := loadVectorTree(t, tree, snapshotarchivetest.TreeRoot)

	if err := snapshotarchive.ValidateTree(root.tree, snapshotarchive.ReadOptions{}); err != nil {
		t.Fatalf("ValidateTree on the vector tree: %v", err)
	}

	nodes := map[string]*vectorNode{}
	flattenVectorTree(root, nodes)

	if len(nodes) != len(want) {
		t.Fatalf("vector tree has %d nodes, expected.json lists %d", len(nodes), len(want))
	}

	for _, want := range want {
		node, ok := nodes[want.Path]
		if !ok {
			t.Fatalf("expected node %q is not in the vector tree", want.Path)
		}

		d := node.tree.Descriptor
		if d.FormatVersion != version {
			t.Errorf("%s: formatVersion %d, want %d", want.Path, d.FormatVersion, version)
		}

		if got := node.tree.ContentChecksum.Hex; got != want.Checksum {
			t.Errorf("%s: recomputed checksum %s, want %s", want.Path, got, want.Checksum)
		}

		if d.Checksum.Hex != want.Checksum || d.ChildrenChecksum.Hex != want.ChildrenChecksum ||
			d.MetadataChecksum.Hex != want.MetadataChecksum {
			t.Errorf("%s: stored digests %s/%s/%s, want %s/%s/%s", want.Path,
				d.Checksum.Hex, d.ChildrenChecksum.Hex, d.MetadataChecksum.Hex,
				want.Checksum, want.ChildrenChecksum, want.MetadataChecksum)
		}

		metadata, err := snapshotarchive.ComputeMetadataChecksum(d)
		if err != nil {
			t.Fatalf("%s: ComputeMetadataChecksum: %v", want.Path, err)
		}

		if metadata.Hex != want.MetadataChecksum {
			t.Errorf("%s: recomputed metadataChecksum %s, want %s", want.Path, metadata.Hex, want.MetadataChecksum)
		}
	}
}

// TestGoldenVectorTreeSealReproducesBytes strips every digest from each vector tree, seals it
// again bottom up at the tree's format version and requires every snapshot.yaml to come out byte
// for byte as stored.
func TestGoldenVectorTreeSealReproducesBytes(t *testing.T) {
	tests := []struct {
		name string
		tree fs.FS
		seal func(root *snapshotarchive.TreeNode) error
	}{
		{
			name: "version 3",
			tree: snapshotarchivetest.TreeFS(),
			seal: func(root *snapshotarchive.TreeNode) error {
				return snapshotarchive.SealTreeAt(root, snapshotarchive.FormatVersionPayloadSizes)
			},
		},
		{name: "version 4 with SealTree", tree: snapshotarchivetest.ChildMetadataTreeFS(), seal: snapshotarchive.SealTree},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := loadVectorTree(t, tt.tree, snapshotarchivetest.TreeRoot)

			var strip func(node *vectorNode)
			strip = func(node *vectorNode) {
				node.tree.Descriptor.FormatVersion = 0
				node.tree.Descriptor.Checksum = snapshotarchive.NodeChecksum{}
				node.tree.Descriptor.ChildrenChecksum = nil
				node.tree.Descriptor.MetadataChecksum = nil

				for _, child := range node.children {
					strip(child)
				}
			}
			strip(root)

			if err := tt.seal(root.tree); err != nil {
				t.Fatalf("seal: %v", err)
			}

			nodes := map[string]*vectorNode{}
			flattenVectorTree(root, nodes)

			for rel, node := range nodes {
				got, err := snapshotarchive.MarshalDescriptor(node.tree.Descriptor)
				if err != nil {
					t.Fatalf("%s: MarshalDescriptor: %v", rel, err)
				}

				if !bytes.Equal(got, node.raw) {
					t.Errorf("%s: sealed snapshot.yaml differs from the vector\n got:\n%s\nwant:\n%s", rel, got, node.raw)
				}
			}
		})
	}
}

// TestGoldenVectorTamperedChildMetadata puts each tampered descriptor in place of its node in the
// version 4 vector tree and requires the tree to fail at the node's parent, while the descriptor
// alone still decodes and validates.
func TestGoldenVectorTamperedChildMetadata(t *testing.T) {
	expected, err := snapshotarchivetest.LoadExpected()
	if err != nil {
		t.Fatal(err)
	}

	if len(expected.TamperedChildMetadata) == 0 {
		t.Fatal("expected.json lists no tampered descriptor vectors")
	}

	sentinels := map[string]error{"ErrChildrenChecksumMismatch": snapshotarchive.ErrChildrenChecksumMismatch}

	for _, want := range expected.TamperedChildMetadata {
		t.Run(want.File, func(t *testing.T) {
			wantErr, ok := sentinels[want.Error]
			if !ok {
				t.Fatalf("unknown error %q", want.Error)
			}

			raw, err := fs.ReadFile(snapshotarchivetest.TamperedFS(), want.File)
			if err != nil {
				t.Fatal(err)
			}

			tampered, err := snapshotarchive.UnmarshalDescriptor(raw, snapshotarchive.ReadOptions{})
			if err != nil {
				t.Fatalf("the tampered descriptor must decode on its own: %v", err)
			}

			root := loadVectorTree(t, snapshotarchivetest.ChildMetadataTreeFS(), snapshotarchivetest.TreeRoot)

			nodes := map[string]*vectorNode{}
			flattenVectorTree(root, nodes)

			node, ok := nodes[want.Node]
			if !ok {
				t.Fatalf("node %q is not in the vector tree", want.Node)
			}

			if _, ok := nodes[want.FailingNode]; !ok {
				t.Fatalf("failing node %q is not in the vector tree", want.FailingNode)
			}

			if err := snapshotarchive.ValidateDescriptor(tampered, node.tree.HasBlockData, node.tree.HasFilesystemData,
				snapshotarchive.ReadOptions{}); err != nil {
				t.Fatalf("the tampered descriptor must validate on its own: %v", err)
			}

			node.tree.Descriptor = tampered

			err = snapshotarchive.ValidateTree(root.tree, snapshotarchive.ReadOptions{})
			if !errors.Is(err, wantErr) {
				t.Fatalf("ValidateTree error = %v, want %v", err, wantErr)
			}

			if prefix := kindNamePath(nodes, want.FailingNode) + ": "; !strings.HasPrefix(err.Error(), prefix) {
				t.Errorf("error %q does not name the node %q", err, want.FailingNode)
			}
		})
	}
}

// kindNamePath renders the node at rel as ValidateTree names it: the kind/name of every node from
// the root down to it, joined with " > ".
func kindNamePath(nodes map[string]*vectorNode, rel string) string {
	var elements []string

	for current := rel; ; current = path.Dir(path.Dir(current)) {
		d := nodes[current].tree.Descriptor
		elements = append([]string{d.Kind + "/" + d.Name}, elements...)

		if current == "." {
			return strings.Join(elements, " > ")
		}
	}
}

func TestGoldenVectorDescriptors(t *testing.T) {
	expected, err := snapshotarchivetest.LoadExpected()
	if err != nil {
		t.Fatal(err)
	}

	if len(expected.Descriptors) == 0 {
		t.Fatal("expected.json lists no descriptor vectors")
	}

	for _, want := range expected.Descriptors {
		t.Run(want.File, func(t *testing.T) {
			raw, err := fs.ReadFile(snapshotarchivetest.DescriptorsFS(), want.File)
			if err != nil {
				t.Fatal(err)
			}

			d, err := snapshotarchive.UnmarshalDescriptor(raw, snapshotarchive.ReadOptions{})
			if err != nil {
				t.Fatalf("UnmarshalDescriptor: %v", err)
			}

			if d.FormatVersion != want.FormatVersion {
				t.Errorf("formatVersion %d, want %d", d.FormatVersion, want.FormatVersion)
			}

			metadata, err := snapshotarchive.ComputeMetadataChecksum(d)
			if err != nil {
				t.Fatal(err)
			}

			if metadata.Hex != want.MetadataChecksum {
				t.Errorf("metadataChecksum %s, want %s", metadata.Hex, want.MetadataChecksum)
			}

			canonical, err := snapshotarchivetest.CanonicalJSON(want.File)
			if err != nil {
				t.Fatal(err)
			}

			sum := sha256.Sum256(canonical)
			if hex.EncodeToString(sum[:]) != want.MetadataChecksum {
				t.Errorf("sha256 of the canonical JSON vector is %x, want %s", sum, want.MetadataChecksum)
			}
		})
	}
}

func TestGoldenEmptyChildrenChecksum(t *testing.T) {
	expected, err := snapshotarchivetest.LoadExpected()
	if err != nil {
		t.Fatal(err)
	}

	if got := snapshotarchive.EmptyChildrenChecksum().Hex; got != expected.EmptyChildrenChecksum {
		t.Errorf("EmptyChildrenChecksum %s, want %s", got, expected.EmptyChildrenChecksum)
	}
}

func TestCopyTreeMaterializesVectorTree(t *testing.T) {
	tests := []struct {
		name string
		tree fs.FS
		copy func(dir string) error
	}{
		{name: "version 3", tree: snapshotarchivetest.TreeFS(), copy: snapshotarchivetest.CopyTree},
		{name: "version 4", tree: snapshotarchivetest.ChildMetadataTreeFS(), copy: snapshotarchivetest.CopyChildMetadataTree},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := tt.copy(dir); err != nil {
				t.Fatalf("copy the tree: %v", err)
			}

			var count int

			err := fs.WalkDir(tt.tree, ".", func(name string, entry fs.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}

				want, err := fs.ReadFile(tt.tree, name)
				if err != nil {
					return err
				}

				got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
				if err != nil {
					return err
				}

				if !bytes.Equal(got, want) {
					t.Errorf("%s: copied bytes differ from the vector", name)
				}

				count++

				return nil
			})
			if err != nil {
				t.Fatal(err)
			}

			if count == 0 {
				t.Fatal("vector tree has no files")
			}
		})
	}
}
