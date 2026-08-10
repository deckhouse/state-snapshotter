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

package snapshotcontent

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func enrichScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	return scheme
}

func fsMode() *corev1.PersistentVolumeMode    { m := corev1.PersistentVolumeFilesystem; return &m }
func blockMode() *corev1.PersistentVolumeMode { m := corev1.PersistentVolumeBlock; return &m }
func scPtr(s string) *string                  { return &s }

// pvcTargetBinding is a binding whose source is the PVC named name. Its SourceRef.UID follows the same
// "uid-<name>" convention as sourcePVC below, because the enricher verifies volume IDENTITY before reading
// metadata off a live PVC: a name match with a different UID is a different volume and is skipped.
func pvcTargetBinding(name string) storagev1alpha1.SnapshotDataBinding {
	return storagev1alpha1.SnapshotDataBinding{
		SourceRef: storagev1alpha1.SnapshotSubjectRef{UID: types.UID(pvcFixtureUID(name)), Kind: "PersistentVolumeClaim", Namespace: "ns1", Name: name},
	}
}

// pvcFixtureUID pairs a fixture PVC with the binding that references it. A real PVC always carries a UID, so
// the fixtures do too — an object without one would make the identity check unverifiable and hide it.
func pvcFixtureUID(name string) string { return "uid-" + name }

// sourcePVC builds the live source PVC a pvcTargetBinding(name) points at, with the matching UID.
func sourcePVC(name string, spec corev1.PersistentVolumeClaimSpec) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: name, UID: types.UID(pvcFixtureUID(name))},
		Spec:       spec,
	}
}

func TestEnrich_FilesystemPVCWithCSIPV(t *testing.T) {
	ctx := context.Background()
	scheme := enrichScheme(t)
	pvc := sourcePVC("data", corev1.PersistentVolumeClaimSpec{
		VolumeMode:       fsMode(),
		StorageClassName: scPtr("fast"),
		VolumeName:       "pv-data",
	})
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-data"},
		Spec:       corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "d", VolumeHandle: "h", FSType: "xfs"}}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc, pv).Build()

	out, err := EnrichDataBindingsWithVolumeMetadata(ctx, cl, cl, []storagev1alpha1.SnapshotDataBinding{pvcTargetBinding("data")})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	b := out[0]
	if b.VolumeMode != "Filesystem" {
		t.Errorf("volumeMode: want Filesystem, got %q", b.VolumeMode)
	}
	if b.FsType != "xfs" {
		t.Errorf("fsType: want xfs, got %q", b.FsType)
	}
	if b.StorageClassName != "fast" {
		t.Errorf("storageClassName: want fast, got %q", b.StorageClassName)
	}
}

func TestEnrich_BlockPVCSkipsPV(t *testing.T) {
	ctx := context.Background()
	scheme := enrichScheme(t)
	pvc := sourcePVC("blk", corev1.PersistentVolumeClaimSpec{VolumeMode: blockMode(), VolumeName: "pv-blk"})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()
	// The PV is intentionally absent: a Block volume must not read it. If it did, the missing PV would
	// surface as an error, so a nil error proves the PV read was skipped.
	out, err := EnrichDataBindingsWithVolumeMetadata(ctx, cl, cl, []storagev1alpha1.SnapshotDataBinding{pvcTargetBinding("blk")})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if out[0].VolumeMode != "Block" {
		t.Errorf("volumeMode: want Block, got %q", out[0].VolumeMode)
	}
	if out[0].FsType != "" {
		t.Errorf("fsType must be empty for Block, got %q", out[0].FsType)
	}
}

func TestEnrich_NilVolumeModeDefaultsFilesystem(t *testing.T) {
	ctx := context.Background()
	scheme := enrichScheme(t)
	pvc := sourcePVC("nm", corev1.PersistentVolumeClaimSpec{})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()
	out, err := EnrichDataBindingsWithVolumeMetadata(ctx, cl, cl, []storagev1alpha1.SnapshotDataBinding{pvcTargetBinding("nm")})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if out[0].VolumeMode != "Filesystem" {
		t.Errorf("nil volumeMode must default to Filesystem, got %q", out[0].VolumeMode)
	}
}

