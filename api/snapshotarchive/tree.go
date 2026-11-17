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
	"fmt"
)

// TreeNode is one node of a descriptor tree held in memory: its descriptor, what the caller
// observed about its content, and its direct children.
type TreeNode struct {
	// Descriptor is the node's snapshot.yaml.
	Descriptor Descriptor
	// ContentChecksum is the content checksum the caller recomputed from the node's covered files
	// (see ComputeContentChecksum). Nil means the content was not examined: SealTree then keeps
	// Descriptor.Checksum as is, and ValidateTree does not compare it.
	ContentChecksum *NodeChecksum
	// HasBlockData and HasFilesystemData report the node's payload, as for ValidateDescriptor.
	HasBlockData      bool
	HasFilesystemData bool
	// Children are the node's direct children.
	Children []*TreeNode
}

// SealTree fills in the digests of every descriptor in the tree bottom up: a node's Checksum is
// set from ContentChecksum when that is not nil, its ChildrenChecksum is computed from its
// children once they are sealed, and the descriptor is then sealed (see Seal). This is the order
// the format requires, because a child's digests, its MetadataChecksum included, enter its
// parent's ChildrenChecksum. Every node is stamped FormatVersionCurrent, whatever version its
// descriptor declared before.
func SealTree(root *TreeNode) error {
	return sealTreeAt(root, FormatVersionCurrent)
}

// sealTreeAt is SealTree stamping formatVersion on every node and committing to the children
// with that version's encoding. Like sealAt, it serves the tests of the older versions only.
func sealTreeAt(root *TreeNode, formatVersion int) error {
	return walkPostOrder(root, nil, func(node *TreeNode) error {
		if node.ContentChecksum != nil {
			node.Descriptor.Checksum = *node.ContentChecksum
		}

		commitments, err := childCommitments(node)
		if err != nil {
			return err
		}

		children, err := ComputeChildrenChecksumAt(formatVersion, commitments)
		if err != nil {
			return err
		}

		node.Descriptor.ChildrenChecksum = &children

		sealed, err := sealAt(node.Descriptor, formatVersion)
		if err != nil {
			return err
		}

		node.Descriptor = sealed

		return nil
	})
}

// ValidateTree validates every node of the tree: its descriptor (see ValidateDescriptor), its
// content checksum when ContentChecksum is set (ErrChecksumMismatch on a difference) and its
// direct-child commitment against its actual children (see VerifyChildrenChecksum), children
// before their parent, so that every child's MetadataChecksum is verified before a parent commits
// to it. A passing tree is authenticated end to end by the root's descriptor, provided the caller
// computed every ContentChecksum itself: the content, the topology and, below a node at
// FormatVersionChildMetadata, every descriptor field. Under an older parent a child's formatVersion,
// sourceName, sourceObjectRef and volumes are covered by the child's own MetadataChecksum only,
// which anyone able to rewrite the child can recompute. Errors name the offending node by its
// kind/name path from the root.
func ValidateTree(root *TreeNode, options ReadOptions) error {
	return walkPostOrder(root, nil, func(node *TreeNode) error {
		d := node.Descriptor
		if err := ValidateDescriptor(d, node.HasBlockData, node.HasFilesystemData, options); err != nil {
			return err
		}

		if node.ContentChecksum != nil && node.ContentChecksum.Hex != d.Checksum.Hex {
			return fmt.Errorf("stored %q computed %q: %w", d.Checksum.Hex, node.ContentChecksum.Hex, ErrChecksumMismatch)
		}

		commitments, err := childCommitments(node)
		if err != nil {
			return err
		}

		return VerifyChildrenChecksum(d, commitments)
	})
}

func childCommitments(node *TreeNode) ([]ChildCommitment, error) {
	if len(node.Children) > MaxDirectChildren {
		return nil, fmt.Errorf("%d direct children exceeds bound %d: %w",
			len(node.Children), MaxDirectChildren, ErrTooManyDirectChildren)
	}

	commitments := make([]ChildCommitment, 0, len(node.Children))

	for _, child := range node.Children {
		commitment, err := ChildCommitmentOf(child.Descriptor)
		if err != nil {
			return nil, err
		}

		commitments = append(commitments, commitment)
	}

	return commitments, nil
}

// walkPostOrder visits children before their parent and prefixes a node's error with its
// kind/name path from the root.
func walkPostOrder(node *TreeNode, path []string, visit func(*TreeNode) error) error {
	if node == nil {
		return fmt.Errorf("%s: nil tree node: %w", pathString(path), ErrInvalidDescriptor)
	}

	path = append(path[:len(path):len(path)], node.Descriptor.Kind+"/"+node.Descriptor.Name)

	for _, child := range node.Children {
		if err := walkPostOrder(child, path, visit); err != nil {
			return err
		}
	}

	if err := visit(node); err != nil {
		return fmt.Errorf("%s: %w", pathString(path), err)
	}

	return nil
}

func pathString(path []string) string {
	if len(path) == 0 {
		return "<root>"
	}

	out := path[0]
	for _, element := range path[1:] {
		out += " > " + element
	}

	return out
}
