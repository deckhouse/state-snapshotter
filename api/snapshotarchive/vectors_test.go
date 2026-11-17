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

	root := loadVectorTree(t, snapshotarchivetest.TreeFS(), snapshotarchivetest.TreeRoot)

	if err := snapshotarchive.ValidateTree(root.tree, snapshotarchive.ReadOptions{}); err != nil {
		t.Fatalf("ValidateTree on the vector tree: %v", err)
	}

	nodes := map[string]*vectorNode{}
	flattenVectorTree(root, nodes)

	if len(nodes) != len(expected.Nodes) {
		t.Fatalf("vector tree has %d nodes, expected.json lists %d", len(nodes), len(expected.Nodes))
	}

	for _, want := range expected.Nodes {
		node, ok := nodes[want.Path]
		if !ok {
			t.Fatalf("expected node %q is not in the vector tree", want.Path)
		}

		d := node.tree.Descriptor
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

// TestGoldenVectorTreeSealReproducesBytes strips every digest from the vector tree, seals it
// again bottom up and requires every snapshot.yaml to come out byte for byte as stored.
func TestGoldenVectorTreeSealReproducesBytes(t *testing.T) {
	root := loadVectorTree(t, snapshotarchivetest.TreeFS(), snapshotarchivetest.TreeRoot)

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

	if err := snapshotarchive.SealTree(root.tree); err != nil {
		t.Fatalf("SealTree: %v", err)
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
	dir := t.TempDir()
	if err := snapshotarchivetest.CopyTree(dir); err != nil {
		t.Fatalf("CopyTree: %v", err)
	}

	var count int

	err := fs.WalkDir(snapshotarchivetest.TreeFS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}

		want, err := fs.ReadFile(snapshotarchivetest.TreeFS(), name)
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
}