func TestEnrich_MissingPVCTolerated(t *testing.T) {
	ctx := context.Background()
	scheme := enrichScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	in := []storagev1alpha1.SnapshotDataBinding{pvcTargetBinding("gone")}
	out, err := EnrichDataBindingsWithVolumeMetadata(ctx, cl, cl, in)
	if err != nil {
		t.Fatalf("a genuinely-gone source PVC must be tolerated, got error: %v", err)
	}
	if out[0].VolumeMode != "" {
		t.Errorf("binding metadata must be left empty for a gone PVC, got %q", out[0].VolumeMode)
	}
}

func TestEnrich_PVReadErrorReturned(t *testing.T) {
	ctx := context.Background()
	scheme := enrichScheme(t)
	pvc := sourcePVC("data", corev1.PersistentVolumeClaimSpec{VolumeMode: fsMode(), VolumeName: "pv-data"})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()
	// A direct reader whose PV Get fails (simulates the RBAC/transient failure that must NOT be
	// silently swallowed): enrichment returns the error so the caller requeues instead of publishing
	// a binding with an empty fsType.
	directFail := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.PersistentVolume); ok {
				return fmt.Errorf("forbidden: PV list")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()

	_, err := EnrichDataBindingsWithVolumeMetadata(ctx, cl, directFail, []storagev1alpha1.SnapshotDataBinding{pvcTargetBinding("data")})
	if err == nil {
		t.Fatal("expected a PV read error to be returned, not swallowed")
	}
}

func TestEnrich_NonPVCTargetSkipped(t *testing.T) {
	ctx := context.Background()
	scheme := enrichScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	binding := storagev1alpha1.SnapshotDataBinding{
		SourceRef: storagev1alpha1.SnapshotSubjectRef{UID: "uid-x", Kind: "DemoVirtualDisk", Namespace: "ns1", Name: "disk"},
	}
	out, err := EnrichDataBindingsWithVolumeMetadata(ctx, cl, cl, []storagev1alpha1.SnapshotDataBinding{binding})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if out[0].VolumeMode != "" {
		t.Errorf("non-PVC target must be skipped, got volumeMode %q", out[0].VolumeMode)
	}
}

// vscWithRestoreSize builds a VolumeSnapshotContent carrying status.restoreSize (bytes, int64), the
// durable source of SnapshotDataBinding.Size. When deleting is true it is marked for deletion (a
// finalizer is required so the fake client keeps it present with a non-zero deletionTimestamp).
func vscWithRestoreSize(name string, bytes int64, deleting bool) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
	obj.SetName(name)
	if bytes > 0 {
		_ = unstructured.SetNestedField(obj.Object, bytes, "status", "restoreSize")
	}
	if deleting {
		obj.SetFinalizers([]string{"keep/for-test"})
		now := metav1.NewTime(time.Now())
		obj.SetDeletionTimestamp(&now)
	}
	return obj
}

func vscArtifact(name string) storagev1alpha1.SnapshotDataArtifactRef {
	return storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: name}
}

