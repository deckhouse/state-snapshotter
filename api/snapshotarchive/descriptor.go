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
	"encoding/json"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
	sigsyaml "sigs.k8s.io/yaml"
)

const (
	// FormatVersionLegacy identifies archives written before explicit envelope versioning.
	// Version zero is accepted only through ReadOptions.AllowUnauthenticatedLegacy.
	FormatVersionLegacy = 0
	// FormatVersionAuthenticatedChildren adds the mandatory ChildrenChecksum and the
	// MetadataChecksum. A descriptor at this version has no RawSizeBytes/StoredSizeBytes;
	// readers measure the payload instead.
	FormatVersionAuthenticatedChildren = 2
	// FormatVersionPayloadSizes adds VolumeInfo.RawSizeBytes/StoredSizeBytes, the measured
	// on-disk payload footprint (as opposed to Size's nominal restoreSize).
	FormatVersionPayloadSizes = 3
	// FormatVersionChildMetadata makes the direct-child commitment cover every child's
	// MetadataChecksum as well (see ComputeChildrenChecksumAt), so that a child's descriptor
	// metadata outside its two other digests (formatVersion, sourceName, sourceObjectRef and the
	// volumes entry) is authenticated by its parent, and a root at this version authenticates
	// every descriptor of its tree. A parent at this version accepts only children at this
	// version or later. The descriptor schema is that of FormatVersionPayloadSizes.
	FormatVersionChildMetadata = 4
	// FormatVersionCurrent is stamped by Seal and SealTree, and by MarshalJSON on a descriptor
	// that declares no authenticated version.
	FormatVersionCurrent = FormatVersionChildMetadata
)

// Volume modes recorded in VolumeInfo.VolumeMode. They mirror the corev1.PersistentVolumeMode
// values and MUST agree with the node's payload kind: a block payload is "Block", a filesystem
// payload is "Filesystem". ValidateDescriptor enforces that agreement.
const (
	VolumeModeBlock      = "Block"
	VolumeModeFilesystem = "Filesystem"
)

// ErrInvalidDescriptor is returned when a descriptor violates a structural invariant of the
// format.
var ErrInvalidDescriptor = errors.New("invalid snapshot.yaml")

// ErrUnsupportedFormat is returned when a descriptor declares an unknown format version.
var ErrUnsupportedFormat = errors.New("unsupported snapshot.yaml format version")

// ErrLegacyFormat is returned when an unversioned descriptor is read without explicit
// permission to trust its unauthenticated metadata.
var ErrLegacyFormat = errors.New(
	"legacy snapshot.yaml metadata is unauthenticated; explicit compatibility mode is required",
)

// ErrMetadataChecksumMismatch is returned when a versioned descriptor differs from the
// canonical metadata digest recorded when it was written.
var ErrMetadataChecksumMismatch = errors.New("snapshot.yaml metadata checksum mismatch")

// ReadOptions controls compatibility when decoding or validating a descriptor. The zero value
// fails closed. AllowUnauthenticatedLegacy must be selected explicitly by migration or inspection
// callers that accept legacy metadata without an integrity checksum. It never bypasses the
// mandatory ChildrenChecksum direct-child commitment: every descriptor, legacy or current, must
// carry one.
type ReadOptions struct {
	AllowUnauthenticatedLegacy bool
}

