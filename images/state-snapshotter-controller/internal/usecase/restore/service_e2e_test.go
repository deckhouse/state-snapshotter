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

package restore

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/sds-common-lib/logger"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/transform"
)

func encodeChunk(objects []map[string]interface{}) (string, string) {
	jsonData, _ := json.Marshal(objects)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(jsonData)
	_ = gz.Close()
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	hash := sha256.Sum256(buf.Bytes())
	return encoded, hex.EncodeToString(hash[:])
}

// newVolumeLegRestoreService returns the restore compiler over the tree of newVolumeLegRestoreCluster,
// with no delegation configured: every node is resolved and compiled in-process. It is the shape that
// produced the pinned fixture below.
func newVolumeLegRestoreService(t *testing.T) *Service {
	t.Helper()
	cl := newVolumeLegRestoreCluster(t)
	log := logger.NewNop()
	arch := usecase.NewArchiveService(cl, cl, log)
	return NewService(cl, arch, nil, nil)
}

// newVolumeLegRestoreCluster seeds a namespace root "snap"/source-ns with two legs — a ConfigMap in its
// own manifests and a volume leg (a child VolumeSnapshot node whose own manifests carry the orphan PVC,
// bound to the durable VolumeSnapshotContent "vsc-orphan") — and returns a client over it.
// It is the smallest tree that exercises every step of the compiler chain: resolve run tree -> load MCP ->
// sanitize -> bind the PVC to its VolumeSnapshot -> marshal.
//
// The tree is seeded once and read by both shapes of the compiler — the in-process one and the delegating
// one — so the two cannot drift apart into "the same tree" that is not the same.
func newVolumeLegRestoreCluster(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = ssv1alpha1.AddToScheme(scheme)
	_ = storagev1alpha1.AddToScheme(scheme)
	vsGVK := schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}
	scheme.AddKnownTypeWithName(vsGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: vsGVK.Group, Version: vsGVK.Version, Kind: "VolumeSnapshotList"}, &unstructured.UnstructuredList{})

	// Variant A: the root MCP holds only the ConfigMap; the orphan PVC manifest lives on its own child
	// volume node's MCP (mcp-orphan), reachable via the VolumeSnapshot handle's boundSnapshotContentName.
	rootData, rootChecksum := encodeChunk([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "cfg", "namespace": "captured-elsewhere-ns"}, "data": map[string]interface{}{"k": "v"}},
	})
	rootChunk := &ssv1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: "chunk-root-0"},
		Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: "mcp-root", Index: 0, Data: rootData, Checksum: rootChecksum, ObjectsCount: 1,
		},
	}
	rootMCP := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-root", UID: types.UID("uid-mcp-root")},
		Spec:       ssv1alpha1.ManifestCheckpointSpec{},
		Status: ssv1alpha1.ManifestCheckpointStatus{
			Chunks:       []ssv1alpha1.ChunkInfo{{Name: "chunk-root-0", Index: 0, Checksum: rootChecksum}},
			TotalObjects: 1,
		},
	}
	meta.SetStatusCondition(&rootMCP.Status.Conditions, metav1.Condition{
		Type: ssv1alpha1.ManifestCheckpointConditionTypeReady, Status: metav1.ConditionTrue,
		Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
	})

	orphanData, orphanChecksum := encodeChunk([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]interface{}{"name": "orphan", "namespace": "captured-elsewhere-ns", "uid": "uid-orphan"}, "spec": map[string]interface{}{"accessModes": []interface{}{"ReadWriteOnce"}, "volumeName": "pv-x"}},
	})
	orphanChunk := &ssv1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: "chunk-orphan-0"},
		Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: "mcp-orphan", Index: 0, Data: orphanData, Checksum: orphanChecksum, ObjectsCount: 1,
		},
	}
	orphanMCP := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-orphan", UID: types.UID("uid-mcp-orphan")},
		Spec:       ssv1alpha1.ManifestCheckpointSpec{},
		Status: ssv1alpha1.ManifestCheckpointStatus{
			Chunks:       []ssv1alpha1.ChunkInfo{{Name: "chunk-orphan-0", Index: 0, Checksum: orphanChecksum}},
			TotalObjects: 1,
		},
	}
	meta.SetStatusCondition(&orphanMCP.Status.Conditions, metav1.Condition{
		Type: ssv1alpha1.ManifestCheckpointConditionTypeReady, Status: metav1.ConditionTrue,
		Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
	})

	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "Snapshot",
				Namespace:  "source-ns",
				Name:       "snap",
			},
		},
		Status: storagev1alpha1.SnapshotContentStatus{
			ManifestCheckpointName: "mcp-root",
		},
	}
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed"})

	orphanContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content-vol-orphan"},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: "snapshot.storage.k8s.io/v1",
				Kind:       "VolumeSnapshot",
				Namespace:  "source-ns",
				Name:       "vs-orphan",
			},
		},
		Status: storagev1alpha1.SnapshotContentStatus{
			ManifestCheckpointName: "mcp-orphan",
			Data: &storagev1alpha1.SnapshotDataBinding{
				SourceRef:   storagev1alpha1.SnapshotSubjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "orphan", Namespace: "source-ns", UID: "uid-orphan"},
				ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-orphan"},
			},
		},
	}
	meta.SetStatusCondition(&orphanContent.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed"})

	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "source-ns"},
		Status: storagev1alpha1.SnapshotStatus{
			BoundSnapshotContentName: "root-content",
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{
				{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "vs-orphan"},
			},
		},
	}
	meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed"})

	vs := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata":   map[string]interface{}{"name": "vs-orphan", "namespace": "source-ns"},
		"status":     map[string]interface{}{"boundVolumeSnapshotContentName": "vsc-orphan", "boundSnapshotContentName": "root-content-vol-orphan", "readyToUse": true},
	}}

	return fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(rootChunk, rootMCP, orphanChunk, orphanMCP, content, orphanContent, snap, vs).Build()
}