// Size is read from the durable VolumeSnapshotContent.status.restoreSize (10 GiB here), for both a PVC
// target and a domain (non-PVC) data leaf, since the artifact-derived size outlives the source PVC.
func TestEnrich_PopulatesSizeFromVSCRestoreSize(t *testing.T) {
	ctx := context.Background()
	scheme := enrichScheme(t)
	const tenGiB = int64(10) * 1024 * 1024 * 1024
	pvc := sourcePVC("data", corev1.PersistentVolumeClaimSpec{VolumeMode: blockMode(), VolumeName: "pv-data"})
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(pvc, vscWithRestoreSize("vsc-pvc", tenGiB, false), vscWithRestoreSize("vsc-disk", tenGiB, false)).
		Build()

	pvcBinding := pvcTargetBinding("data")
	pvcBinding.ArtifactRef = vscArtifact("vsc-pvc")
	domainBinding := storagev1alpha1.SnapshotDataBinding{
		SourceRef:   storagev1alpha1.SnapshotSubjectRef{UID: "uid-disk", Kind: "DemoVirtualDisk", Namespace: "ns1", Name: "disk"},
		ArtifactRef: vscArtifact("vsc-disk"),
	}

	out, err := EnrichDataBindingsWithVolumeMetadata(ctx, cl, cl, []storagev1alpha1.SnapshotDataBinding{pvcBinding, domainBinding})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if out[0].Size != "10Gi" {
		t.Errorf("PVC binding size: want 10Gi, got %q", out[0].Size)
	}
	if out[1].Size != "10Gi" {
		t.Errorf("domain (non-PVC) binding size: want 10Gi, got %q", out[1].Size)
	}
}

// Enrichment backfills the durable artifact uid from the live VolumeSnapshotContent when an upstream
// producer referenced the artifact by name only (the import path), and MUST NOT override a uid a
// producer already supplied (the VCR / orphan paths).
func TestEnrich_BackfillsArtifactUIDFromVSC(t *testing.T) {
	ctx := context.Background()
	scheme := enrichScheme(t)
	const tenGiB = int64(10) * 1024 * 1024 * 1024
	vscNamed := vscWithRestoreSize("vsc-named", tenGiB, false)
	vscNamed.SetUID("vsc-uid-from-object")
	vscPreset := vscWithRestoreSize("vsc-preset", tenGiB, false)
	vscPreset.SetUID("vsc-uid-from-object")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vscNamed, vscPreset).Build()

	nameOnly := storagev1alpha1.SnapshotDataBinding{
		SourceRef:   storagev1alpha1.SnapshotSubjectRef{UID: "uid-disk", Kind: "DemoVirtualDisk", Namespace: "ns1", Name: "disk"},
		ArtifactRef: vscArtifact("vsc-named"),
	}
	preset := storagev1alpha1.SnapshotDataBinding{
		SourceRef:   storagev1alpha1.SnapshotSubjectRef{UID: "uid-disk2", Kind: "DemoVirtualDisk", Namespace: "ns1", Name: "disk2"},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-preset", UID: "producer-supplied-uid"},
	}

	out, err := EnrichDataBindingsWithVolumeMetadata(ctx, cl, cl, []storagev1alpha1.SnapshotDataBinding{nameOnly, preset})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if out[0].ArtifactRef.UID != "vsc-uid-from-object" {
		t.Errorf("name-only artifact uid: want backfill from VSC object, got %q", out[0].ArtifactRef.UID)
	}
	if out[1].ArtifactRef.UID != "producer-supplied-uid" {
		t.Errorf("producer-supplied artifact uid must not be overridden, got %q", out[1].ArtifactRef.UID)
	}
}

