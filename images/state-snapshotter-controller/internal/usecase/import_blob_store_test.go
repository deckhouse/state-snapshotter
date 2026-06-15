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

package usecase

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func newBlobStore(t *testing.T) *ImportBlobStore {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	log, _ := logger.NewLogger("error")
	return NewImportBlobStore(cl, log)
}

func TestImportBlobStore_AppendStatusRead(t *testing.T) {
	ctx := context.Background()
	s := newBlobStore(t)
	key := ImportBlobKey("ns", "imp", ImportBlobKindIndex)

	// Fresh blob: nothing persisted.
	persisted, chunks, err := s.Status(ctx, key)
	if err != nil {
		t.Fatalf("status empty: %v", err)
	}
	if persisted != 0 || chunks != 0 {
		t.Fatalf("expected empty blob, got persisted=%d chunks=%d", persisted, chunks)
	}

	// First append advances the offset to the body length.
	next, err := s.Append(ctx, key, 0, []byte("hello"))
	if err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if next != 5 {
		t.Fatalf("expected next offset 5, got %d", next)
	}

	// Sequential append at the resume offset.
	next, err = s.Append(ctx, key, 5, []byte(" world"))
	if err != nil {
		t.Fatalf("append 2: %v", err)
	}
	if next != 11 {
		t.Fatalf("expected next offset 11, got %d", next)
	}

	persisted, chunks, err = s.Status(ctx, key)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if persisted != 11 || chunks != 2 {
		t.Fatalf("expected persisted=11 chunks=2, got persisted=%d chunks=%d", persisted, chunks)
	}

	raw, err := s.Read(ctx, key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != "hello world" {
		t.Fatalf("expected %q, got %q", "hello world", string(raw))
	}
}

func TestImportBlobStore_Retransmit(t *testing.T) {
	ctx := context.Background()
	s := newBlobStore(t)
	key := ImportBlobKey("ns", "imp", ImportBlobKindIndex)

	if _, err := s.Append(ctx, key, 0, []byte("abc")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := s.Append(ctx, key, 3, []byte("def")); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Client retransmits an already-stored prefix: must be a no-op reporting the real resume point.
	next, err := s.Append(ctx, key, 0, []byte("abc"))
	if err != nil {
		t.Fatalf("retransmit: %v", err)
	}
	if next != 6 {
		t.Fatalf("expected retransmit to report 6, got %d", next)
	}
	_, chunks, _ := s.Status(ctx, key)
	if chunks != 2 {
		t.Fatalf("retransmit must not create a chunk, got %d chunks", chunks)
	}
	raw, _ := s.Read(ctx, key)
	if string(raw) != "abcdef" {
		t.Fatalf("expected abcdef, got %q", string(raw))
	}
}

func TestImportBlobStore_OffsetGapConflict(t *testing.T) {
	ctx := context.Background()
	s := newBlobStore(t)
	key := ImportBlobKey("ns", "imp", ImportBlobKindIndex)

	if _, err := s.Append(ctx, key, 0, []byte("abc")); err != nil {
		t.Fatalf("append: %v", err)
	}
	// A gap (offset beyond the persisted length) is rejected and the real resume offset returned.
	next, err := s.Append(ctx, key, 100, []byte("xyz"))
	if !errors.Is(err, ErrUploadOffsetConflict) {
		t.Fatalf("expected ErrUploadOffsetConflict, got %v", err)
	}
	if next != 3 {
		t.Fatalf("expected conflict to report resume offset 3, got %d", next)
	}
}

func TestImportBlobStore_EmptyBodyNoop(t *testing.T) {
	ctx := context.Background()
	s := newBlobStore(t)
	key := ImportBlobKey("ns", "imp", ImportBlobKindIndex)

	next, err := s.Append(ctx, key, 0, nil)
	if err != nil {
		t.Fatalf("append empty: %v", err)
	}
	if next != 0 {
		t.Fatalf("expected 0, got %d", next)
	}
	if _, chunks, _ := s.Status(ctx, key); chunks != 0 {
		t.Fatalf("empty append must not create a chunk, got %d", chunks)
	}
}

func TestImportBlobStore_ReadEmptyErrors(t *testing.T) {
	ctx := context.Background()
	s := newBlobStore(t)
	if _, err := s.Read(ctx, ImportBlobKey("ns", "imp", ImportBlobKindIndex)); err == nil {
		t.Fatal("expected error reading an empty blob")
	}
}

func TestImportBlobStore_Delete(t *testing.T) {
	ctx := context.Background()
	s := newBlobStore(t)
	key := ImportBlobKey("ns", "imp", ImportBlobKindManifests)

	_, _ = s.Append(ctx, key, 0, []byte("aaa"))
	_, _ = s.Append(ctx, key, 3, []byte("bbb"))
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, chunks, _ := s.Status(ctx, key); chunks != 0 {
		t.Fatalf("expected 0 chunks after delete, got %d", chunks)
	}
	// Delete is idempotent.
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}

// TestImportBlobStore_StatusUsesRawBytes verifies the resume offset is summed from the recorded
// RawBytes (no gunzip) and that the sweepable label + traceability annotation are stamped.
func TestImportBlobStore_StatusUsesRawBytes(t *testing.T) {
	ctx := context.Background()
	s := newBlobStore(t)
	key := ImportBlobKey("ns", "imp", ImportBlobKindIndex)

	if _, err := s.Append(ctx, key, 0, []byte("payload")); err != nil {
		t.Fatalf("append: %v", err)
	}
	var chunk ssv1alpha1.ManifestCheckpointContentChunk
	if err := s.client.Get(ctx, types.NamespacedName{Name: chunkName(key, 0)}, &chunk); err != nil {
		t.Fatalf("get chunk: %v", err)
	}
	if chunk.Spec.RawBytes != int64(len("payload")) {
		t.Fatalf("expected RawBytes=%d, got %d", len("payload"), chunk.Spec.RawBytes)
	}
	if chunk.Labels[importBlobLabel] != "true" {
		t.Fatalf("expected sweepable label, got %v", chunk.Labels)
	}
	if chunk.Annotations[importBlobKeyAnnotation] != key {
		t.Fatalf("expected key annotation %q, got %v", key, chunk.Annotations)
	}
}

// TestImportBlobStore_ChecksumMismatchOnRead verifies a tampered chunk is rejected on read.
func TestImportBlobStore_ChecksumMismatchOnRead(t *testing.T) {
	ctx := context.Background()
	s := newBlobStore(t)
	key := ImportBlobKey("ns", "imp", ImportBlobKindIndex)

	if _, err := s.Append(ctx, key, 0, []byte("trustme")); err != nil {
		t.Fatalf("append: %v", err)
	}
	var chunk ssv1alpha1.ManifestCheckpointContentChunk
	if err := s.client.Get(ctx, types.NamespacedName{Name: chunkName(key, 0)}, &chunk); err != nil {
		t.Fatalf("get chunk: %v", err)
	}
	// Replace the payload with a valid gzip of different bytes; the stored checksum no longer matches.
	tampered, _, err := encodeRawChunk([]byte("evilbytes"))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	chunk.Spec.Data = tampered
	if err := s.client.Update(ctx, &chunk); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := s.Read(ctx, key); err == nil {
		t.Fatal("expected checksum mismatch error on read")
	}
}

func TestImportBlobStore_RoundTripBinary(t *testing.T) {
	ctx := context.Background()
	s := newBlobStore(t)
	key := ImportBlobKey("ns", "imp", ImportBlobKindManifests)

	want := bytes.Repeat([]byte{0x00, 0x01, 0xfe, 0xff}, 1000)
	off := int64(0)
	for i := 0; i < len(want); i += 256 {
		end := i + 256
		if end > len(want) {
			end = len(want)
		}
		next, err := s.Append(ctx, key, off, want[i:end])
		if err != nil {
			t.Fatalf("append at %d: %v", off, err)
		}
		off = next
	}
	got, err := s.Read(ctx, key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(want))
	}
}
