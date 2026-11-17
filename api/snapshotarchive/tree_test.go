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

package snapshotarchive

import (
	"errors"
	"strings"
	"testing"
)

// sealedTestTree builds a root with a block leaf and an aggregator that has a filesystem leaf, and
// seals it at FormatVersionCurrent.
func sealedTestTree(t *testing.T) *TreeNode {
	t.Helper()

	return sealedTestTreeAt(t, FormatVersionCurrent)
}

// sealedTestTreeAt is sealedTestTree sealed at formatVersion.
func sealedTestTreeAt(t *testing.T, formatVersion int) *TreeNode {
	t.Helper()

	content := func(seed string) *NodeChecksum {
		c := validChildChecksum(seed)

		return &c
	}

	blockLeaf := &TreeNode{
		Descriptor: Descriptor{
			APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "data-0", Namespace: "ns",
			Volumes: []VolumeInfo{validVolume(VolumeModeBlock)},
		},
		ContentChecksum: content("data-0"),
		HasBlockData:    true,
	}
	fsLeaf := &TreeNode{
		Descriptor: Descriptor{
			APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "files-0", Namespace: "ns",
			Volumes: []VolumeInfo{validVolume(VolumeModeFilesystem)},
		},
		ContentChecksum:   content("files-0"),
		HasFilesystemData: true,
	}
	aggregator := &TreeNode{
		Descriptor: Descriptor{
			APIVersion: "demo.deckhouse.io/v1alpha1", Kind: "DemoVirtualMachineSnapshot", Name: "vm", Namespace: "ns",
			SourceObjectRef: &SourceObjectRef{APIVersion: "demo.deckhouse.io/v1alpha1", Kind: "DemoVirtualMachine", Name: "vm"},
		},
		ContentChecksum: content("vm"),
		Children:        []*TreeNode{fsLeaf},
	}
	root := &TreeNode{
		Descriptor: Descriptor{
			APIVersion: "state-snapshotter.deckhouse.io/v1alpha1", Kind: "Snapshot", Name: "root", Namespace: "ns",
		},
		ContentChecksum: content("root"),
		Children:        []*TreeNode{blockLeaf, aggregator},
	}

	if err := sealTreeAt(root, formatVersion); err != nil {
		t.Fatalf("seal the tree at version %d: %v", formatVersion, err)
	}

	return root
}

// resealed returns node's descriptor changed by edit and sealed again at its own version, as
// someone able to rewrite the node's snapshot.yaml would do.
func resealed(node *TreeNode, edit func(d *Descriptor)) Descriptor {
	d := node.Descriptor
	edit(&d)

	sealed, err := sealAt(d, d.FormatVersion)
	if err != nil {
		panic(err)
	}

	return sealed
}

// childMetadataTampering lists edits of a child's descriptor fields that neither its Checksum nor
// its ChildrenChecksum covers, each resealed so that the child verifies on its own, with the node
// whose direct-child commitment catches it at FormatVersionChildMetadata.
var childMetadataTampering = []struct {
	name     string
	tamper   func(root *TreeNode)
	wantPath string
}{
	{
		name: "a volume's storageClassName",
		tamper: func(root *TreeNode) {
			leaf := root.Children[0]
			leaf.Descriptor = resealed(leaf, func(d *Descriptor) { d.Volumes[0].StorageClassName = "other" })
		},
		wantPath: "Snapshot/root",
	},
	{
		name: "a volume's size",
		tamper: func(root *TreeNode) {
			leaf := root.Children[1].Children[0]
			leaf.Descriptor = resealed(leaf, func(d *Descriptor) { d.Volumes[0].Size = "100Gi" })
		},
		wantPath: "Snapshot/root > DemoVirtualMachineSnapshot/vm",
	},
	{
		name: "a volume's target",
		tamper: func(root *TreeNode) {
			leaf := root.Children[0]
			leaf.Descriptor = resealed(leaf, func(d *Descriptor) { d.Volumes[0].Target.Name = "other" })
		},
		wantPath: "Snapshot/root",
	},
	{
		name: "a sourceObjectRef",
		tamper: func(root *TreeNode) {
			aggregator := root.Children[1]
			aggregator.Descriptor = resealed(aggregator, func(d *Descriptor) { d.SourceObjectRef.Name = "other" })
		},
		wantPath: "Snapshot/root",
	},
	{
		name: "a sourceName",
		tamper: func(root *TreeNode) {
			aggregator := root.Children[1]
			aggregator.Descriptor = resealed(aggregator, func(d *Descriptor) { d.SourceName = "other" })
		},
		wantPath: "Snapshot/root",
	},
}

