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
	"strings"
	"testing"

	sigsyaml "sigs.k8s.io/yaml"
)

// validChecksum returns a well-formed NodeChecksum (sha256, 64 lowercase hex, consistent short).
func validChecksum() NodeChecksum {
	hex := strings.Repeat("a", 64)

	return NodeChecksum{Algorithm: ChecksumAlgorithmSHA256, Hex: hex, Short: ShortChecksum(hex)}
}

// validChildrenChecksum returns the empty-child-set commitment, for descriptors that only need
// a structurally valid ChildrenChecksum.
func validChildrenChecksum() *NodeChecksum {
	checksum := EmptyChildrenChecksum()

	return &checksum
}

// validVolume returns a complete data VolumeInfo whose volumeMode is mode.
func validVolume(mode string) VolumeInfo {
	return VolumeInfo{
		Target:           VolumeObjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-1"},
		Artifact:         VolumeObjectRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "content-1"},
		VolumeMode:       mode,
		StorageClassName: "sc",
		Size:             "1Gi",
	}
}

func mustMarshal(t *testing.T, d Descriptor) []byte {
	t.Helper()

	data, err := MarshalDescriptor(d)
	if err != nil {
		t.Fatalf("MarshalDescriptor: %v", err)
	}

	return data
}

func rewriteDescriptorMap(t *testing.T, data []byte, rewrite func(map[string]interface{})) []byte {
	t.Helper()

	var fields map[string]interface{}
	if err := sigsyaml.Unmarshal(data, &fields); err != nil {
		t.Fatalf("unmarshal descriptor map: %v", err)
	}

	rewrite(fields)

	rewritten, err := sigsyaml.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal descriptor map: %v", err)
	}

	return rewritten
}