func TestReadArtifactRestoreSize_Branches(t *testing.T) {
	ctx := context.Background()
	scheme := enrichScheme(t)
	const eightGiB = int64(8) * 1024 * 1024 * 1024
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(
			vscWithRestoreSize("vsc-ok", eightGiB, false),
			vscWithRestoreSize("vsc-zero", 0, false),
			vscWithRestoreSize("vsc-deleting", eightGiB, true),
		).Build()

	cases := []struct {
		name     string
		artifact storagev1alpha1.SnapshotDataArtifactRef
		want     string
	}{
		{"valid restoreSize", vscArtifact("vsc-ok"), "8Gi"},
		{"non-VSC artifact", storagev1alpha1.SnapshotDataArtifactRef{Kind: "PersistentVolume", Name: "pv-x"}, ""},
		{"empty name", vscArtifact(""), ""},
		{"not found", vscArtifact("vsc-gone"), ""},
		{"missing/zero restoreSize", vscArtifact("vsc-zero"), ""},
		{"deleting VSC", vscArtifact("vsc-deleting"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readArtifactRestoreSize(ctx, cl, tc.artifact)
			if err != nil {
				t.Fatalf("readArtifactRestoreSize: unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("size = %q, want %q", got, tc.want)
			}
		})
	}
}

// A transient (non-NotFound) read error must propagate so the caller requeues instead of publishing a
// binding with an empty Size.
func TestReadArtifactRestoreSize_TransientErrorPropagates(t *testing.T) {
	ctx := context.Background()
	scheme := enrichScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetObjectKind().GroupVersionKind().Kind == "VolumeSnapshotContent" {
				return fmt.Errorf("etcdserver: request timed out")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()

	if _, err := readArtifactRestoreSize(ctx, cl, vscArtifact("vsc-any")); err == nil {
		t.Fatal("expected a transient VSC read error to be returned, not swallowed")
	}
}

// dataBindingEqual is the publish latch's comparison (Variant A: a content holds a single dataRef, so
// equality is per-binding). A field it forgets is a field the latch cannot see change: the content stays
// "already equal" and freezes a stale value for the rest of its life, which for volumeMode fail-closes export
// forever. So EVERY field of SnapshotDataBinding must take part, and the fields are enumerated by REFLECTION
// rather than by hand — a hand-written list is exactly what lets a newly added field slip through uncompared
// (this test used to carry an accessModes entry, and dropping that field meant editing the list).
//
// Limit of the sweep: it mutates string-kinded leaves, which is every leaf the binding has. A future leaf of
// any other kind (slice, pointer, number) fails the sweep loudly instead of being skipped in silence.
func TestDataBindingEqual_EveryFieldParticipates(t *testing.T) {
	base := storagev1alpha1.SnapshotDataBinding{
		SourceRef:        storagev1alpha1.SnapshotSubjectRef{APIVersion: "v1", UID: "u1", Kind: "PersistentVolumeClaim", Name: "p", Namespace: "n"},
		ArtifactRef:      storagev1alpha1.SnapshotDataArtifactRef{Kind: "VolumeSnapshotContent", Name: "vsc", APIVersion: "snapshot.storage.k8s.io/v1", UID: "a1"},
		VolumeMode:       "Filesystem",
		FsType:           "ext4",
		StorageClassName: "sc",
		Size:             "10Gi",
	}
	if !dataBindingEqual(base, base) {
		t.Fatal("identical bindings must compare equal")
	}

	paths := stringLeafPaths(t, reflect.TypeOf(base), "")
	if len(paths) == 0 {
		t.Fatal("reflection found no leaves to mutate: the sweep would report success without comparing anything")
	}
	for _, path := range paths {
		mutated := base
		leaf := leafByPath(t, reflect.ValueOf(&mutated).Elem(), path)
		leaf.SetString(leaf.String() + "-changed")
		if dataBindingEqual(base, mutated) {
			t.Errorf("bindings differing by %s compare EQUAL: dataBindingEqual does not look at that field, so the latch cannot see it change", path)
		}
	}
	t.Logf("dataBindingEqual swept %d fields of SnapshotDataBinding: %v", len(paths), paths)
}

// stringLeafPaths returns the dotted paths of every string-kinded leaf field reachable in t, descending into
// nested structs. Any other kind is a hard failure: the sweep above would otherwise quietly stop covering it.
func stringLeafPaths(t *testing.T, typ reflect.Type, prefix string) []string {
	t.Helper()
	var paths []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		path := f.Name
		if prefix != "" {
			path = prefix + "." + f.Name
		}
		switch f.Type.Kind() {
		case reflect.String:
			paths = append(paths, path)
		case reflect.Struct:
			paths = append(paths, stringLeafPaths(t, f.Type, path)...)
		default:
			t.Fatalf("%s is a %s: this sweep only mutates string leaves, so extend it (and dataBindingEqual) for the new kind", path, f.Type.Kind())
		}
	}
	return paths
}

// leafByPath resolves a dotted path produced by stringLeafPaths against an addressable value.
func leafByPath(t *testing.T, v reflect.Value, path string) reflect.Value {
	t.Helper()
	for _, name := range strings.Split(path, ".") {
		v = v.FieldByName(name)
		if !v.IsValid() {
			t.Fatalf("field %q of path %q not found", name, path)
		}
	}
	return v
}

// A live PVC that carries the captured source NAME but a different UID is a different volume, and its
// metadata must not be enriched onto the binding. Two producers hand the enricher a name whose live holder
// may be a stranger: an imported leaf's sourceRef is rebuilt from the checkpoint manifest, and a restore can
// recreate a PVC under the captured name. Substituting a stranger's metadata is worse than publishing none —
// an inverted volumeMode restores a Block source as a filesystem and serves garbage, while a wrong
// fsType/StorageClass is silently plausible. The artifact-derived Size must still be filled: it does not come
// from the PVC at all.
func TestEnrich_SkipsLivePVCWithDifferentUID(t *testing.T) {
	ctx := context.Background()
	scheme := enrichScheme(t)
	const tenGiB = int64(10) * 1024 * 1024 * 1024
	// Same namespace/name as the binding's source, different UID: a Block volume on another class, so every
	// field the enricher could copy differs from what the real source was.
	foreign := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "data", UID: types.UID("uid-someone-else")},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeMode:       blockMode(),
			StorageClassName: scPtr("foreign-class"),
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(foreign, vscWithRestoreSize("vsc-data", tenGiB, false)).Build()

	binding := pvcTargetBinding("data")
	binding.ArtifactRef = vscArtifact("vsc-data")
	out, err := EnrichDataBindingsWithVolumeMetadata(ctx, cl, cl, []storagev1alpha1.SnapshotDataBinding{binding})
	if err != nil {
		t.Fatalf("a same-named foreign PVC must be tolerated, got error: %v", err)
	}
	b := out[0]
	if b.VolumeMode != "" || b.FsType != "" || b.StorageClassName != "" {
		t.Fatalf("metadata of a PVC with a different UID must not be published: %#v", b)
	}
	if b.Size != "10Gi" {
		t.Fatalf("the artifact-derived size must still be enriched, got %q", b.Size)
	}
}