func TestSealTreeThenValidateTree(t *testing.T) {
	t.Parallel()

	root := sealedTestTree(t)

	if err := ValidateTree(root, ReadOptions{}); err != nil {
		t.Fatalf("ValidateTree on a sealed tree: %v", err)
	}

	leaf := root.Children[0].Descriptor
	if leaf.ChildrenChecksum.Hex != EmptyChildrenChecksum().Hex {
		t.Errorf("leaf ChildrenChecksum = %s, want the empty commitment", leaf.ChildrenChecksum.Hex)
	}

	if leaf.Checksum != *root.Children[0].ContentChecksum {
		t.Errorf("leaf Checksum = %+v, want its ContentChecksum", leaf.Checksum)
	}

	// Every sealed descriptor survives an encode/decode cycle unchanged.
	var check func(node *TreeNode)
	check = func(node *TreeNode) {
		data, err := MarshalDescriptor(node.Descriptor)
		if err != nil {
			t.Fatal(err)
		}

		decoded, err := UnmarshalDescriptor(data, ReadOptions{})
		if err != nil {
			t.Fatalf("%s: %v", node.Descriptor.Name, err)
		}

		if decoded.MetadataChecksum.Hex != node.Descriptor.MetadataChecksum.Hex {
			t.Errorf("%s: metadata checksum changed across encode/decode", node.Descriptor.Name)
		}

		for _, child := range node.Children {
			check(child)
		}
	}
	check(root)
}

func TestValidateTreeDetectsTampering(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		tamper   func(root *TreeNode)
		wantErr  error
		wantPath string
	}{
		{
			name: "content differs from the recorded checksum",
			tamper: func(root *TreeNode) {
				changed := validChildChecksum("changed")
				root.Children[1].Children[0].ContentChecksum = &changed
			},
			wantErr:  ErrChecksumMismatch,
			wantPath: "Snapshot/root > DemoVirtualMachineSnapshot/vm > VolumeSnapshot/files-0",
		},
		{
			name:     "a child is removed",
			tamper:   func(root *TreeNode) { root.Children = root.Children[:1] },
			wantErr:  ErrChildrenChecksumMismatch,
			wantPath: "Snapshot/root",
		},
		{
			name: "a child is added",
			tamper: func(root *TreeNode) {
				root.Children[1].Children = append(root.Children[1].Children, root.Children[0])
			},
			wantErr:  ErrChildrenChecksumMismatch,
			wantPath: "Snapshot/root > DemoVirtualMachineSnapshot/vm",
		},
		{
			name: "a child's descriptor is resealed with other content",
			tamper: func(root *TreeNode) {
				leaf := root.Children[0]
				changed := validChildChecksum("swapped")
				leaf.ContentChecksum = &changed
				leaf.Descriptor.Checksum = changed

				sealed, err := Seal(leaf.Descriptor)
				if err != nil {
					panic(err)
				}

				leaf.Descriptor = sealed
			},
			wantErr:  ErrChildrenChecksumMismatch,
			wantPath: "Snapshot/root",
		},
		{
			name: "a child's volume entry is resealed",
			tamper: func(root *TreeNode) {
				leaf := root.Children[0]
				leaf.Descriptor = resealed(leaf, func(d *Descriptor) { d.Volumes[0].Size = "100Gi" })
			},
			wantErr:  ErrChildrenChecksumMismatch,
			wantPath: "Snapshot/root",
		},
		{
			name:     "a descriptor field is edited without resealing",
			tamper:   func(root *TreeNode) { root.Children[1].Descriptor.SourceName = "edited" },
			wantErr:  ErrMetadataChecksumMismatch,
			wantPath: "Snapshot/root > DemoVirtualMachineSnapshot/vm",
		},
		{
			name:     "a payload flag disagrees with the volumes",
			tamper:   func(root *TreeNode) { root.Children[0].HasBlockData = false },
			wantErr:  ErrInvalidDescriptor,
			wantPath: "Snapshot/root > VolumeSnapshot/data-0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := sealedTestTree(t)
			tt.tamper(root)

			err := ValidateTree(root, ReadOptions{})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateTree error = %v, want %v", err, tt.wantErr)
			}

			if !strings.HasPrefix(err.Error(), tt.wantPath+": ") {
				t.Errorf("error %q does not name the node %q", err, tt.wantPath)
			}
		})
	}
}

// TestValidateTreeChildMetadataCommitment proves that a parent at FormatVersionChildMetadata
// catches a resealed edit of its child's metadata, and that a tree of an older version, verified
// with that version's encoding, does not: the older commitment leaves those fields to the child's
// own metadata checksum.
func TestValidateTreeChildMetadataCommitment(t *testing.T) {
	t.Parallel()

	for _, tt := range childMetadataTampering {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := sealedTestTree(t)
			tt.tamper(root)

			err := ValidateTree(root, ReadOptions{})
			if !errors.Is(err, ErrChildrenChecksumMismatch) {
				t.Fatalf("ValidateTree error = %v, want ErrChildrenChecksumMismatch", err)
			}

			if !strings.HasPrefix(err.Error(), tt.wantPath+": ") {
				t.Errorf("error %q does not name the node %q", err, tt.wantPath)
			}

			for _, version := range []int{FormatVersionAuthenticatedChildren, FormatVersionPayloadSizes} {
				old := sealedTestTreeAt(t, version)
				tt.tamper(old)

				if err := ValidateTree(old, ReadOptions{}); err != nil {
					t.Errorf("version %d tree must verify as before: %v", version, err)
				}
			}
		})
	}
}

