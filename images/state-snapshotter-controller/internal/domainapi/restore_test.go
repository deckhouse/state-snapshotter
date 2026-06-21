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

package domainapi

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// diskSnapWithReady builds the test DemoVirtualDiskSnapshot CR (ns1/dsnap-a) carrying a Ready condition
// of the given status, used to satisfy (or deliberately fail) the readiness gate in
// ManifestsWithDataRestoration.
func diskSnapWithReady(ready bool) *demov1alpha1.DemoVirtualDiskSnapshot {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	d := &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "dsnap-a", Namespace: "ns1"},
	}
	meta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: status, Reason: "Test"})
	return d
}

func domainScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	if err := demov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add demo scheme: %v", err)
	}
	return scheme
}

func demoDiskObj(name, pvc string) unstructured.Unstructured {
	o := unstructured.Unstructured{Object: map[string]interface{}{}}
	o.SetAPIVersion(demov1alpha1.SchemeGroupVersion.String())
	o.SetKind(controllercommon.KindDemoVirtualDisk)
	o.SetName(name)
	if pvc != "" {
		_ = unstructured.SetNestedField(o.Object, pvc, "spec", "persistentVolumeClaimName")
	}
	return o
}

func pvcObj(name string) unstructured.Unstructured {
	o := unstructured.Unstructured{Object: map[string]interface{}{}}
	o.SetAPIVersion("v1")
	o.SetKind("PersistentVolumeClaim")
	o.SetName(name)
	return o
}

func dataSourceName(t *testing.T, obj unstructured.Unstructured) (string, bool) {
	t.Helper()
	name, found, err := unstructured.NestedString(obj.Object, "spec", "dataSource", "name")
	if err != nil {
		t.Fatalf("read spec.dataSource.name: %v", err)
	}
	return name, found
}

func findByName(objs []unstructured.Unstructured, kind, name string) (unstructured.Unstructured, bool) {
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o, true
		}
	}
	return unstructured.Unstructured{}, false
}

func TestApplyTransform_DiskLeaf_CoversPVCAndSetsDataSource(t *testing.T) {
	svc := NewRestoreService(nil, nil, nil)
	base := []unstructured.Unstructured{demoDiskObj("disk-a", "pvc-a"), pvcObj("pvc-a")}

	out, err := svc.applyTransform(base, "ns1", "", diskOwnerResolver{defaultOwner: "dsnap-a"})
	if err != nil {
		t.Fatalf("applyTransform: %v", err)
	}
	if _, ok := findByName(out, "PersistentVolumeClaim", "pvc-a"); ok {
		t.Fatalf("expected covered PVC pvc-a to be dropped, got it in output")
	}
	disk, ok := findByName(out, controllercommon.KindDemoVirtualDisk, "disk-a")
	if !ok {
		t.Fatalf("expected DemoVirtualDisk disk-a in output")
	}
	name, found := dataSourceName(t, disk)
	if !found || name != "dsnap-a" {
		t.Fatalf("expected disk-a spec.dataSource.name=dsnap-a, got %q (found=%v)", name, found)
	}
}

func TestApplyTransform_VMCorrelatesDisksToOwningSnapshots(t *testing.T) {
	svc := NewRestoreService(nil, nil, nil)
	base := []unstructured.Unstructured{
		demoDiskObj("disk-a", "pvc-a"),
		demoDiskObj("disk-b", "pvc-b"),
		pvcObj("pvc-a"),
		pvcObj("pvc-b"),
	}
	owners := diskOwnerResolver{byDiskName: map[string]string{"disk-a": "dsnap-a", "disk-b": "dsnap-b"}}

	out, err := svc.applyTransform(base, "ns1", "target-ns", owners)
	if err != nil {
		t.Fatalf("applyTransform: %v", err)
	}
	for _, o := range out {
		if o.GetNamespace() != "target-ns" {
			t.Fatalf("expected sanitized objects rewritten to target-ns, got %q for %s", o.GetNamespace(), o.GetName())
		}
	}
	for _, o := range out {
		if o.GetKind() == "PersistentVolumeClaim" {
			t.Fatalf("expected covered PVCs dropped, found %s", o.GetName())
		}
	}
	for disk, want := range map[string]string{"disk-a": "dsnap-a", "disk-b": "dsnap-b"} {
		obj, ok := findByName(out, controllercommon.KindDemoVirtualDisk, disk)
		if !ok {
			t.Fatalf("expected %s in output", disk)
		}
		name, found := dataSourceName(t, obj)
		if !found || name != want {
			t.Fatalf("expected %s spec.dataSource.name=%s, got %q (found=%v)", disk, want, name, found)
		}
	}
}