// The identity check compares UIDs; it must not turn into "skip whenever a UID is present". A live PVC whose
// UID matches the source ref is the captured volume and is enriched as usual — this is the capture path.
func TestEnrich_EnrichesLivePVCWithMatchingUID(t *testing.T) {
	ctx := context.Background()
	scheme := enrichScheme(t)
	pvc := sourcePVC("data", corev1.PersistentVolumeClaimSpec{VolumeMode: fsMode(), StorageClassName: scPtr("fast")})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()

	out, err := EnrichDataBindingsWithVolumeMetadata(ctx, cl, cl, []storagev1alpha1.SnapshotDataBinding{pvcTargetBinding("data")})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if out[0].VolumeMode != "Filesystem" || out[0].StorageClassName != "fast" {
		t.Fatalf("a UID-matching source PVC must be enriched: %#v", out[0])
	}
}

// A binding whose sourceRef carries no UID cannot be verified, so it keeps the pre-existing behavior (enrich
// by name). Without this, an older producer that publishes no source uid would silently lose all volume
// metadata instead of keeping today's best effort.
func TestEnrich_EnrichesWhenSourceRefHasNoUID(t *testing.T) {
	ctx := context.Background()
	scheme := enrichScheme(t)
	pvc := sourcePVC("data", corev1.PersistentVolumeClaimSpec{VolumeMode: fsMode(), StorageClassName: scPtr("fast")})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()

	binding := pvcTargetBinding("data")
	binding.SourceRef.UID = ""
	out, err := EnrichDataBindingsWithVolumeMetadata(ctx, cl, cl, []storagev1alpha1.SnapshotDataBinding{binding})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if out[0].VolumeMode != "Filesystem" || out[0].StorageClassName != "fast" {
		t.Fatalf("an unverifiable (uid-less) source ref must still be enriched by name: %#v", out[0])
	}
}