// TestValidateDescriptor exercises the structural invariants ValidateDescriptor enforces.
func TestValidateDescriptor(t *testing.T) {
	t.Parallel()

	base := func() Descriptor {
		return Descriptor{
			APIVersion:       "g/v1",
			Kind:             "Snapshot",
			Name:             "root",
			Checksum:         validChecksum(),
			ChildrenChecksum: validChildrenChecksum(),
		}
	}

	tests := []struct {
		name     string
		mutate   func(d *Descriptor)
		hasBlock bool
		hasFS    bool
		wantErr  bool
	}{
		{name: "valid: non-data structural node", mutate: func(*Descriptor) {}},
		{
			name:     "valid: block data node",
			mutate:   func(d *Descriptor) { d.Volumes = []VolumeInfo{validVolume(VolumeModeBlock)} },
			hasBlock: true,
		},
		{
			name:   "valid: filesystem data node",
			mutate: func(d *Descriptor) { d.Volumes = []VolumeInfo{validVolume(VolumeModeFilesystem)} },
			hasFS:  true,
		},
		{
			name: "valid: complete sourceObjectRef",
			mutate: func(d *Descriptor) {
				d.SourceObjectRef = &SourceObjectRef{APIVersion: "demo/v1", Kind: "Disk", Name: "d1"}
			},
		},
		{name: "error: missing apiVersion", mutate: func(d *Descriptor) { d.APIVersion = "" }, wantErr: true},
		{name: "error: missing kind", mutate: func(d *Descriptor) { d.Kind = "" }, wantErr: true},
		{name: "error: missing name", mutate: func(d *Descriptor) { d.Name = "" }, wantErr: true},
		{name: "error: bad checksum algorithm", mutate: func(d *Descriptor) { d.Checksum.Algorithm = "md5" }, wantErr: true},
		{
			name:    "error: short hex",
			mutate:  func(d *Descriptor) { d.Checksum.Hex = "abcd"; d.Checksum.Short = "abcd" },
			wantErr: true,
		},
		{
			name: "error: uppercase hex",
			mutate: func(d *Descriptor) {
				d.Checksum.Hex = strings.Repeat("A", 64)
				d.Checksum.Short = strings.Repeat("A", 8)
			},
			wantErr: true,
		},
		{name: "error: inconsistent short", mutate: func(d *Descriptor) { d.Checksum.Short = "deadbeef" }, wantErr: true},
		{
			name: "error: partial sourceObjectRef",
			mutate: func(d *Descriptor) {
				d.SourceObjectRef = &SourceObjectRef{APIVersion: "demo/v1", Name: "d1"}
			},
			wantErr: true,
		},
		{
			name: "error: two volumes",
			mutate: func(d *Descriptor) {
				d.Volumes = []VolumeInfo{validVolume(VolumeModeBlock), validVolume(VolumeModeBlock)}
			},
			hasBlock: true,
			wantErr:  true,
		},
		{
			name:    "error: non-data node carries a volume",
			mutate:  func(d *Descriptor) { d.Volumes = []VolumeInfo{validVolume(VolumeModeBlock)} },
			wantErr: true,
		},
		{name: "error: data node with zero volumes", mutate: func(*Descriptor) {}, hasBlock: true, wantErr: true},
		{
			name: "error: data volume missing target identity",
			mutate: func(d *Descriptor) {
				v := validVolume(VolumeModeBlock)
				v.Target.Name = ""
				d.Volumes = []VolumeInfo{v}
			},
			hasBlock: true,
			wantErr:  true,
		},
		{
			name: "error: data volume missing artifact identity",
			mutate: func(d *Descriptor) {
				v := validVolume(VolumeModeBlock)
				v.Artifact.APIVersion = ""
				d.Volumes = []VolumeInfo{v}
			},
			hasBlock: true,
			wantErr:  true,
		},
		{
			name: "error: data volume missing storageClassName",
			mutate: func(d *Descriptor) {
				v := validVolume(VolumeModeBlock)
				v.StorageClassName = ""
				d.Volumes = []VolumeInfo{v}
			},
			hasBlock: true,
			wantErr:  true,
		},
		{
			name: "error: data volume unparseable size",
			mutate: func(d *Descriptor) {
				v := validVolume(VolumeModeBlock)
				v.Size = "not-a-quantity"
				d.Volumes = []VolumeInfo{v}
			},
			hasBlock: true,
			wantErr:  true,
		},
		{
			name: "error: data volume zero size",
			mutate: func(d *Descriptor) {
				v := validVolume(VolumeModeBlock)
				v.Size = "0"
				d.Volumes = []VolumeInfo{v}
			},
			hasBlock: true,
			wantErr:  true,
		},
		{
			name:     "error: block payload with Filesystem volumeMode",
			mutate:   func(d *Descriptor) { d.Volumes = []VolumeInfo{validVolume(VolumeModeFilesystem)} },
			hasBlock: true,
			wantErr:  true,
		},
		{
			name:    "error: filesystem payload with Block volumeMode",
			mutate:  func(d *Descriptor) { d.Volumes = []VolumeInfo{validVolume(VolumeModeBlock)} },
			hasFS:   true,
			wantErr: true,
		},
		{
			name:     "error: block payload with empty volumeMode",
			mutate:   func(d *Descriptor) { d.Volumes = []VolumeInfo{validVolume("")} },
			hasBlock: true,
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := base()
			tc.mutate(&d)

			err := ValidateDescriptor(d, tc.hasBlock, tc.hasFS, ReadOptions{AllowUnauthenticatedLegacy: true})

			if tc.wantErr {
				if !errors.Is(err, ErrInvalidDescriptor) {
					t.Fatalf("expected ErrInvalidDescriptor, got: %v", err)
				}

				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			// The same descriptor, sealed, must also pass under the strict default policy.
			sealed, err := Seal(d)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}

			if err := ValidateDescriptor(sealed, tc.hasBlock, tc.hasFS, ReadOptions{}); err != nil {
				t.Errorf("sealed descriptor: unexpected error: %v", err)
			}
		})
	}
}

