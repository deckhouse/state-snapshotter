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

// Package snapshotarchivetest holds the golden test vectors of the snapshot archive format, so
// that every implementation — the snapshotarchive package itself, the d8 client, a backup
// product — can prove it produces byte-identical descriptors and digests.
//
// The vectors are:
//
//   - a small archive tree (TreeFS, rooted at TreeRoot) of five nodes at format version 3: a
//     namespace Snapshot root with a block VolumeSnapshot, a filesystem VolumeSnapshot and a domain
//     snapshot that has a VolumeSnapshot child of its own. Every node's snapshot.yaml carries its
//     three digests, and Expected lists them again per node. The root's manifests/ holds a
//     notes.yml that the content checksum does not cover;
//   - the same tree at format version 4 (ChildMetadataTreeFS, Expected.ChildMetadataNodes): the
//     same files and content checksums, with every childrenChecksum committing to the children's
//     metadataChecksum;
//   - tampered descriptors (TamperedFS, Expected.TamperedChildMetadata): a version 4 node of that
//     tree with a field outside its checksum and childrenChecksum edited and its metadataChecksum
//     recomputed. Each verifies on its own, and put in place of the node's snapshot.yaml it must
//     fail its parent's children checksum;
//   - stand-alone descriptors (Expected.Descriptors) pinning the canonical form: the example from
//     docs/BACKUP_INTEGRATION.md at format version 2, and a version 3 descriptor exercising string
//     escaping and the payload size fields. For each, CanonicalJSON is the exact byte string the
//     metadata checksum is taken over.
//
// The version 2 and 3 vectors were produced by the d8 client before the format code moved into
// snapshotarchive. The version 4 vectors were produced by snapshotarchive when that version was
// introduced, and their digests were recomputed independently of it from the format's definition.
// None of them may ever be regenerated to make a change pass (see the snapshotarchive package
// documentation).
package snapshotarchivetest

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// TreeRoot is the name of the root node directory inside TreeFS.
const TreeRoot = "namespace-snapshot"

//go:embed vectors
var vectors embed.FS

// ExpectedNode is the digests one node of the vector tree must have.
type ExpectedNode struct {
	// Path is the node directory relative to TreeRoot, "/"-separated; "." is the root.
	Path string `json:"path"`
	// Checksum, ChildrenChecksum and MetadataChecksum are the hex digests of the node.
	Checksum         string `json:"checksum"`
	ChildrenChecksum string `json:"childrenChecksum"`
	MetadataChecksum string `json:"metadataChecksum"`
}

// ExpectedDescriptor is one stand-alone descriptor vector.
type ExpectedDescriptor struct {
	// File is the descriptor's name in DescriptorsFS.
	File string `json:"file"`
	// FormatVersion is the format version the descriptor declares.
	FormatVersion int `json:"formatVersion"`
	// MetadataChecksum is the hex metadata digest the descriptor must have.
	MetadataChecksum string `json:"metadataChecksum"`
}

// ExpectedTampered is one tampered descriptor vector.
type ExpectedTampered struct {
	// File is the descriptor's name in TamperedFS.
	File string `json:"file"`
	// Node is the path, relative to TreeRoot in ChildMetadataTreeFS, of the node whose
	// snapshot.yaml the descriptor replaces.
	Node string `json:"node"`
	// FailingNode is the path of the node whose check must fail: the parent of Node.
	FailingNode string `json:"failingNode"`
	// Error names the snapshotarchive sentinel error the check fails with.
	Error string `json:"error"`
}

// Expected is the full set of expected values.
type Expected struct {
	// EmptyChildrenChecksum is the hex ChildrenChecksum of a node without children.
	EmptyChildrenChecksum string `json:"emptyChildrenChecksum"`
	// TreeRoot repeats the TreeRoot constant.
	TreeRoot string `json:"treeRoot"`
	// Nodes lists every node of the vector tree, sorted by Path.
	Nodes []ExpectedNode `json:"nodes"`
	// Descriptors lists the stand-alone descriptor vectors.
	Descriptors []ExpectedDescriptor `json:"descriptors"`
	// ChildMetadataNodes lists every node of the version 4 vector tree, sorted by Path.
	ChildMetadataNodes []ExpectedNode `json:"childMetadataNodes"`
	// TamperedChildMetadata lists the tampered descriptor vectors.
	TamperedChildMetadata []ExpectedTampered `json:"tamperedChildMetadata"`
}

// LoadExpected returns the expected values of all vectors.
func LoadExpected() (Expected, error) {
	data, err := vectors.ReadFile("vectors/expected.json")
	if err != nil {
		return Expected{}, fmt.Errorf("read expected vectors: %w", err)
	}

	var expected Expected
	if err := json.Unmarshal(data, &expected); err != nil {
		return Expected{}, fmt.Errorf("decode expected vectors: %w", err)
	}

	return expected, nil
}

// TreeFS returns the version 3 vector tree; its only top-level entry is the TreeRoot directory.
func TreeFS() fs.FS {
	return mustSub("vectors/tree")
}

// ChildMetadataTreeFS returns the version 4 vector tree; its only top-level entry is the TreeRoot
// directory.
func ChildMetadataTreeFS() fs.FS {
	return mustSub("vectors/tree-child-metadata")
}

// TamperedFS returns the tampered descriptor vectors, named as in Expected.TamperedChildMetadata.
func TamperedFS() fs.FS {
	return mustSub("vectors/tampered")
}

// DescriptorsFS returns the stand-alone descriptor vectors, named as in Expected.Descriptors.
func DescriptorsFS() fs.FS {
	return mustSub("vectors/descriptors")
}

// CanonicalJSON returns the canonical JSON bytes of a stand-alone descriptor vector: exactly what
// its metadata checksum is computed over.
func CanonicalJSON(descriptorFile string) ([]byte, error) {
	data, err := fs.ReadFile(mustSub("vectors/canonical"), path.Base(descriptorFile)+".json")
	if err != nil {
		return nil, fmt.Errorf("read canonical form of %s: %w", descriptorFile, err)
	}

	return data, nil
}

// CopyTree writes the version 3 vector tree under dir, so that implementations that work on a
// real directory can run against it. The root node ends up at filepath.Join(dir, TreeRoot).
func CopyTree(dir string) error {
	return copyTree(TreeFS(), dir)
}

// CopyChildMetadataTree writes the version 4 vector tree under dir, as CopyTree does.
func CopyChildMetadataTree(dir string) error {
	return copyTree(ChildMetadataTreeFS(), dir)
}

func copyTree(tree fs.FS, dir string) error {
	return fs.WalkDir(tree, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		target := filepath.Join(dir, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		data, err := fs.ReadFile(tree, name)
		if err != nil {
			return err
		}

		return os.WriteFile(target, data, 0o644)
	})
}

func mustSub(dir string) fs.FS {
	sub, err := fs.Sub(vectors, dir)
	if err != nil {
		// fs.Sub fails only on an invalid path, and dir is a constant.
		panic(err)
	}

	return sub
}
