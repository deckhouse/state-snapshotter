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
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
)

// ChecksumAlgorithmSHA256 is the only digest algorithm of the format; it is the value every
// NodeChecksum.Algorithm must carry.
const ChecksumAlgorithmSHA256 = "sha256"

// MaxDirectChildren bounds the number of direct children one node's ChildrenChecksum may commit
// to. It bounds both the memory retained while computing or verifying the commitment and the
// work an adversarial archive can force onto a single node.
const MaxDirectChildren = 10_000

// sha256HexLen is the length of a hex-encoded SHA-256 digest.
const sha256HexLen = 64

// childrenChecksumDomain separates the direct-child commitment from every other SHA-256 input.
const childrenChecksumDomain = "d8-snapshot-children-checksum-v1\x00"

// ErrChecksumMismatch is returned when a recomputed content checksum differs from the value
// recorded in the descriptor.
var ErrChecksumMismatch = errors.New("checksum mismatch")

// ErrChildrenChecksumMismatch is returned when a node's direct-child commitment does not match
// its actual direct children (a child was added, removed, replaced, duplicated, or its digest no
// longer matches).
var ErrChildrenChecksumMismatch = errors.New("children checksum mismatch")

// ErrTooManyDirectChildren is returned when a node has more than MaxDirectChildren direct
// children.
var ErrTooManyDirectChildren = errors.New("direct child count exceeds bound")

// NodeChecksum is one SHA-256 digest as recorded in a descriptor.
type NodeChecksum struct {
	// Algorithm is always ChecksumAlgorithmSHA256.
	Algorithm string `json:"algorithm"`
	// Hex is the full lowercase hex-encoded digest.
	Hex string `json:"hex"`
	// Short is the first 8 characters of Hex (see ShortChecksum).
	Short string `json:"short"`
}

// ShortChecksum returns the first 8 hex characters of hex, or hex itself when it is shorter.
func ShortChecksum(hex string) string {
	if len(hex) >= 8 {
		return hex[:8]
	}

	return hex
}

// ValidateChecksum enforces the algorithm/hex/short consistency of a recorded NodeChecksum:
// algorithm is "sha256", hex is 64 lowercase hex characters and short is ShortChecksum(hex).
func ValidateChecksum(c NodeChecksum) error {
	if c.Algorithm != ChecksumAlgorithmSHA256 {
		return fmt.Errorf("checksum.algorithm must be %q, got %q: %w",
			ChecksumAlgorithmSHA256, c.Algorithm, ErrInvalidDescriptor)
	}

	if len(c.Hex) != sha256HexLen || !isLowerHex(c.Hex) {
		return fmt.Errorf("checksum.hex must be %d lowercase hex characters, got %q: %w",
			sha256HexLen, c.Hex, ErrInvalidDescriptor)
	}

	if want := ShortChecksum(c.Hex); c.Short != want {
		return fmt.Errorf("checksum.short %q is inconsistent with hex (want %q): %w",
			c.Short, want, ErrInvalidDescriptor)
	}

	return nil
}

func checksumFromSum(sum []byte) NodeChecksum {
	hexString := fmt.Sprintf("%x", sum)

	return NodeChecksum{
		Algorithm: ChecksumAlgorithmSHA256,
		Hex:       hexString,
		Short:     ShortChecksum(hexString),
	}
}

// isLowerHex reports whether s consists solely of lowercase hexadecimal digits.
func isLowerHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}

	return true
}

// FileDigest is one covered file's contribution to a node's content checksum.
type FileDigest struct {
	// Path is the file's path relative to the node directory, exactly as it was fed to
	// NewFileHash (e.g. "manifests/configmap_app.yaml", "data.bin").
	Path string
	// Digest is SHA-256(Path || 0x00 || file bytes).
	Digest [sha256.Size]byte
}

