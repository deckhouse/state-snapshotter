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
// seals it.
func sealedTestTree(t *testing.T) *TreeNode {
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

	if err := SealTree(root); err != nil {
		t.Fatalf("SealTree: %v", err)
	}

	return root
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
