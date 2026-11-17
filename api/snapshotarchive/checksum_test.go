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
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// validChildChecksum returns a well-formed NodeChecksum seeded from content.
func validChildChecksum(content string) NodeChecksum {
	return checksumFromSum(func() []byte { s := sha256.Sum256([]byte(content)); return s[:] }())
}

func TestShortChecksum(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{"abcdef0123456789", "abcdef01"},
		{"abcdef01", "abcdef01"},
		{"abcd", "abcd"},
		{"", ""},
	}

	for _, tc := range cases {
		if got := ShortChecksum(tc.in); got != tc.want {
			t.Errorf("ShortChecksum(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateChecksum(t *testing.T) {
	t.Parallel()

	if err := ValidateChecksum(validChecksum()); err != nil {
		t.Fatalf("valid checksum: %v", err)
	}

	for name, mutate := range map[string]func(*NodeChecksum){
		"algorithm":       func(c *NodeChecksum) { c.Algorithm = "SHA256" },
		"short hex":       func(c *NodeChecksum) { c.Hex = "abc"; c.Short = "abc" },
		"uppercase hex":   func(c *NodeChecksum) { c.Hex = strings.Repeat("A", 64); c.Short = strings.Repeat("A", 8) },
		"non-hex":         func(c *NodeChecksum) { c.Hex = strings.Repeat("g", 64); c.Short = strings.Repeat("g", 8) },
		"short mismatch":  func(c *NodeChecksum) { c.Short = "deadbeef" },
		"short too short": func(c *NodeChecksum) { c.Short = c.Hex[:7] },
	} {
		c := validChecksum()
		mutate(&c)

		if err := ValidateChecksum(c); !errors.Is(err, ErrInvalidDescriptor) {
			t.Errorf("%s: got %v, want ErrInvalidDescriptor", name, err)
		}
	}
}

// TestComputeContentChecksumAlgorithm recomputes the content checksum by hand from its
// definition and checks the order independence of the input.
func TestComputeContentChecksumAlgorithm(t *testing.T) {
	t.Parallel()

	files := map[string][]byte{
		"manifests/configmap_app.yaml": []byte("apiVersion: v1\nkind: ConfigMap\n"),
		"data.bin":                     bytes.Repeat([]byte{0x5a}, 4096),
		"manifests/a.yaml":             nil,
	}
	order := []string{"data.bin", "manifests/a.yaml", "manifests/configmap_app.yaml"}

	final := sha256.New()

	var digests []FileDigest

	for _, rel := range order {
		perFile := sha256.Sum256(append(append([]byte(rel), 0), files[rel]...))
		final.Write(perFile[:])

		digest, err := ComputeFileDigest(rel, bytes.NewReader(files[rel]))
		if err != nil {
			t.Fatal(err)
		}

		if digest.Digest != perFile {
			t.Errorf("%s: ComputeFileDigest = %x, want %x", rel, digest.Digest, perFile)
		}

		h := NewFileHash(rel)
		h.Write(files[rel])

		if !bytes.Equal(h.Sum(nil), perFile[:]) {
			t.Errorf("%s: NewFileHash digest differs from ComputeFileDigest", rel)
		}

		// Prepend, so the input order is the reverse of the canonical one.
		digests = append([]FileDigest{digest}, digests...)
	}

	got, err := ComputeContentChecksum(digests)
	if err != nil {
		t.Fatal(err)
	}

	if want := fmt.Sprintf("%x", final.Sum(nil)); got.Hex != want {
		t.Errorf("ComputeContentChecksum = %s, want %s", got.Hex, want)
	}

	if err := ValidateChecksum(got); err != nil {
		t.Errorf("result is not a valid checksum: %v", err)
	}

	empty, err := ComputeContentChecksum(nil)
	if err != nil {
		t.Fatal(err)
	}

	if want := fmt.Sprintf("%x", sha256.Sum256(nil)); empty.Hex != want {
		t.Errorf("empty content checksum = %s, want %s", empty.Hex, want)
	}
}

func TestComputeContentChecksumRejectsDuplicatePath(t *testing.T) {
	t.Parallel()

	digest, err := ComputeFileDigest("data.bin", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ComputeContentChecksum([]FileDigest{digest, digest}); !errors.Is(err, ErrInvalidDescriptor) {
		t.Errorf("got %v, want ErrInvalidDescriptor", err)
	}
}

// TestComputeChildrenChecksumAlgorithm recomputes the direct-child commitment by hand from its
// definition, at every format version.
func TestComputeChildrenChecksumAlgorithm(t *testing.T) {
	t.Parallel()

	child := ChildCommitment{
		APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "data-0", UID: "u",
		NodeChecksum: validChildChecksum("node"), ChildrenChecksum: EmptyChildrenChecksum(),
		MetadataChecksum: validChildChecksum("metadata"), FormatVersion: FormatVersionChildMetadata,
	}

	oldFields := []string{
		child.APIVersion, child.Kind, "", child.Name, child.UID, child.NodeChecksum.Hex, child.ChildrenChecksum.Hex,
	}

	tests := []struct {
		version int
		fields  []string
	}{
		{version: FormatVersionLegacy, fields: oldFields},
		{version: FormatVersionAuthenticatedChildren, fields: oldFields},
		{version: FormatVersionPayloadSizes, fields: oldFields},
		{version: FormatVersionChildMetadata, fields: append(oldFields[:len(oldFields):len(oldFields)], child.MetadataChecksum.Hex)},
	}

	for _, tt := range tests {
		var input []byte

		prefixed := func(s string) {
			input = binary.BigEndian.AppendUint64(input, uint64(len(s)))
			input = append(input, s...)
		}

		input = append(input, "d8-snapshot-children-checksum-v1\x00"...)
		prefixed("1")

		for _, field := range tt.fields {
			prefixed(field)
		}

		got, err := ComputeChildrenChecksumAt(tt.version, []ChildCommitment{child})
		if err != nil {
			t.Fatalf("version %d: %v", tt.version, err)
		}

		if want := fmt.Sprintf("%x", sha256.Sum256(input)); got.Hex != want {
			t.Errorf("version %d: ComputeChildrenChecksumAt = %s, want %s", tt.version, got.Hex, want)
		}

		empty, err := ComputeChildrenChecksumAt(tt.version, nil)
		if err != nil {
			t.Fatalf("version %d, no children: %v", tt.version, err)
		}

		if empty != EmptyChildrenChecksum() {
			t.Errorf("version %d: empty commitment %s differs from EmptyChildrenChecksum", tt.version, empty.Hex)
		}
	}

	current, err := ComputeChildrenChecksum([]ChildCommitment{child})
	if err != nil {
		t.Fatal(err)
	}

	atCurrent, err := ComputeChildrenChecksumAt(FormatVersionCurrent, []ChildCommitment{child})
	if err != nil {
		t.Fatal(err)
	}

	if current != atCurrent {
		t.Errorf("ComputeChildrenChecksum = %s, want the FormatVersionCurrent commitment %s", current.Hex, atCurrent.Hex)
	}

	if want := "926e82507890b7d5c7ad50f8cb7fd2172f81bfd2cf1d2ca86e3b644f66b02701"; EmptyChildrenChecksum().Hex != want {
		t.Errorf("EmptyChildrenChecksum = %s, want %s", EmptyChildrenChecksum().Hex, want)
	}
}

// TestComputeChildrenChecksumDeterministicAndOrderIndependent proves the same child set in any
// input order hashes identically, and that changing a committed field changes the result.
func TestComputeChildrenChecksumDeterministicAndOrderIndependent(t *testing.T) {
	t.Parallel()

	a := ChildCommitment{
		APIVersion: "v1", Kind: "Snapshot", Name: "a", Namespace: "ns",
		NodeChecksum: validChildChecksum("a-node"), ChildrenChecksum: validChildChecksum("a-children"),
		MetadataChecksum: validChildChecksum("a-metadata"), FormatVersion: FormatVersionChildMetadata,
	}
	b := ChildCommitment{
		APIVersion: "v1", Kind: "Snapshot", Name: "b", Namespace: "ns",
		NodeChecksum: validChildChecksum("b-node"), ChildrenChecksum: validChildChecksum("b-children"),
		MetadataChecksum: validChildChecksum("b-metadata"), FormatVersion: FormatVersionChildMetadata,
	}

	for _, version := range []int{FormatVersionPayloadSizes, FormatVersionChildMetadata} {
		forward, err := ComputeChildrenChecksumAt(version, []ChildCommitment{a, b})
		if err != nil {
			t.Fatal(err)
		}

		reversed, err := ComputeChildrenChecksumAt(version, []ChildCommitment{b, a})
		if err != nil {
			t.Fatal(err)
		}

		if forward.Hex != reversed.Hex {
			t.Errorf("version %d: checksum depends on input order: %q vs %q", version, forward.Hex, reversed.Hex)
		}

		if forward.Hex == EmptyChildrenChecksum().Hex {
			t.Errorf("version %d: non-empty child set must not collide with the empty commitment", version)
		}

		for name, mutate := range map[string]func(*ChildCommitment){
			"node checksum":     func(c *ChildCommitment) { c.NodeChecksum = validChildChecksum("tampered") },
			"children checksum": func(c *ChildCommitment) { c.ChildrenChecksum = validChildChecksum("tampered") },
			"metadata checksum": func(c *ChildCommitment) { c.MetadataChecksum = validChildChecksum("tampered") },
			"namespace":         func(c *ChildCommitment) { c.Namespace = "other" },
			"uid":               func(c *ChildCommitment) { c.UID = "uid" },
		} {
			tampered := a
			mutate(&tampered)

			changed, err := ComputeChildrenChecksumAt(version, []ChildCommitment{tampered, b})
			if err != nil {
				t.Fatalf("version %d, %s: %v", version, name, err)
			}

			// Below FormatVersionChildMetadata the child's metadata checksum is not committed.
			committed := name != "metadata checksum" || version >= FormatVersionChildMetadata
			if committed == (changed.Hex == forward.Hex) {
				t.Errorf("version %d: changing the %s changed the checksum: %t, want %t",
					version, name, changed.Hex != forward.Hex, committed)
			}
		}
	}

	old, err := ComputeChildrenChecksumAt(FormatVersionPayloadSizes, []ChildCommitment{a, b})
	if err != nil {
		t.Fatal(err)
	}

	current, err := ComputeChildrenChecksumAt(FormatVersionChildMetadata, []ChildCommitment{a, b})
	if err != nil {
		t.Fatal(err)
	}

	if old.Hex == current.Hex {
		t.Error("the same child set must commit differently at FormatVersionPayloadSizes and FormatVersionChildMetadata")
	}
}

func TestComputeChildrenChecksumRejectsInvalidCommitments(t *testing.T) {
	t.Parallel()

	base := func() ChildCommitment {
		return ChildCommitment{
			APIVersion: "v1", Kind: "Snapshot", Name: "child", Namespace: "ns",
			NodeChecksum: validChildChecksum("node"), ChildrenChecksum: EmptyChildrenChecksum(),
			MetadataChecksum: validChildChecksum("metadata"), FormatVersion: FormatVersionChildMetadata,
		}
	}

	tests := map[string]func(c *ChildCommitment){
		"missing apiVersion":              func(c *ChildCommitment) { c.APIVersion = "" },
		"missing kind":                    func(c *ChildCommitment) { c.Kind = "" },
		"missing name":                    func(c *ChildCommitment) { c.Name = "" },
		"bad node checksum algorithm":     func(c *ChildCommitment) { c.NodeChecksum.Algorithm = "md5" },
		"bad node checksum hex length":    func(c *ChildCommitment) { c.NodeChecksum.Hex = "abc" },
		"bad children checksum algorithm": func(c *ChildCommitment) { c.ChildrenChecksum.Algorithm = "md5" },
	}

	for name, mutate := range tests {
		c := base()
		mutate(&c)

		for _, version := range []int{FormatVersionPayloadSizes, FormatVersionChildMetadata} {
			if _, err := ComputeChildrenChecksumAt(version, []ChildCommitment{c}); !errors.Is(err, ErrInvalidDescriptor) {
				t.Errorf("version %d, %s: got %v, want ErrInvalidDescriptor", version, name, err)
			}
		}
	}

	// What only a FormatVersionChildMetadata parent requires of a child.
	metadataTests := map[string]func(c *ChildCommitment){
		"child at FormatVersionPayloadSizes":  func(c *ChildCommitment) { c.FormatVersion = FormatVersionPayloadSizes },
		"legacy child":                        func(c *ChildCommitment) { c.FormatVersion, c.MetadataChecksum = 0, NodeChecksum{} },
		"missing metadata checksum":           func(c *ChildCommitment) { c.MetadataChecksum = NodeChecksum{} },
		"bad metadata checksum algorithm":     func(c *ChildCommitment) { c.MetadataChecksum.Algorithm = "md5" },
		"bad metadata checksum short":         func(c *ChildCommitment) { c.MetadataChecksum.Short = "00000000" },
		"uppercase metadata checksum hex":     func(c *ChildCommitment) { c.MetadataChecksum.Hex = strings.ToUpper(c.MetadataChecksum.Hex) },
		"metadata checksum hex of wrong size": func(c *ChildCommitment) { c.MetadataChecksum.Hex = "abc" },
	}

	for name, mutate := range metadataTests {
		c := base()
		mutate(&c)

		if _, err := ComputeChildrenChecksumAt(FormatVersionChildMetadata, []ChildCommitment{c}); !errors.Is(err, ErrInvalidDescriptor) {
			t.Errorf("%s: got %v, want ErrInvalidDescriptor", name, err)
		}

		for _, version := range []int{FormatVersionLegacy, FormatVersionAuthenticatedChildren, FormatVersionPayloadSizes} {
			if _, err := ComputeChildrenChecksumAt(version, []ChildCommitment{c}); err != nil {
				t.Errorf("version %d ignores the child's metadata, %s: %v", version, name, err)
			}
		}
	}

	dup := base()
	other := dup
	other.NodeChecksum = validChildChecksum("second")

	if _, err := ComputeChildrenChecksum([]ChildCommitment{dup, other}); !errors.Is(err, ErrInvalidDescriptor) {
		t.Errorf("duplicate identity: got %v, want ErrInvalidDescriptor", err)
	}

	for _, version := range []int{1, FormatVersionCurrent + 1, -1} {
		if _, err := ComputeChildrenChecksumAt(version, nil); !errors.Is(err, ErrUnsupportedFormat) {
			t.Errorf("version %d: got %v, want ErrUnsupportedFormat", version, err)
		}
	}
}

func TestComputeChildrenChecksumRejectsTooManyChildren(t *testing.T) {
	t.Parallel()

	commitments := make([]ChildCommitment, MaxDirectChildren+1)
	for i := range commitments {
		commitments[i] = ChildCommitment{
			APIVersion: "v1", Kind: "Snapshot", Name: fmt.Sprintf("c-%d", i),
			NodeChecksum: validChildChecksum("n"), ChildrenChecksum: EmptyChildrenChecksum(),
			MetadataChecksum: validChildChecksum("m"), FormatVersion: FormatVersionChildMetadata,
		}
	}

	if _, err := ComputeChildrenChecksum(commitments); !errors.Is(err, ErrTooManyDirectChildren) {
		t.Errorf("got %v, want ErrTooManyDirectChildren", err)
	}

	if _, err := ComputeChildrenChecksum(commitments[:MaxDirectChildren]); err != nil {
		t.Errorf("exactly MaxDirectChildren children: %v", err)
	}
}

func TestChildCommitmentOf(t *testing.T) {
	t.Parallel()

	metadata := validChildChecksum("metadata")
	d := Descriptor{
		FormatVersion: FormatVersionChildMetadata,
		APIVersion:    "v1", Kind: "Snapshot", Name: "n", Namespace: "ns", UID: "u",
		Checksum: validChecksum(), ChildrenChecksum: validChildrenChecksum(), MetadataChecksum: &metadata,
	}

	got, err := ChildCommitmentOf(d)
	if err != nil {
		t.Fatal(err)
	}

	want := ChildCommitment{
		APIVersion: "v1", Kind: "Snapshot", Name: "n", Namespace: "ns", UID: "u",
		NodeChecksum: validChecksum(), ChildrenChecksum: EmptyChildrenChecksum(),
		MetadataChecksum: metadata, FormatVersion: FormatVersionChildMetadata,
	}
	if got != want {
		t.Errorf("ChildCommitmentOf = %+v, want %+v", got, want)
	}

	// A legacy child has no metadata checksum; the commitment records it as the zero value.
	d.FormatVersion, d.MetadataChecksum = FormatVersionLegacy, nil
	want.FormatVersion, want.MetadataChecksum = FormatVersionLegacy, NodeChecksum{}

	if got, err := ChildCommitmentOf(d); err != nil || got != want {
		t.Errorf("legacy child: ChildCommitmentOf = %+v, %v, want %+v", got, err, want)
	}

	d.ChildrenChecksum = nil
	if _, err := ChildCommitmentOf(d); !errors.Is(err, ErrInvalidDescriptor) {
		t.Errorf("without ChildrenChecksum: got %v, want ErrInvalidDescriptor", err)
	}
}

func TestVerifyChildrenChecksum(t *testing.T) {
	t.Parallel()

	child := ChildCommitment{
		APIVersion: "v1", Kind: "Snapshot", Name: "child",
		NodeChecksum: validChildChecksum("node"), ChildrenChecksum: EmptyChildrenChecksum(),
		MetadataChecksum: validChildChecksum("metadata"), FormatVersion: FormatVersionChildMetadata,
	}

	committed, err := ComputeChildrenChecksum([]ChildCommitment{child})
	if err != nil {
		t.Fatal(err)
	}

	d := Descriptor{FormatVersion: FormatVersionCurrent, ChildrenChecksum: &committed}

	if err := VerifyChildrenChecksum(d, []ChildCommitment{child}); err != nil {
		t.Errorf("matching children: %v", err)
	}

	if err := VerifyChildrenChecksum(d, nil); !errors.Is(err, ErrChildrenChecksumMismatch) {
		t.Errorf("removed child: got %v, want ErrChildrenChecksumMismatch", err)
	}

	resealed := child
	resealed.MetadataChecksum = validChildChecksum("resealed")

	if err := VerifyChildrenChecksum(d, []ChildCommitment{resealed}); !errors.Is(err, ErrChildrenChecksumMismatch) {
		t.Errorf("child metadata changed: got %v, want ErrChildrenChecksumMismatch", err)
	}

	// The parent's own version selects the encoding: a FormatVersionPayloadSizes parent verifies
	// with the old one, ignores the child's metadata checksum, and a commitment recorded at one
	// version does not verify under the other.
	old, err := ComputeChildrenChecksumAt(FormatVersionPayloadSizes, []ChildCommitment{child})
	if err != nil {
		t.Fatal(err)
	}

	oldParent := Descriptor{FormatVersion: FormatVersionPayloadSizes, ChildrenChecksum: &old}
	if err := VerifyChildrenChecksum(oldParent, []ChildCommitment{resealed}); err != nil {
		t.Errorf("FormatVersionPayloadSizes parent: %v", err)
	}

	relabeled := Descriptor{FormatVersion: FormatVersionChildMetadata, ChildrenChecksum: &old}
	if err := VerifyChildrenChecksum(relabeled, []ChildCommitment{child}); !errors.Is(err, ErrChildrenChecksumMismatch) {
		t.Errorf("old commitment under a new parent: got %v, want ErrChildrenChecksumMismatch", err)
	}

	oldParent.ChildrenChecksum = &committed
	if err := VerifyChildrenChecksum(oldParent, []ChildCommitment{child}); !errors.Is(err, ErrChildrenChecksumMismatch) {
		t.Errorf("new commitment under an old parent: got %v, want ErrChildrenChecksumMismatch", err)
	}

	unsupported := Descriptor{FormatVersion: FormatVersionCurrent + 1, ChildrenChecksum: &committed}
	if err := VerifyChildrenChecksum(unsupported, []ChildCommitment{child}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("unsupported parent version: got %v, want ErrUnsupportedFormat", err)
	}

	d.ChildrenChecksum = nil
	if err := VerifyChildrenChecksum(d, nil); !errors.Is(err, ErrChildrenChecksumMismatch) {
		t.Errorf("missing commitment: got %v, want ErrChildrenChecksumMismatch", err)
	}

	d.FormatVersion = FormatVersionLegacy
	if err := VerifyChildrenChecksum(d, []ChildCommitment{child}); err != nil {
		t.Errorf("legacy descriptors are exempt: %v", err)
	}
}