// Descriptor is the per-node snapshot.yaml of a snapshot archive. It records the identity of the
// node's snapshot object and the node's three integrity digests.
//
// The field order of this struct is the canonical JSON key order the metadata checksum is
// computed over, and each field's omitempty tag is its canonical omission rule. Neither may
// change without a format version change (see the package documentation).
type Descriptor struct {
	// FormatVersion identifies the descriptor schema. Missing decodes as legacy version zero,
	// which normal readers reject; a new descriptor is always stamped FormatVersionCurrent (see
	// Seal and MarshalJSON).
	FormatVersion int `json:"formatVersion,omitempty"`
	// APIVersion is the apiVersion of the node's snapshot object
	// (e.g. "state-snapshotter.deckhouse.io/v1alpha1").
	APIVersion string `json:"apiVersion"`
	// Kind is the kind of the node's snapshot object (e.g. "Snapshot", "VolumeSnapshot").
	Kind string `json:"kind"`
	// Name is the metadata.name of the node's snapshot object.
	Name string `json:"name"`
	// Namespace is the namespace of the node's snapshot object. Omitted for cluster-scoped objects.
	Namespace string `json:"namespace,omitempty"`
	// UID is the metadata.uid of the node's snapshot object.
	UID string `json:"uid,omitempty"`
	// SourceName is the metadata.name of the captured source object, recorded for readability.
	// It is not an identity component.
	SourceName string `json:"sourceName,omitempty"`
	// SourceObjectRef carries the structured spec.sourceRef of a domain snapshot object. Absent
	// for core Snapshot nodes and VolumeSnapshot data leaves.
	SourceObjectRef *SourceObjectRef `json:"sourceObjectRef,omitempty"`
	// Checksum is the content checksum of the node (see ComputeContentChecksum).
	Checksum NodeChecksum `json:"checksum"`
	// ChildrenChecksum authenticates the exact set of the node's direct children (see
	// ComputeChildrenChecksum). A node with no children commits to EmptyChildrenChecksum. It is
	// mandatory on every descriptor.
	ChildrenChecksum *NodeChecksum `json:"childrenChecksum,omitempty"`
	// MetadataChecksum covers the canonical descriptor except this field itself (see
	// ComputeMetadataChecksum). It is absent only on legacy version-zero descriptors.
	MetadataChecksum *NodeChecksum `json:"metadataChecksum,omitempty"`
	// Volumes lists the captured volume of the node: exactly one entry on a node with a payload,
	// none otherwise.
	Volumes []VolumeInfo `json:"volumes,omitempty"`
}

// SourceObjectRef is the structured spec.sourceRef of a domain snapshot object: the
// apiVersion/kind/name of the source object. It is all-or-nothing.
type SourceObjectRef struct {
	// APIVersion is the apiVersion of the source object (e.g. "demo.deckhouse.io/v1alpha1").
	APIVersion string `json:"apiVersion"`
	// Kind is the kind of the source object (e.g. "DemoVirtualDisk").
	Kind string `json:"kind"`
	// Name is the metadata.name of the source object.
	Name string `json:"name"`
}

// VolumeObjectRef is a reference to a Kubernetes object stored in a descriptor's volume entry.
type VolumeObjectRef struct {
	// APIVersion is the apiVersion of the referenced object.
	APIVersion string `json:"apiVersion"`
	// Kind is the kind of the referenced object.
	Kind string `json:"kind"`
	// Name is the metadata.name of the referenced object.
	Name string `json:"name"`
	// Namespace is the namespace of the referenced object. Omitted for cluster-scoped objects.
	Namespace string `json:"namespace,omitempty"`
	// UID is the metadata.uid of the referenced object. Omitted when unknown.
	UID string `json:"uid,omitempty"`
}

// VolumeInfo describes the captured volume whose bytes are the node's payload.
type VolumeInfo struct {
	// Target is the captured PersistentVolumeClaim.
	Target VolumeObjectRef `json:"target"`
	// Artifact is the VolumeSnapshotContent that held the data at capture time. Recorded for
	// provenance only.
	Artifact VolumeObjectRef `json:"artifact"`
	// VolumeMode is the source volume mode, VolumeModeBlock or VolumeModeFilesystem.
	VolumeMode string `json:"volumeMode,omitempty"`
	// StorageClassName is the StorageClass of the captured volume.
	StorageClassName string `json:"storageClassName,omitempty"`
	// Size is the nominal capacity of the captured volume as a resource.Quantity string
	// (e.g. "10Gi"). It is not the payload's byte size.
	Size string `json:"size,omitempty"`
	// RawSizeBytes is the exact decoded byte count of the payload. Recorded from
	// FormatVersionPayloadSizes onward; zero on an older descriptor means "not recorded".
	RawSizeBytes int64 `json:"rawSizeBytes,omitempty"`
	// StoredSizeBytes is the stored size of the payload file. Same presence rules as RawSizeBytes.
	StoredSizeBytes int64 `json:"storedSizeBytes,omitempty"`
}