// volumeLegRestoreOptions addresses the namespace root of newVolumeLegRestoreService. There is no target
// namespace to choose: the output is addressed into the namespace of the node, always. The rewrite is
// still exercised, and not trivially — the captured manifests of this tree carry a THIRD namespace
// (captured-elsewhere-ns), the shape a tree loaded back into a cluster has, so an output in source-ns
// proves the compiler re-addressed it rather than passed it through.
func volumeLegRestoreOptions() Options {
	return Options{SnapshotName: "snap", SnapshotNamespace: "source-ns"}
}

// volumeLegRestoreOutput is the pinned response body of manifests-with-data-restoration for the tree of
// newVolumeLegRestoreService, captured from the compiler itself. Regenerating it is a deliberate act: see
// TestBuildManifestsWithDataRestoration_VolumeLegOutputIsByteStable.
//
//go:embed testdata/volume-leg-restore-output.json
var volumeLegRestoreOutput []byte

// TestBuildManifestsWithDataRestoration_VolumeLegOutputIsByteStable pins the EXACT bytes the compiler
// returns for a namespace root with a volume leg. This response is a client wire format — an external
// backup product and the web console apply these bytes as they arrive — so object order, field content and
// JSON shape are part of the contract, not an implementation detail. Restructuring the compiler must keep
// this test green; changing it means the wire format changed and the fixture is re-captured on purpose,
// together with the client-facing documentation of the subresource.
//
// Limit of this check: it pins ONE tree (a ConfigMap plus a volume-leg PVC bound to its VolumeSnapshot).
// It proves the emitted shape, the post-order (leg before parent) and the data binding — not every kind
// the restore-safe sanitizer touches; those are asserted case by case next to the sanitizer itself.
func TestBuildManifestsWithDataRestoration_VolumeLegOutputIsByteStable(t *testing.T) {
	svc := newVolumeLegRestoreService(t)

	out, err := svc.BuildManifestsWithDataRestoration(context.Background(), volumeLegRestoreOptions())
	if err != nil {
		t.Fatalf("BuildManifestsWithDataRestoration: %v", err)
	}
	if !bytes.Equal(out, volumeLegRestoreOutput) {
		t.Fatalf("compiled restore output changed.\n got: %s\nwant: %s", out, volumeLegRestoreOutput)
	}
}