// NewFileHash returns a SHA-256 hash already fed with relPath and one zero byte. Writing the
// file's bytes into it and taking Sum yields the file's per-file digest. It lets a caller hash a
// file in the same pass that also serves other consumers (for example an io.MultiWriter).
//
// relPath is the file's path relative to the node directory with "/" as the separator.
func NewFileHash(relPath string) hash.Hash {
	h := sha256.New()
	_, _ = h.Write([]byte(relPath))
	_, _ = h.Write([]byte{0})

	return h
}

// ComputeFileDigest reads content to its end and returns its per-file digest under relPath.
func ComputeFileDigest(relPath string, content io.Reader) (FileDigest, error) {
	h := NewFileHash(relPath)
	if _, err := io.Copy(h, content); err != nil {
		return FileDigest{}, fmt.Errorf("hash %s: %w", relPath, err)
	}

	digest := FileDigest{Path: relPath}
	copy(digest.Digest[:], h.Sum(nil))

	return digest, nil
}

// ComputeContentChecksum folds per-file digests into a node's content checksum: the files are
// sorted by Path bytewise and their raw 32-byte digests are fed in that order into one final
// SHA-256. The input order does not matter; a repeated Path is rejected (ErrInvalidDescriptor).
// A node without covered files yields the SHA-256 of the empty input.
func ComputeContentChecksum(files []FileDigest) (NodeChecksum, error) {
	sorted := make([]FileDigest, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	final := sha256.New()

	for i, file := range sorted {
		if i > 0 && file.Path == sorted[i-1].Path {
			return NodeChecksum{}, fmt.Errorf("file %q is covered twice: %w", file.Path, ErrInvalidDescriptor)
		}

		_, _ = final.Write(file.Digest[:])
	}

	return checksumFromSum(final.Sum(nil)), nil
}

// ChildCommitment is the record a parent's ChildrenChecksum commits to for one direct child: the
// child's identity plus its own Checksum and ChildrenChecksum. Chaining the child's own
// ChildrenChecksum makes the root's commitment cover the whole tree once every node's commitment
// is verified.
type ChildCommitment struct {
	APIVersion       string
	Kind             string
	Name             string
	Namespace        string
	UID              string
	NodeChecksum     NodeChecksum
	ChildrenChecksum NodeChecksum
}

// ChildCommitmentOf returns the commitment a parent records for the child described by d. It
// fails with ErrInvalidDescriptor when d carries no ChildrenChecksum.
func ChildCommitmentOf(d Descriptor) (ChildCommitment, error) {
	if d.ChildrenChecksum == nil {
		return ChildCommitment{}, fmt.Errorf("%s/%s has no authenticated children commitment: %w",
			d.Kind, d.Name, ErrInvalidDescriptor)
	}

	return ChildCommitment{
		APIVersion:       d.APIVersion,
		Kind:             d.Kind,
		Name:             d.Name,
		Namespace:        d.Namespace,
		UID:              d.UID,
		NodeChecksum:     d.Checksum,
		ChildrenChecksum: *d.ChildrenChecksum,
	}, nil
}

// EmptyChildrenChecksum is the ChildrenChecksum of a node with no direct children.
func EmptyChildrenChecksum() NodeChecksum {
	checksum, err := ComputeChildrenChecksum(nil)
	if err != nil {
		// Hashing zero records cannot fail ComputeChildrenChecksum's validation.
		panic(fmt.Sprintf("compute empty children checksum: %v", err))
	}

	return checksum
}

// ComputeChildrenChecksum computes the digest committing to an exact set of direct children. It
// is independent of the input order: the commitments are sorted by the identity tuple
// (apiVersion, kind, namespace, name, uid) joined with 0x1f and compared bytewise. The hash input
// is the domain string "d8-snapshot-children-checksum-v1" and a zero byte, the decimal child count,
// then for every child its apiVersion, kind, namespace, name, uid, checksum hex and
// childrenChecksum hex, every string prefixed with its byte length as a big-endian uint64.
//
// It rejects more than MaxDirectChildren commitments (ErrTooManyDirectChildren), and a commitment
// with an incomplete identity, a malformed digest or a duplicate identity (ErrInvalidDescriptor).
func ComputeChildrenChecksum(commitments []ChildCommitment) (NodeChecksum, error) {
	if len(commitments) > MaxDirectChildren {
		return NodeChecksum{}, fmt.Errorf("%d direct children exceeds bound %d: %w",
			len(commitments), MaxDirectChildren, ErrTooManyDirectChildren)
	}

	canonical := make([]ChildCommitment, len(commitments))
	copy(canonical, commitments)

	for i := range canonical {
		record := canonical[i]
		if record.APIVersion == "" || record.Kind == "" || record.Name == "" {
			return NodeChecksum{}, fmt.Errorf(
				"child identity apiVersion/kind/name must be complete (got %q/%q/%q): %w",
				record.APIVersion, record.Kind, record.Name, ErrInvalidDescriptor)
		}

		if err := ValidateChecksum(record.NodeChecksum); err != nil {
			return NodeChecksum{}, fmt.Errorf("child %s/%s node checksum: %w", record.Kind, record.Name, err)
		}

		if err := ValidateChecksum(record.ChildrenChecksum); err != nil {
			return NodeChecksum{}, fmt.Errorf("child %s/%s children checksum: %w", record.Kind, record.Name, err)
		}
	}

	sort.Slice(canonical, func(i, j int) bool {
		return childIdentityKey(canonical[i]) < childIdentityKey(canonical[j])
	})

	for i := 1; i < len(canonical); i++ {
		if childIdentityKey(canonical[i]) == childIdentityKey(canonical[i-1]) {
			return NodeChecksum{}, fmt.Errorf("duplicate direct child identity %s: %w",
				childIdentityKey(canonical[i]), ErrInvalidDescriptor)
		}
	}

	h := sha256.New()
	_, _ = h.Write([]byte(childrenChecksumDomain))
	writeLengthPrefixed(h, fmt.Sprintf("%d", len(canonical)))

	for _, record := range canonical {
		writeLengthPrefixed(h, record.APIVersion)
		writeLengthPrefixed(h, record.Kind)
		writeLengthPrefixed(h, record.Namespace)
		writeLengthPrefixed(h, record.Name)
		writeLengthPrefixed(h, record.UID)
		writeLengthPrefixed(h, record.NodeChecksum.Hex)
		writeLengthPrefixed(h, record.ChildrenChecksum.Hex)
	}

	return checksumFromSum(h.Sum(nil)), nil
}

// VerifyChildrenChecksum recomputes the commitment over children and compares it with
// d.ChildrenChecksum (ErrChildrenChecksumMismatch on a difference). A legacy version-zero
// descriptor is exempt: its commitment is as unauthenticated as the rest of its metadata.
func VerifyChildrenChecksum(d Descriptor, children []ChildCommitment) error {
	if d.FormatVersion == FormatVersionLegacy {
		return nil
	}

	computed, err := ComputeChildrenChecksum(children)
	if err != nil {
		return err
	}

	stored := "<missing>"
	if d.ChildrenChecksum != nil {
		stored = d.ChildrenChecksum.Hex
	}

	if d.ChildrenChecksum == nil || computed.Hex != d.ChildrenChecksum.Hex {
		return fmt.Errorf("stored %q computed %q: %w", stored, computed.Hex, ErrChildrenChecksumMismatch)
	}

	return nil
}

// childIdentityKey renders a commitment's identity as a 0x1f-separated sort and comparison key.
func childIdentityKey(c ChildCommitment) string {
	return c.APIVersion + "\x1f" + c.Kind + "\x1f" + c.Namespace + "\x1f" + c.Name + "\x1f" + c.UID
}

func writeLengthPrefixed(w io.Writer, field string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(field)))
	_, _ = w.Write(length[:])
	_, _ = w.Write([]byte(field))
}