// descriptorWire is Descriptor without its JSON methods; it is the canonical encoding.
type descriptorWire Descriptor

// MarshalJSON encodes the sealed descriptor (see Seal): it defaults a nil ChildrenChecksum to
// EmptyChildrenChecksum and records the canonical MetadataChecksum. A descriptor that already
// declares an authenticated version (FormatVersionAuthenticatedChildren or later) keeps it, because
// its ChildrenChecksum was committed with that version's encoding and restamping it would pair the
// commitment with another encoding; any other descriptor, a new one included, is stamped
// FormatVersionCurrent. A descriptor read from an archive therefore encodes back unchanged.
func (d Descriptor) MarshalJSON() ([]byte, error) {
	version := FormatVersionCurrent

	switch d.FormatVersion {
	case FormatVersionAuthenticatedChildren, FormatVersionPayloadSizes, FormatVersionChildMetadata:
		version = d.FormatVersion
	}

	sealed, err := sealAt(d, version)
	if err != nil {
		return nil, err
	}

	data, err := json.Marshal(descriptorWire(sealed))
	if err != nil {
		return nil, fmt.Errorf("marshal versioned snapshot.yaml envelope: %w", err)
	}

	return data, nil
}

// UnmarshalJSON validates the envelope version and metadata integrity before exposing the
// fields. It fails closed on a missing or zero formatVersion; legacy callers use
// UnmarshalDescriptor with ReadOptions.AllowUnauthenticatedLegacy.
func (d *Descriptor) UnmarshalJSON(data []byte) error {
	var wire descriptorWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("unmarshal snapshot.yaml envelope: %w", err)
	}

	decoded := Descriptor(wire)
	if err := ValidateEnvelope(decoded, ReadOptions{}); err != nil {
		return err
	}

	*d = decoded

	return nil
}

// Seal returns d stamped with FormatVersionCurrent, with a nil ChildrenChecksum replaced by
// EmptyChildrenChecksum, and with MetadataChecksum set to its canonical value. Checksum and
// ChildrenChecksum must already hold the node's digests, the latter computed at
// FormatVersionCurrent (see ComputeChildrenChecksum).
func Seal(d Descriptor) (Descriptor, error) {
	return sealAt(d, FormatVersionCurrent)
}

// sealAt is Seal stamping formatVersion instead of FormatVersionCurrent. Writers never stamp an
// older version: it serves MarshalJSON, which keeps the version of a descriptor already sealed,
// and the tests that reproduce descriptors of the older versions.
func sealAt(d Descriptor, formatVersion int) (Descriptor, error) {
	d.FormatVersion = formatVersion
	d.MetadataChecksum = nil

	if d.ChildrenChecksum == nil {
		empty := EmptyChildrenChecksum()
		d.ChildrenChecksum = &empty
	}

	checksum, err := ComputeMetadataChecksum(d)
	if err != nil {
		return Descriptor{}, err
	}

	d.MetadataChecksum = &checksum

	return d, nil
}

// MarshalDescriptor encodes the sealed descriptor as snapshot.yaml bytes (see MarshalJSON).
func MarshalDescriptor(d Descriptor) ([]byte, error) {
	data, err := sigsyaml.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot.yaml: %w", err)
	}

	return data, nil
}