func TestApplyTransform_UnownedDiskWithPVCFailsClosed(t *testing.T) {
	svc := NewRestoreService(nil, nil, nil)
	// disk-x has a PVC data leg (pvc-x, dropped as covered) but no resolvable owning disk snapshot
	// (empty owners map → ownerFor returns the empty default). Emitting it would silently restore an
	// empty volume, so the transform must fail closed.
	base := []unstructured.Unstructured{demoDiskObj("disk-x", "pvc-x"), pvcObj("pvc-x")}
	if _, err := svc.applyTransform(base, "ns1", "", diskOwnerResolver{byDiskName: map[string]string{}}); err == nil {
		t.Fatalf("expected fail-closed error for an unowned disk with a PVC data leg")
	}
}

func TestApplyTransform_UnownedDiskWithoutPVCEmitted(t *testing.T) {
	svc := NewRestoreService(nil, nil, nil)
	// A disk with no PVC has no data leg, so it is safe to emit untouched even without an owner.
	base := []unstructured.Unstructured{demoDiskObj("disk-x", "")}
	out, err := svc.applyTransform(base, "ns1", "", diskOwnerResolver{byDiskName: map[string]string{}})
	if err != nil {
		t.Fatalf("unexpected error for a data-less disk: %v", err)
	}
	if _, ok := findByName(out, controllercommon.KindDemoVirtualDisk, "disk-x"); !ok {
		t.Fatalf("expected data-less disk-x to be emitted")
	}
}

func TestResolveVMDiskOwners_AnnotationAndSpecRef(t *testing.T) {
	scheme := domainScheme(t)

	identity := controllercommon.SnapshotSourceIdentity{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDisk,
		Namespace:  "ns1",
		Name:       "disk-a",
		UID:        "disk-a-uid",
	}
	identityJSON, err := json.Marshal(identity)
	if err != nil {
		t.Fatalf("marshal identity: %v", err)
	}

	vm := &demov1alpha1.DemoVirtualMachineSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-snap", Namespace: "ns1"},
		Status: demov1alpha1.DemoVirtualMachineSnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{
				{APIVersion: demov1alpha1.SchemeGroupVersion.String(), Kind: controllercommon.KindDemoVirtualDiskSnapshot, Name: "dsnap-a"},
				{APIVersion: demov1alpha1.SchemeGroupVersion.String(), Kind: controllercommon.KindDemoVirtualDiskSnapshot, Name: "dsnap-b"},
			},
		},
	}
	// dsnap-a carries the generic source-identity annotation (root-planned path).
	dsnapA := &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "dsnap-a",
			Namespace:   "ns1",
			Annotations: map[string]string{controllercommon.AnnotationKeySourceRef: string(identityJSON)},
		},
	}
	// dsnap-b carries only spec.sourceRef (manual path).
	dsnapB := &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "dsnap-b", Namespace: "ns1"},
		Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
			SourceRef: demov1alpha1.SnapshotSourceRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       controllercommon.KindDemoVirtualDisk,
				Name:       "disk-b",
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vm, dsnapA, dsnapB).Build()
	svc := NewRestoreService(c, nil, nil)

	owners, err := svc.resolveVMDiskOwners(context.Background(), "ns1", "vm-snap")
	if err != nil {
		t.Fatalf("resolveVMDiskOwners: %v", err)
	}
	if got := owners.ownerFor("disk-a"); got != "dsnap-a" {
		t.Fatalf("expected disk-a owner dsnap-a, got %q", got)
	}
	if got := owners.ownerFor("disk-b"); got != "dsnap-b" {
		t.Fatalf("expected disk-b owner dsnap-b, got %q", got)
	}
}

type stubFetcher struct {
	objs []unstructured.Unstructured
	err  error
}

func (s stubFetcher) BaseManifests(_ context.Context, _, _, _ string) ([]unstructured.Unstructured, error) {
	return s.objs, s.err
}