// TestBuildManifestsWithDataRestoration_NamespaceRootOrphanPVC exercises the full restore compiler
// chain (resolve run tree -> load MCP -> sanitize -> transform -> marshal) for a namespace root with
// a ConfigMap and an orphan PVC. See volumeLegRestoreOptions for where the output is addressed and why
// that is not a tautology here.
func TestBuildManifestsWithDataRestoration_NamespaceRootOrphanPVC(t *testing.T) {
	svc := newVolumeLegRestoreService(t)

	out, err := svc.BuildManifestsWithDataRestoration(context.Background(), volumeLegRestoreOptions())
	if err != nil {
		t.Fatalf("BuildManifestsWithDataRestoration: %v", err)
	}

	var objects []map[string]interface{}
	if err := json.Unmarshal(out, &objects); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(objects) != 2 {
		t.Fatalf("expected 2 objects (ConfigMap + PVC), got %d: %s", len(objects), string(out))
	}

	var foundPVC bool
	for _, obj := range objects {
		u := unstructured.Unstructured{Object: obj}
		switch u.GetKind() {
		case "VolumeSnapshot", "VolumeSnapshotContent", "VolumeRestoreRequest":
			t.Fatalf("control-plane kind %s must not be emitted", u.GetKind())
		}
		if u.GetNamespace() != "source-ns" {
			t.Fatalf("%s namespace = %q, want source-ns", u.GetKind(), u.GetNamespace())
		}
		if u.GetKind() == "PersistentVolumeClaim" {
			foundPVC = true
			name, _, _ := unstructured.NestedString(u.Object, "spec", "dataSourceRef", "name")
			if name != "vs-orphan" {
				t.Fatalf("PVC dataSourceRef.name = %q, want vs-orphan", name)
			}
			if _, found, _ := unstructured.NestedString(u.Object, "spec", "volumeName"); found {
				t.Fatal("PVC spec.volumeName must be stripped")
			}
		}
	}
	if !foundPVC {
		t.Fatal("expected restored PVC in output")
	}
}

// --------------------------------------------------------------------------------------------------
// The volume leg after its subresources moved to the module that owns the kind
// --------------------------------------------------------------------------------------------------

// volumeNodeFacadeCall is one delegated request as the owner received it.
type volumeNodeFacadeCall struct {
	gvk       schema.GroupVersionKind
	namespace string
	name      string
}

// volumeNodeOwnerFacade stands in for the aggregated apiserver of the module that owns the CSI
// VolumeSnapshot — the server that answers manifests-with-data-restoration for a volume node now that this
// module delegates it instead of compiling it.
//
// It stands in for the TRANSPORT only, and that is what makes the byte comparison below worth anything.
// The compilation it runs is the shared one from the SDK (transform.CompileNodeManifests), and the captured
// objects it compiles are read through the same content-layer primitive the real facade fetches over HTTP
// (snapshotcontents/<name>/manifests-download). So the leg of the answer is produced by the shared code an
// owner in another module would run, and the bytes are still expected to be the ones recorded before the
// move.
//
// It DOES NOT call the core restore compiler — but being in the same package it could, and that is the one
// way to break this test into a tautology while making it look simpler: routing this facade through
// transformNodeObjects would compare core against core and prove nothing, however identical the two calls
// look today. Keep the SDK call spelled out here.
//
// What it does not reproduce: the aggregation layer (the answer crosses a JSON round trip in-process), and
// the facade's full set of refusals — it resolves the handle, checks it is usable and enforces the
// anti-spoofing back-reference, but the gates around a deleting handle or a not-Ready content are asserted
// where that facade lives, not here.
type volumeNodeOwnerFacade struct {
	reader client.Client
	core   *usecase.AggregatedNamespaceManifests
	calls  []volumeNodeFacadeCall
}

var _ DomainSubtreeRestorer = (*volumeNodeOwnerFacade)(nil)