// UnmarshalDescriptor decodes snapshot.yaml bytes (YAML or JSON) under the caller-selected
// compatibility policy and validates the envelope (see ValidateEnvelope). The zero options value
// enforces the current authenticated envelope.
func UnmarshalDescriptor(data []byte, options ReadOptions) (Descriptor, error) {
	var wire descriptorWire
	if err := sigsyaml.Unmarshal(data, &wire); err != nil {
		return Descriptor{}, fmt.Errorf("unmarshal snapshot.yaml envelope: %w", err)
	}

	decoded := Descriptor(wire)
	if err := ValidateEnvelope(decoded, options); err != nil {
		return Descriptor{}, err
	}

	return decoded, nil
}

// ComputeMetadataChecksum computes the canonical metadata digest of d: SHA-256 over the compact
// encoding/json form of d with MetadataChecksum removed. The key order is the Descriptor field
// order, empty omitempty fields are dropped, and strings use encoding/json escaping (including
// <, > and & for <, > and &).
func ComputeMetadataChecksum(d Descriptor) (NodeChecksum, error) {
	d.MetadataChecksum = nil

	canonical, err := json.Marshal(descriptorWire(d))
	if err != nil {
		return NodeChecksum{}, fmt.Errorf("marshal canonical snapshot.yaml metadata: %w", err)
	}

	sum := sha256.Sum256(canonical)

	return checksumFromSum(sum[:]), nil
}

// ValidateEnvelope checks the version and integrity envelope of d: the format version is
// supported under options, ChildrenChecksum is present and well-formed, and on a versioned
// descriptor MetadataChecksum is present and equals ComputeMetadataChecksum(d).
func ValidateEnvelope(d Descriptor, options ReadOptions) error {
	switch d.FormatVersion {
	case FormatVersionLegacy:
		if !options.AllowUnauthenticatedLegacy {
			return fmt.Errorf("format version %d may indicate an archive downgrade: %w",
				d.FormatVersion, ErrLegacyFormat)
		}

		return validateChildrenChecksumPresent(d)
	case FormatVersionAuthenticatedChildren, FormatVersionPayloadSizes, FormatVersionChildMetadata:
	default:
		return fmt.Errorf("%d: %w", d.FormatVersion, ErrUnsupportedFormat)
	}

	if err := validateChildrenChecksumPresent(d); err != nil {
		return err
	}

	if d.MetadataChecksum == nil {
		return fmt.Errorf("format version %d requires metadataChecksum: %w",
			d.FormatVersion, ErrInvalidDescriptor)
	}

	if err := ValidateChecksum(*d.MetadataChecksum); err != nil {
		return fmt.Errorf("metadataChecksum: %w", err)
	}

	got, err := ComputeMetadataChecksum(d)
	if err != nil {
		return err
	}

	if got.Hex != d.MetadataChecksum.Hex {
		return fmt.Errorf("stored %q computed %q: %w",
			d.MetadataChecksum.Hex, got.Hex, ErrMetadataChecksumMismatch)
	}

	return nil
}

// validateChildrenChecksumPresent enforces the mandatory direct-child commitment. It applies to
// every format version, legacy included: AllowUnauthenticatedLegacy relaxes only the
// metadataChecksum requirement, never the child-topology commitment.
func validateChildrenChecksumPresent(d Descriptor) error {
	if d.ChildrenChecksum == nil {
		return fmt.Errorf("format version %d requires childrenChecksum: %w",
			d.FormatVersion, ErrInvalidDescriptor)
	}

	if err := ValidateChecksum(*d.ChildrenChecksum); err != nil {
		return fmt.Errorf("childrenChecksum: %w", err)
	}

	return nil
}

