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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

func pvcTargetBinding(ns, name string) storagev1alpha1.SnapshotDataBinding {
	return storagev1alpha1.SnapshotDataBinding{
		TargetUID: "uid-" + name,
		Target:    storagev1alpha1.SnapshotSubjectRef{Kind: "PersistentVolumeClaim", Namespace: ns, Name: name},
	}
}

func TestEnrich_FilesystemPVCWithCSIPV(t *testing.T) {
	ctx := context.Background()
	scheme := enrichScheme(t)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "data"},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeMode:       fsMode(),
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadOnlyMany},
			StorageClassName: scPtr("fast"),
			VolumeName:       "pv-data",
		},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-data"},
		Spec:       corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "d", VolumeHandle: "h", FSType: "xfs"}}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc, pv).Build()

	out, err := EnrichDataBindingsWithVolumeMetadata(ctx, cl, cl, []storagev1alpha1.SnapshotDataBinding{pvcTargetBinding("ns1", "data")})
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
	if len(b.AccessModes) != 2 || b.AccessModes[0] != "ReadWriteOnce" || b.AccessModes[1] != "ReadOnlyMany" {
		t.Errorf("accessModes: got %v", b.AccessModes)
	}
}

func TestEnrich_BlockPVCSkipsPV(t *testing.T) {
	ctx := context.Background()
	scheme := enrichScheme(t)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "blk"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeMode: blockMode(), VolumeName: "pv-blk"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()
	// The PV is intentionally absent: a Block volume must not read it. If it did, the missing PV would
	// surface as an error, so a nil error proves the PV read was skipped.
	out, err := EnrichDataBindingsWithVolumeMetadata(ctx, cl, cl, []storagev1alpha1.SnapshotDataBinding{pvcTargetBinding("ns1", "blk")})
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
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "nm"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()
	out, err := EnrichDataBindingsWithVolumeMetadata(ctx, cl, cl, []storagev1alpha1.SnapshotDataBinding{pvcTargetBinding("ns1", "nm")})
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
	in := []storagev1alpha1.SnapshotDataBinding{pvcTargetBinding("ns1", "gone")}
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
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "data"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeMode: fsMode(), VolumeName: "pv-data"},
	}
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

	_, err := EnrichDataBindingsWithVolumeMetadata(ctx, cl, directFail, []storagev1alpha1.SnapshotDataBinding{pvcTargetBinding("ns1", "data")})
	if err == nil {
		t.Fatal("expected a PV read error to be returned, not swallowed")
	}
}

func TestEnrich_NonPVCTargetSkipped(t *testing.T) {
	ctx := context.Background()
	scheme := enrichScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	binding := storagev1alpha1.SnapshotDataBinding{
		TargetUID: "uid-x",
		Target:    storagev1alpha1.SnapshotSubjectRef{Kind: "DemoVirtualDisk", Namespace: "ns1", Name: "disk"},
	}
	out, err := EnrichDataBindingsWithVolumeMetadata(ctx, cl, cl, []storagev1alpha1.SnapshotDataBinding{binding})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if out[0].VolumeMode != "" {
		t.Errorf("non-PVC target must be skipped, got volumeMode %q", out[0].VolumeMode)
	}
}

func TestSnapshotDataRefsEqual_VolumeMetadata(t *testing.T) {
	base := storagev1alpha1.SnapshotDataBinding{
		TargetUID:        "u1",
		Target:           storagev1alpha1.SnapshotSubjectRef{Kind: "PersistentVolumeClaim", Name: "p", Namespace: "n"},
		Artifact:         storagev1alpha1.SnapshotDataArtifactRef{Kind: "VolumeSnapshotContent", Name: "vsc", APIVersion: "snapshot.storage.k8s.io/v1"},
		VolumeMode:       "Filesystem",
		FsType:           "ext4",
		StorageClassName: "sc",
		AccessModes:      []string{"ReadWriteOnce"},
	}
	mut := func(f func(b *storagev1alpha1.SnapshotDataBinding)) []storagev1alpha1.SnapshotDataBinding {
		c := base
		c.AccessModes = append([]string(nil), base.AccessModes...)
		f(&c)
		return []storagev1alpha1.SnapshotDataBinding{c}
	}
	a := []storagev1alpha1.SnapshotDataBinding{base}

	if !snapshotDataRefsEqual(a, mut(func(b *storagev1alpha1.SnapshotDataBinding) {})) {
		t.Error("identical bindings must compare equal")
	}
	for name, f := range map[string]func(b *storagev1alpha1.SnapshotDataBinding){
		"volumeMode":       func(b *storagev1alpha1.SnapshotDataBinding) { b.VolumeMode = "Block" },
		"fsType":           func(b *storagev1alpha1.SnapshotDataBinding) { b.FsType = "xfs" },
		"storageClassName": func(b *storagev1alpha1.SnapshotDataBinding) { b.StorageClassName = "other" },
		"accessModes":      func(b *storagev1alpha1.SnapshotDataBinding) { b.AccessModes = []string{"ReadWriteMany"} },
	} {
		if snapshotDataRefsEqual(a, mut(f)) {
			t.Errorf("bindings differing by %s must compare unequal", name)
		}
	}
}