func TestUnmarshalDescriptorEnvelopeCompatibility(t *testing.T) {
	t.Parallel()

	empty := EmptyChildrenChecksum()
	legacyChildren := fmt.Sprintf("childrenChecksum: {algorithm: %s, hex: %q, short: %q}\n",
		empty.Algorithm, empty.Hex, empty.Short)

	tests := []struct {
		name        string
		data        []byte
		wantVersion int
		wantErr     error
		// legacyErr is the error with AllowUnauthenticatedLegacy; nil means it must succeed.
		legacyErr error
	}{
		{
			name: "current version round trip",
			data: mustMarshal(t, Descriptor{
				APIVersion: "snapshot.example.io/v1", Kind: "Snapshot", Name: "current", Checksum: validChecksum(),
			}),
			wantVersion: FormatVersionCurrent,
		},
		{
			name: "unknown future version, even with the legacy opt-in",
			data: []byte(fmt.Sprintf("formatVersion: %d\napiVersion: snapshot.example.io/v1\nkind: Snapshot\nname: future\n%s",
				FormatVersionCurrent+1, legacyChildren)),
			wantErr:   ErrUnsupportedFormat,
			legacyErr: ErrUnsupportedFormat,
		},
		{
			name:        "legacy unversioned descriptor requires explicit compatibility",
			data:        []byte("apiVersion: snapshot.example.io/v1\nkind: Snapshot\nname: legacy\n" + legacyChildren),
			wantVersion: FormatVersionLegacy,
			wantErr:     ErrLegacyFormat,
		},
		{
			name: "explicit zero version requires explicit compatibility",
			data: []byte("{name: legacy, kind: Snapshot, formatVersion: 0, apiVersion: snapshot.example.io/v1, " +
				fmt.Sprintf("childrenChecksum: {algorithm: %s, hex: %q, short: %q}}\n", empty.Algorithm, empty.Hex, empty.Short)),
			wantVersion: FormatVersionLegacy,
			wantErr:     ErrLegacyFormat,
		},
		{
			name: "current version without childrenChecksum is never accepted",
			data: []byte(fmt.Sprintf("formatVersion: %d\napiVersion: snapshot.example.io/v1\nkind: Snapshot\nname: current\n",
				FormatVersionCurrent)),
			wantErr:   ErrInvalidDescriptor,
			legacyErr: ErrInvalidDescriptor,
		},
		{
			name:      "legacy descriptor without childrenChecksum is never accepted",
			data:      []byte("apiVersion: snapshot.example.io/v1\nkind: Snapshot\nname: legacy\n"),
			wantErr:   ErrLegacyFormat,
			legacyErr: ErrInvalidDescriptor,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := UnmarshalDescriptor(tt.data, ReadOptions{})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("UnmarshalDescriptor error = %v, want %v", err, tt.wantErr)
				}

				got, err = UnmarshalDescriptor(tt.data, ReadOptions{AllowUnauthenticatedLegacy: true})
				if tt.legacyErr != nil {
					if !errors.Is(err, tt.legacyErr) {
						t.Fatalf("UnmarshalDescriptor(AllowUnauthenticatedLegacy) error = %v, want %v", err, tt.legacyErr)
					}

					return
				}
			}

			if err != nil {
				t.Fatalf("UnmarshalDescriptor: %v", err)
			}

			if got.FormatVersion != tt.wantVersion {
				t.Errorf("FormatVersion = %d, want %d", got.FormatVersion, tt.wantVersion)
			}

			if (got.MetadataChecksum != nil) != (tt.wantVersion != FormatVersionLegacy) {
				t.Errorf("MetadataChecksum presence = %t on version %d", got.MetadataChecksum != nil, got.FormatVersion)
			}
		})
	}
}