// RestoreDomainSubtree answers one volume node the way its owner does: resolve the addressed
// VolumeSnapshot, check it is usable and that its bound SnapshotContent points back at it (anti-spoofing),
// fetch the captured objects from the snapshot core's content layer, compile them with the shared
// compilation, and hand back the JSON array — decoded again on this side, as core's delegate client does.
func (f *volumeNodeOwnerFacade) RestoreDomainSubtree(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) ([]unstructured.Unstructured, error) {
	f.calls = append(f.calls, volumeNodeFacadeCall{gvk: gvk, namespace: namespace, name: name})

	wantGVK := schema.GroupVersionKind{Group: snapshot.CSISnapshotGroup, Version: snapshot.CSISnapshotVersion, Kind: snapshot.KindVolumeSnapshot}
	if gvk != wantGVK {
		return nil, fmt.Errorf("owner: this apiserver serves %s, addressed as %s", wantGVK, gvk)
	}

	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(gvk)
	if err := f.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, vs); err != nil {
		return nil, fmt.Errorf("owner: get VolumeSnapshot %s/%s: %w", namespace, name, err)
	}
	if readyToUse, _, _ := unstructured.NestedBool(vs.Object, "status", "readyToUse"); !readyToUse {
		return nil, fmt.Errorf("owner: VolumeSnapshot %s/%s is not readyToUse", namespace, name)
	}
	boundVSC, _, _ := unstructured.NestedString(vs.Object, "status", "boundVolumeSnapshotContentName")
	contentName, _, _ := unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName")
	if boundVSC == "" || contentName == "" {
		return nil, fmt.Errorf("owner: VolumeSnapshot %s/%s is not bound yet (durable artifact %q, content %q)", namespace, name, boundVSC, contentName)
	}

	content := &storagev1alpha1.SnapshotContent{}
	if err := f.reader.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		return nil, fmt.Errorf("owner: get SnapshotContent %s: %w", contentName, err)
	}
	ref := content.Spec.SnapshotRef
	if ref == nil || ref.APIVersion != snapshot.CSISnapshotAPIVersion || ref.Kind != snapshot.KindVolumeSnapshot ||
		ref.Namespace != namespace || ref.Name != name {
		return nil, fmt.Errorf("owner: SnapshotContent %s does not point back at VolumeSnapshot %s/%s", contentName, namespace, name)
	}

	// The base is fetched, not recomputed: this is the byte-for-byte answer of the core content layer's
	// own download, which is what the facade receives over the aggregation layer.
	raw, err := f.core.BuildSingleNodeJSON(ctx, contentName)
	if err != nil {
		return nil, fmt.Errorf("owner: fetch the captured manifests of %s: %w", contentName, err)
	}
	captured, err := decodeOwnerManifests(raw)
	if err != nil {
		return nil, fmt.Errorf("owner: %w", err)
	}

	objects, err := transform.CompileNodeManifests(transform.NodeRestoreInput{
		CapturedManifests: captured,
		// The handle this request addresses lives in the request's namespace, and that is the only
		// namespace its subtree is compiled into.
		NodeNamespace:   namespace,
		PVCDataBindings: ownerPVCDataBindings(content.Status.Data),
		// This handle IS the visibility leaf of the volume its captured PVC used: the durable artifact the
		// data binding names is the content it is bound to, and this namespaced object is what a restored
		// PVC can reference.
		VolumeSnapshotByContentName: map[string]string{boundVSC: name},
		// The owner of a volume node recreates nothing itself, so every PVC in the node must be backed by a
		// data binding or the compilation fails closed.
		DomainCoveredPVCNames: nil,
	})
	if err != nil {
		return nil, err
	}

	// The answer crosses the aggregation layer as a JSON array and is decoded back by core's delegate
	// client, so the round trip happens here too: a wire format is only preserved if it survives being
	// written out and read back.
	body := make([]map[string]interface{}, 0, len(objects))
	for i := range objects {
		body = append(body, objects[i].Object)
	}
	wire, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("owner: serialize the compiled manifests: %w", err)
	}
	return decodeOwnerManifests(wire)
}

// decodeOwnerManifests decodes a JSON array of objects, as both the facade (reading the content layer's
// download) and core's delegate client (reading the facade's answer) do.
func decodeOwnerManifests(raw []byte) ([]unstructured.Unstructured, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var list []unstructured.Unstructured
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode the manifests array: %w", err)
	}
	return list, nil
}