// TestValidateTreeMixedVersions pins how the parent's version decides the commitment of a tree
// whose nodes are at different versions.
func TestValidateTreeMixedVersions(t *testing.T) {
	t.Parallel()

	t.Run("a new parent refuses an older child", func(t *testing.T) {
		t.Parallel()

		root := sealedTestTreeAt(t, FormatVersionPayloadSizes)

		commitments, err := childCommitments(root)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := ComputeChildrenChecksumAt(FormatVersionChildMetadata, commitments); !errors.Is(err, ErrInvalidDescriptor) {
			t.Errorf("commitment over older children: got %v, want ErrInvalidDescriptor", err)
		}

		// A root restamped at the new version over its older children, whatever it commits to.
		root.Descriptor = resealed(root, func(d *Descriptor) { d.FormatVersion = FormatVersionChildMetadata })

		err = ValidateTree(root, ReadOptions{})
		if !errors.Is(err, ErrInvalidDescriptor) {
			t.Fatalf("ValidateTree error = %v, want ErrInvalidDescriptor", err)
		}

		if !strings.HasPrefix(err.Error(), "Snapshot/root: ") {
			t.Errorf("error %q does not name the root", err)
		}
	})

	t.Run("an older parent verifies newer children with its own encoding", func(t *testing.T) {
		t.Parallel()

		root := sealedTestTree(t)

		commitments, err := childCommitments(root)
		if err != nil {
			t.Fatal(err)
		}

		old, err := ComputeChildrenChecksumAt(FormatVersionPayloadSizes, commitments)
		if err != nil {
			t.Fatal(err)
		}

		root.Descriptor = resealed(root, func(d *Descriptor) {
			d.FormatVersion = FormatVersionPayloadSizes
			d.ChildrenChecksum = &old
		})

		if err := ValidateTree(root, ReadOptions{}); err != nil {
			t.Fatalf("ValidateTree: %v", err)
		}

		// The old encoding does not commit to the children's metadata.
		leaf := root.Children[0]
		leaf.Descriptor = resealed(leaf, func(d *Descriptor) { d.Volumes[0].Size = "100Gi" })

		if err := ValidateTree(root, ReadOptions{}); err != nil {
			t.Errorf("ValidateTree after a resealed child edit: %v", err)
		}
	})

	t.Run("an older commitment relabeled as new fails", func(t *testing.T) {
		t.Parallel()

		root := sealedTestTree(t)

		commitments, err := childCommitments(root)
		if err != nil {
			t.Fatal(err)
		}

		old, err := ComputeChildrenChecksumAt(FormatVersionPayloadSizes, commitments)
		if err != nil {
			t.Fatal(err)
		}

		root.Descriptor = resealed(root, func(d *Descriptor) { d.ChildrenChecksum = &old })

		if err := ValidateTree(root, ReadOptions{}); !errors.Is(err, ErrChildrenChecksumMismatch) {
			t.Errorf("ValidateTree error = %v, want ErrChildrenChecksumMismatch", err)
		}
	})
}

func TestSealTreeStampsCurrentVersion(t *testing.T) {
	t.Parallel()

	root := sealedTestTreeAt(t, FormatVersionPayloadSizes)
	if err := SealTree(root); err != nil {
		t.Fatalf("SealTree over an older sealed tree: %v", err)
	}

	var check func(node *TreeNode)
	check = func(node *TreeNode) {
		if node.Descriptor.FormatVersion != FormatVersionCurrent {
			t.Errorf("%s: FormatVersion = %d, want %d", node.Descriptor.Name, node.Descriptor.FormatVersion, FormatVersionCurrent)
		}

		for _, child := range node.Children {
			check(child)
		}
	}
	check(root)

	if err := ValidateTree(root, ReadOptions{}); err != nil {
		t.Errorf("ValidateTree: %v", err)
	}
}

func TestValidateTreeRejectsNilChild(t *testing.T) {
	t.Parallel()

	root := sealedTestTree(t)
	root.Children = append(root.Children, nil)

	if err := ValidateTree(root, ReadOptions{}); !errors.Is(err, ErrInvalidDescriptor) {
		t.Errorf("got %v, want ErrInvalidDescriptor", err)
	}
}

func TestSealTreeRejectsInvalidChild(t *testing.T) {
	t.Parallel()

	root := &TreeNode{
		Descriptor: Descriptor{APIVersion: "v1", Kind: "Snapshot", Name: "root", Checksum: validChecksum()},
		Children: []*TreeNode{{
			// No ContentChecksum and no Checksum: the child's digest is malformed.
			Descriptor: Descriptor{APIVersion: "v1", Kind: "Snapshot", Name: "child"},
		}},
	}

	if err := SealTree(root); !errors.Is(err, ErrInvalidDescriptor) {
		t.Errorf("got %v, want ErrInvalidDescriptor", err)
	}
}