func TestUnmarshalDescriptorRejectsDowngradeVariants(t *testing.T) {
	t.Parallel()

	current := mustMarshal(t, Descriptor{
		APIVersion: "snapshot.example.io/v1", Kind: "Snapshot", Name: "original", Checksum: validChecksum(),
	})

	tests := []struct {
		name    string
		rewrite func(map[string]interface{})
		wantErr error
	}{
		{
			name: "omit formatVersion and metadataChecksum after semantic mutation",
			rewrite: func(fields map[string]interface{}) {
				delete(fields, "formatVersion")
				delete(fields, "metadataChecksum")
				fields["name"] = "tampered"
			},
			wantErr: ErrLegacyFormat,
		},
		{
			name: "set version zero and omit metadataChecksum",
			rewrite: func(fields map[string]interface{}) {
				fields["formatVersion"] = float64(0)
				delete(fields, "metadataChecksum")
			},
			wantErr: ErrLegacyFormat,
		},
		{
			name:    "current version without metadataChecksum",
			rewrite: func(fields map[string]interface{}) { delete(fields, "metadataChecksum") },
			wantErr: ErrInvalidDescriptor,
		},
		{
			name:    "current version with stale checksum after semantic mutation",
			rewrite: func(fields map[string]interface{}) { fields["name"] = "tampered" },
			wantErr: ErrMetadataChecksumMismatch,
		},
		{
			name: "downgrade to version 2 keeps the checksum stale",
			rewrite: func(fields map[string]interface{}) {
				fields["formatVersion"] = float64(FormatVersionAuthenticatedChildren)
			},
			wantErr: ErrMetadataChecksumMismatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := UnmarshalDescriptor(rewriteDescriptorMap(t, current, tt.rewrite), ReadOptions{})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("UnmarshalDescriptor error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestUnmarshalJSONFailsClosed proves the json.Unmarshaler path enforces the strict envelope.
func TestUnmarshalJSONFailsClosed(t *testing.T) {
	t.Parallel()

	var d Descriptor
	if err := json.Unmarshal([]byte(`{"apiVersion":"v1","kind":"Snapshot","name":"x"}`), &d); !errors.Is(err, ErrLegacyFormat) {
		t.Fatalf("json.Unmarshal error = %v, want ErrLegacyFormat", err)
	}

	data, err := json.Marshal(Descriptor{APIVersion: "v1", Kind: "Snapshot", Name: "x", Checksum: validChecksum()})
	if err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("json.Unmarshal of a marshaled descriptor: %v", err)
	}
}

func TestMarshalDescriptorRoundTrip(t *testing.T) {
	t.Parallel()

	want := Descriptor{
		APIVersion:      "demo.deckhouse.io/v1alpha1",
		Kind:            "DemoVirtualDiskSnapshot",
		Name:            "snap-1",
		Namespace:       "ns-a",
		UID:             "uid-1",
		SourceName:      "my-disk",
		SourceObjectRef: &SourceObjectRef{APIVersion: "demo.deckhouse.io/v1alpha1", Kind: "DemoVirtualDisk", Name: "my-disk"},
		Checksum:        validChecksum(),
		Volumes: []VolumeInfo{{
			Target:           VolumeObjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-v3", Namespace: "ns-a", UID: "abc"},
			Artifact:         VolumeObjectRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-v3"},
			VolumeMode:       VolumeModeBlock,
			StorageClassName: "sc-thin",
			Size:             "1Gi",
			RawSizeBytes:     1077665792,
			StoredSizeBytes:  900000000,
		}},
	}

	got, err := UnmarshalDescriptor(mustMarshal(t, want), ReadOptions{})
	if err != nil {
		t.Fatalf("UnmarshalDescriptor: %v", err)
	}

	if got.FormatVersion != FormatVersionCurrent {
		t.Errorf("FormatVersion = %d, want %d", got.FormatVersion, FormatVersionCurrent)
	}

	if got.ChildrenChecksum == nil || got.ChildrenChecksum.Hex != EmptyChildrenChecksum().Hex {
		t.Errorf("ChildrenChecksum = %+v, want the empty commitment", got.ChildrenChecksum)
	}

	if got.MetadataChecksum == nil {
		t.Fatal("MetadataChecksum must be set on a current-format write")
	}

	if err := ValidateChecksum(*got.MetadataChecksum); err != nil {
		t.Errorf("MetadataChecksum invalid: %v", err)
	}

	got.FormatVersion, got.ChildrenChecksum, got.MetadataChecksum = 0, nil, nil

	gotJSON, _ := json.Marshal(descriptorWire(got))
	wantJSON, _ := json.Marshal(descriptorWire(want))

	if string(gotJSON) != string(wantJSON) {
		t.Errorf("round trip changed the descriptor:\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

func TestMarshalDescriptorOmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	data := string(mustMarshal(t, Descriptor{
		APIVersion: "state-snapshotter.deckhouse.io/v1alpha1", Kind: "Snapshot", Name: "snap", Checksum: validChecksum(),
	}))

	for _, key := range []string{"volumes:", "sourceName:", "sourceObjectRef:", "namespace:", "uid:"} {
		if strings.Contains(data, key) {
			t.Errorf("marshaled descriptor contains %q for an empty field:\n%s", key, data)
		}
	}

	for _, key := range []string{"formatVersion:", "childrenChecksum:", "metadataChecksum:"} {
		if !strings.Contains(data, key) {
			t.Errorf("marshaled descriptor lacks %q:\n%s", key, data)
		}
	}
}

// TestVolumeInfoZeroSizesOmittedFromJSON proves RawSizeBytes/StoredSizeBytes at their zero value
// are omitted from JSON entirely, which keeps the metadata checksum of a version 2 descriptor
// (whose JSON never had these keys) stable.
func TestVolumeInfoZeroSizesOmittedFromJSON(t *testing.T) {
	t.Parallel()

	vol := validVolume(VolumeModeBlock)

	data, err := json.Marshal(vol)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	for _, key := range []string{"rawSizeBytes", "storedSizeBytes"} {
		if strings.Contains(string(data), key) {
			t.Errorf("marshaled VolumeInfo contains key %q with a zero value: %s", key, data)
		}
	}

	vol.RawSizeBytes = 42

	data, err = json.Marshal(vol)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	if !strings.Contains(string(data), `"rawSizeBytes":42`) {
		t.Errorf("marshaled VolumeInfo with RawSizeBytes=42 must contain the key: %s", data)
	}
}

// descriptorShadowV2 is the version 2 wire shape, without rawSizeBytes/storedSizeBytes: exactly
// what a pre-version-3 writer produced.
type descriptorShadowV2 struct {
	FormatVersion    int              `json:"formatVersion,omitempty"`
	APIVersion       string           `json:"apiVersion"`
	Kind             string           `json:"kind"`
	Name             string           `json:"name"`
	Namespace        string           `json:"namespace,omitempty"`
	UID              string           `json:"uid,omitempty"`
	SourceName       string           `json:"sourceName,omitempty"`
	SourceObjectRef  *SourceObjectRef `json:"sourceObjectRef,omitempty"`
	Checksum         NodeChecksum     `json:"checksum"`
	ChildrenChecksum *NodeChecksum    `json:"childrenChecksum,omitempty"`
	MetadataChecksum *NodeChecksum    `json:"metadataChecksum,omitempty"`
	Volumes          []volumeShadowV2 `json:"volumes,omitempty"`
}

type volumeShadowV2 struct {
	Target           VolumeObjectRef `json:"target"`
	Artifact         VolumeObjectRef `json:"artifact"`
	VolumeMode       string          `json:"volumeMode,omitempty"`
	StorageClassName string          `json:"storageClassName,omitempty"`
	Size             string          `json:"size,omitempty"`
}

// TestVersion2DescriptorStillValidates reproduces a version 2 descriptor byte for byte and proves
// it still decodes and validates.
func TestVersion2DescriptorStillValidates(t *testing.T) {
	t.Parallel()

	children := EmptyChildrenChecksum()
	shadow := descriptorShadowV2{
		FormatVersion:    FormatVersionAuthenticatedChildren,
		APIVersion:       "snapshot.storage.k8s.io/v1",
		Kind:             "VolumeSnapshot",
		Name:             "pre-v3-node",
		Namespace:        "ns-legacy",
		Checksum:         validChecksum(),
		ChildrenChecksum: &children,
		Volumes: []volumeShadowV2{{
			Target:           VolumeObjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-legacy"},
			Artifact:         VolumeObjectRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-legacy"},
			VolumeMode:       VolumeModeBlock,
			StorageClassName: "sc-legacy",
			Size:             "1Gi",
		}},
	}

	canonical, err := json.Marshal(shadow)
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(canonical)
	metadata := checksumFromSum(sum[:])
	shadow.MetadataChecksum = &metadata

	final, err := json.Marshal(shadow)
	if err != nil {
		t.Fatal(err)
	}

	got, err := UnmarshalDescriptor(final, ReadOptions{})
	if err != nil {
		t.Fatalf("a version 2 descriptor failed to decode: %v", err)
	}

	if got.FormatVersion != FormatVersionAuthenticatedChildren {
		t.Errorf("FormatVersion = %d, want %d", got.FormatVersion, FormatVersionAuthenticatedChildren)
	}

	if got.Volumes[0].RawSizeBytes != 0 || got.Volumes[0].StoredSizeBytes != 0 {
		t.Errorf("version 2 payload sizes must read back as zero, got %d/%d",
			got.Volumes[0].RawSizeBytes, got.Volumes[0].StoredSizeBytes)
	}

	if err := ValidateDescriptor(got, true, false, ReadOptions{}); err != nil {
		t.Errorf("ValidateDescriptor: %v", err)
	}
}

// TestMetadataChecksumCanonicalizesEmptyValues pins how the canonical form treats a field that is
// written but empty: an explicit null, "~", "" or [] is the same as the field being absent,
// because the digest is taken over the decoded descriptor re-encoded, not over the file bytes.
func TestMetadataChecksumCanonicalizesEmptyValues(t *testing.T) {
	t.Parallel()

	base := mustMarshal(t, Descriptor{
		APIVersion: "state-snapshotter.deckhouse.io/v1alpha1", Kind: "Snapshot", Name: "snap", Checksum: validChecksum(),
	})

	for _, extra := range []string{
		"sourceName: null\n", "sourceName: ~\n", "sourceName: \"\"\n", "namespace: \"\"\n",
		"sourceObjectRef: null\n", "volumes: []\n", "volumes: null\n",
	} {
		if _, err := UnmarshalDescriptor(append([]byte(extra), base...), ReadOptions{}); err != nil {
			t.Errorf("%q: %v", strings.TrimSpace(extra), err)
		}
	}

	quoted := strings.Replace(string(base), fmt.Sprintf("formatVersion: %d", FormatVersionCurrent),
		fmt.Sprintf("formatVersion: \"%d\"", FormatVersionCurrent), 1)
	if _, err := UnmarshalDescriptor([]byte(quoted), ReadOptions{}); err == nil {
		t.Error("a quoted formatVersion must be refused")
	}
}