// ownerPVCDataBindings projects the bound content's data binding onto the compilation's view of it, as the
// owner's facade does. Cardinality is at most one by design: a node carries at most one data artifact.
func ownerPVCDataBindings(data *storagev1alpha1.SnapshotDataBinding) []transform.PVCDataBinding {
	if data == nil {
		return nil
	}
	return []transform.PVCDataBinding{{
		TargetUID: string(data.SourceRef.UID),
		Target: storagev1alpha1.ObjectRef{
			APIVersion: data.SourceRef.APIVersion,
			Kind:       data.SourceRef.Kind,
			Name:       data.SourceRef.Name,
			Namespace:  data.SourceRef.Namespace,
			UID:        string(data.SourceRef.UID),
		},
		Artifact: storagev1alpha1.ObjectRef{
			APIVersion: data.ArtifactRef.APIVersion,
			Kind:       data.ArtifactRef.Kind,
			Name:       data.ArtifactRef.Name,
			UID:        string(data.ArtifactRef.UID),
		},
	}}
}

// TestBuildManifestsWithDataRestoration_DelegatedVolumeLegKeepsTheWireFormat walks the whole path a tree
// with a volume leg now takes — core compiles the nodes it owns, addresses the owner of the volume node,
// and splices the owner's already apply-ready answer — and compares the result against the SAME pinned
// bytes as the in-process compilation, recorded before the subresources moved to the other module.
//
// That is the one thing the move had to leave untouched: the response is applied as it arrives, by an
// external backup product and by the web console, so object order, field content and JSON shape are the
// client contract. Two servers now produce it instead of one, and this is what says the seam is invisible
// to the client.
//
// The delegated call is asserted as well, and that assertion is load-bearing rather than decorative: the
// fixtures of this tree are complete enough for core to resolve the volume leg itself, so a compiler that
// stopped delegating would emit the very same bytes. Without the call, this case would pass on exactly the
// regression it exists to catch.
//
// Limit of this check: one tree (a ConfigMap on the root, a volume leg with one PVC bound to its
// VolumeSnapshot), and the transport between the two servers is a JSON round trip in-process rather than
// the aggregation layer. The address a delegated call builds is asserted where the client is built (see
// the DomainRestoreClient specs in internal/api); two real apiservers are only reachable from the manual
// walkthrough.
func TestBuildManifestsWithDataRestoration_DelegatedVolumeLegKeepsTheWireFormat(t *testing.T) {
	cl := newVolumeLegRestoreCluster(t)
	log := logger.NewNop()
	arch := usecase.NewArchiveService(cl, cl, log)

	owner := &volumeNodeOwnerFacade{reader: cl, core: usecase.NewAggregatedNamespaceManifests(cl, arch, nil)}
	svc := NewService(cl, arch, owner, isOutOfProcessRestoreKind(t))

	out, err := svc.BuildManifestsWithDataRestoration(context.Background(), volumeLegRestoreOptions())
	if err != nil {
		t.Fatalf("BuildManifestsWithDataRestoration: %v", err)
	}

	if len(owner.calls) != 1 {
		t.Fatalf("expected the volume leg to be answered by its owner exactly once, got %d call(s)", len(owner.calls))
	}
	call := owner.calls[0]
	wantGVK := schema.GroupVersionKind{Group: snapshot.CSISnapshotGroup, Version: snapshot.CSISnapshotVersion, Kind: snapshot.KindVolumeSnapshot}
	if call.gvk != wantGVK {
		t.Fatalf("delegated identity = %v, want %v", call.gvk, wantGVK)
	}
	if call.namespace != "source-ns" || call.name != "vs-orphan" {
		t.Fatalf("delegated call = %s/%s, want source-ns/vs-orphan", call.namespace, call.name)
	}

	if !bytes.Equal(out, volumeLegRestoreOutput) {
		t.Fatalf("the delegated volume leg changed the response bytes.\n got: %s\nwant: %s", out, volumeLegRestoreOutput)
	}
}