// ValidateDescriptor validates the envelope of d (see ValidateEnvelope) and its structural
// metadata.
//
// hasBlockData and hasFilesystemData report the node's volume payload (a block image or a
// filesystem tar); the caller derives them from the node's content. A node is a data node when it
// carries either payload. The rules:
//
//   - apiVersion, kind and name are required.
//   - checksum is well-formed (see ValidateChecksum).
//   - sourceObjectRef is all-or-nothing: omitted, or all of apiVersion/kind/name set.
//   - at most one volume.
//   - a data node carries exactly one volume with a complete target and artifact identity
//     (apiVersion/kind/name each), a storageClassName, a positive parseable size, and a
//     volumeMode that agrees with the payload kind (Block when hasBlockData, Filesystem
//     otherwise).
//   - a non-data node carries no volume.
func ValidateDescriptor(d Descriptor, hasBlockData, hasFilesystemData bool, options ReadOptions) error {
	if err := ValidateEnvelope(d, options); err != nil {
		return err
	}

	if d.APIVersion == "" || d.Kind == "" || d.Name == "" {
		return fmt.Errorf("apiVersion/kind/name are required (got apiVersion=%q kind=%q name=%q): %w",
			d.APIVersion, d.Kind, d.Name, ErrInvalidDescriptor)
	}

	if err := ValidateChecksum(d.Checksum); err != nil {
		return err
	}

	if ref := d.SourceObjectRef; ref != nil {
		if ref.APIVersion == "" || ref.Kind == "" || ref.Name == "" {
			return fmt.Errorf("sourceObjectRef must set all of apiVersion/kind/name or be omitted (got %+v): %w",
				*ref, ErrInvalidDescriptor)
		}
	}

	if len(d.Volumes) > 1 {
		return fmt.Errorf("a node carries at most one volume, got %d: %w", len(d.Volumes), ErrInvalidDescriptor)
	}

	if !hasBlockData && !hasFilesystemData {
		if len(d.Volumes) != 0 {
			return fmt.Errorf("non-data node carries %d volume(s) but has no data payload: %w",
				len(d.Volumes), ErrInvalidDescriptor)
		}

		return nil
	}

	if len(d.Volumes) != 1 {
		return fmt.Errorf("data node must carry exactly one volume, got %d: %w", len(d.Volumes), ErrInvalidDescriptor)
	}

	return validateDataVolume(d.Volumes[0], hasBlockData)
}

// validateDataVolume enforces the data-node volume invariants: complete target/artifact
// identity, a storageClassName, a positive parseable size, and a volumeMode that agrees with the
// payload kind (hasBlockData selects Block, otherwise Filesystem).
func validateDataVolume(v VolumeInfo, hasBlockData bool) error {
	if v.Target.APIVersion == "" || v.Target.Kind == "" || v.Target.Name == "" {
		return fmt.Errorf("data volume target identity is incomplete, apiVersion/kind/name required (got %+v): %w",
			v.Target, ErrInvalidDescriptor)
	}

	if v.Artifact.APIVersion == "" || v.Artifact.Kind == "" || v.Artifact.Name == "" {
		return fmt.Errorf("data volume artifact identity is incomplete, apiVersion/kind/name required (got %+v): %w",
			v.Artifact, ErrInvalidDescriptor)
	}

	if v.StorageClassName == "" {
		return fmt.Errorf("data volume storageClassName is required: %w", ErrInvalidDescriptor)
	}

	q, err := resource.ParseQuantity(v.Size)
	if err != nil {
		return fmt.Errorf("data volume size %q is not a valid quantity: %w", v.Size, errors.Join(ErrInvalidDescriptor, err))
	}

	if q.Sign() <= 0 {
		return fmt.Errorf("data volume size %q must be positive: %w", v.Size, ErrInvalidDescriptor)
	}

	want := VolumeModeFilesystem
	if hasBlockData {
		want = VolumeModeBlock
	}

	if v.VolumeMode != want {
		return fmt.Errorf("data volume volumeMode %q disagrees with the on-disk payload (want %q): %w",
			v.VolumeMode, want, ErrInvalidDescriptor)
	}

	return nil
}