func TestManifestsWithDataRestoration_DiskLeaf_SanitizesAndSetsDataSource(t *testing.T) {
	disk := demoDiskObj("disk-a", "pvc-a")
	disk.SetUID("u1")
	disk.SetResourceVersion("123")
	_ = unstructured.SetNestedField(disk.Object, "Ready", "status", "phase")
	base := []unstructured.Unstructured{disk, pvcObj("pvc-a")}

	reader := fake.NewClientBuilder().WithScheme(domainScheme(t)).WithObjects(diskSnapWithReady(true)).Build()
	svc := NewRestoreService(reader, stubFetcher{objs: base}, nil)
	data, err := svc.ManifestsWithDataRestoration(context.Background(), ResourceDemoVirtualDiskSnapshot, "ns1", "dsnap-a", "")
	if err != nil {
		t.Fatalf("ManifestsWithDataRestoration: %v", err)
	}

	out, err := decodeManifestArray(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 object (covered PVC dropped), got %d", len(out))
	}
	disk0 := out[0]
	if disk0.GetKind() != controllercommon.KindDemoVirtualDisk || disk0.GetName() != "disk-a" {
		t.Fatalf("expected DemoVirtualDisk disk-a, got %s/%s", disk0.GetKind(), disk0.GetName())
	}
	if disk0.GetNamespace() != "ns1" {
		t.Fatalf("expected effective namespace ns1, got %q", disk0.GetNamespace())
	}
	if disk0.GetUID() != "" || disk0.GetResourceVersion() != "" {
		t.Fatalf("expected server fields stripped, got uid=%q rv=%q", disk0.GetUID(), disk0.GetResourceVersion())
	}
	if _, found, _ := unstructured.NestedMap(disk0.Object, "status"); found {
		t.Fatalf("expected status stripped")
	}
	name, found := dataSourceName(t, disk0)
	if !found || name != "dsnap-a" {
		t.Fatalf("expected spec.dataSource.name=dsnap-a, got %q (found=%v)", name, found)
	}
}

func TestManifestsWithDataRestoration_FetchError(t *testing.T) {
	reader := fake.NewClientBuilder().WithScheme(domainScheme(t)).WithObjects(diskSnapWithReady(true)).Build()
	svc := NewRestoreService(reader, stubFetcher{err: context.DeadlineExceeded}, nil)
	if _, err := svc.ManifestsWithDataRestoration(context.Background(), ResourceDemoVirtualDiskSnapshot, "ns1", "dsnap-a", ""); err == nil {
		t.Fatalf("expected error from fetch failure")
	}
}

func TestManifestsWithDataRestoration_NotReadyFailsClosed(t *testing.T) {
	reader := fake.NewClientBuilder().WithScheme(domainScheme(t)).WithObjects(diskSnapWithReady(false)).Build()
	// A fetcher that would succeed if reached; the readiness gate must short-circuit before it.
	svc := NewRestoreService(reader, stubFetcher{objs: []unstructured.Unstructured{demoDiskObj("disk-a", "pvc-a")}}, nil)
	if _, err := svc.ManifestsWithDataRestoration(context.Background(), ResourceDemoVirtualDiskSnapshot, "ns1", "dsnap-a", ""); err == nil {
		t.Fatalf("expected fail-closed error when the domain snapshot is not Ready")
	}
}

func TestManifestsWithDataRestoration_MissingSnapshotFailsClosed(t *testing.T) {
	reader := fake.NewClientBuilder().WithScheme(domainScheme(t)).Build()
	svc := NewRestoreService(reader, stubFetcher{objs: []unstructured.Unstructured{demoDiskObj("disk-a", "pvc-a")}}, nil)
	if _, err := svc.ManifestsWithDataRestoration(context.Background(), ResourceDemoVirtualDiskSnapshot, "ns1", "missing", ""); err == nil {
		t.Fatalf("expected fail-closed error when the domain snapshot is missing")
	}
}

func TestMarshalObjects_DedupAndEmpty(t *testing.T) {
	empty, err := marshalObjects(nil)
	if err != nil {
		t.Fatalf("marshalObjects(nil): %v", err)
	}
	if string(empty) != "[]" {
		t.Fatalf("expected [] for empty, got %s", empty)
	}

	dup := []unstructured.Unstructured{demoDiskObj("disk-a", ""), demoDiskObj("disk-a", "")}
	data, err := marshalObjects(dup)
	if err != nil {
		t.Fatalf("marshalObjects(dup): %v", err)
	}
	var list []map[string]interface{}
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected dedup to 1 object, got %d", len(list))
	}
}

func TestDecodeManifestArray(t *testing.T) {
	in := []unstructured.Unstructured{demoDiskObj("disk-a", "pvc-a"), pvcObj("pvc-a")}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := decodeManifestArray(raw)
	if err != nil {
		t.Fatalf("decodeManifestArray: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 objects, got %d", len(out))
	}
	if _, ok := findByName(out, controllercommon.KindDemoVirtualDisk, "disk-a"); !ok {
		t.Fatalf("expected disk-a in decoded output")
	}

	if empty, err := decodeManifestArray(nil); err != nil || empty != nil {
		t.Fatalf("expected nil,nil for empty input, got %v,%v", empty, err)
	}
}
