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
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/domain-controller/internal/controllers/common"
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
	meta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{Type: storagev1alpha1.ConditionReady, Status: status, Reason: "Test"})
	return d
}

// diskSnapReady builds a Ready DemoVirtualDiskSnapshot (a leaf node) with the given name.
func diskSnapReady(name string) *demov1alpha1.DemoVirtualDiskSnapshot {
	d := &demov1alpha1.DemoVirtualDiskSnapshot{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1"}}
	meta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{Type: storagev1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Test"})
	return d
}

// vmSnapReady builds a Ready DemoVirtualMachineSnapshot whose status.childrenSnapshotRefs point at the
// given disk snapshot names, modeling a VM run-tree node for the per-CR recursion.
func vmSnapReady(name string, childDiskSnaps ...string) *demov1alpha1.DemoVirtualMachineSnapshot {
	vm := &demov1alpha1.DemoVirtualMachineSnapshot{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1"}}
	for _, c := range childDiskSnaps {
		vm.Status.ChildrenSnapshotRefs = append(vm.Status.ChildrenSnapshotRefs, storagev1alpha1.SnapshotChildRef{
			APIVersion: demov1alpha1.SchemeGroupVersion.String(),
			Kind:       controllercommon.KindDemoVirtualDiskSnapshot,
			Name:       c,
		})
	}
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{Type: storagev1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Test"})
	return vm
}

func configMapObj(name string) unstructured.Unstructured {
	o := unstructured.Unstructured{Object: map[string]interface{}{}}
	o.SetAPIVersion("v1")
	o.SetKind("ConfigMap")
	o.SetName(name)
	return o
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

	out, err := svc.applyTransform(base, "ns1", "", "dsnap-a")
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

func TestApplyTransform_UnownedDiskWithPVCFailsClosed(t *testing.T) {
	svc := NewRestoreService(nil, nil, nil)
	// disk-x has a PVC data leg (pvc-x, dropped as covered) but no owning disk snapshot (empty
	// ownerSnapshotName). Emitting it would silently restore an empty volume, so the transform must fail
	// closed. This is unreachable in the per-CR model (a disk node always passes its own name as owner)
	// but the guard is kept defensively.
	base := []unstructured.Unstructured{demoDiskObj("disk-x", "pvc-x"), pvcObj("pvc-x")}
	if _, err := svc.applyTransform(base, "ns1", "", ""); err == nil {
		t.Fatalf("expected fail-closed error for an unowned disk with a PVC data leg")
	}
}

func TestApplyTransform_UnownedDiskWithoutPVCEmitted(t *testing.T) {
	svc := NewRestoreService(nil, nil, nil)
	// A disk with no PVC has no data leg, so it is safe to emit untouched even without an owner.
	base := []unstructured.Unstructured{demoDiskObj("disk-x", "")}
	out, err := svc.applyTransform(base, "ns1", "", "")
	if err != nil {
		t.Fatalf("unexpected error for a data-less disk: %v", err)
	}
	if _, ok := findByName(out, controllercommon.KindDemoVirtualDisk, "disk-x"); !ok {
		t.Fatalf("expected data-less disk-x to be emitted")
	}
}

// stubFetcher returns the same objs for every node; per-CR tests that need different bases per node use
// keyedFetcher instead.
type stubFetcher struct {
	objs []unstructured.Unstructured
	err  error
}

func (s stubFetcher) NodeBaseManifests(_ context.Context, _, _, _ string) ([]unstructured.Unstructured, error) {
	return s.objs, s.err
}

// keyedFetcher returns per-node base manifests keyed by "<resource>/<name>", for the per-CR recursion
// tests where each node has its own base.
type keyedFetcher struct {
	byKey map[string][]unstructured.Unstructured
}

func (k keyedFetcher) NodeBaseManifests(_ context.Context, resource, _, name string) ([]unstructured.Unstructured, error) {
	objs, ok := k.byKey[resource+"/"+name]
	if !ok {
		return nil, fmt.Errorf("keyedFetcher: no base for %s/%s", resource, name)
	}
	return objs, nil
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

func TestManifestsWithDataRestoration_VMSubtree_RecursesChildrenPerCR(t *testing.T) {
	// VM node owns a generic object (vm-cm) and two disk-snapshot children, each carrying its OWN base
	// (disk + covered PVC). Per-CR recursion must fetch each node's own base separately, point each disk
	// at its OWN owning disk snapshot, drop covered PVCs, and emit children before the parent (post-order).
	reader := fake.NewClientBuilder().WithScheme(domainScheme(t)).WithObjects(
		vmSnapReady("vm-snap", "dsnap-a", "dsnap-b"),
		diskSnapReady("dsnap-a"),
		diskSnapReady("dsnap-b"),
	).Build()
	fetcher := keyedFetcher{byKey: map[string][]unstructured.Unstructured{
		ResourceDemoVirtualMachineSnapshot + "/vm-snap": {configMapObj("vm-cm")},
		ResourceDemoVirtualDiskSnapshot + "/dsnap-a":    {demoDiskObj("disk-a", "pvc-a"), pvcObj("pvc-a")},
		ResourceDemoVirtualDiskSnapshot + "/dsnap-b":    {demoDiskObj("disk-b", "pvc-b"), pvcObj("pvc-b")},
	}}
	svc := NewRestoreService(reader, fetcher, nil)

	data, err := svc.ManifestsWithDataRestoration(context.Background(), ResourceDemoVirtualMachineSnapshot, "ns1", "vm-snap", "")
	if err != nil {
		t.Fatalf("ManifestsWithDataRestoration: %v", err)
	}
	out, err := decodeManifestArray(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if _, ok := findByName(out, "PersistentVolumeClaim", "pvc-a"); ok {
		t.Fatalf("expected covered PVC pvc-a dropped")
	}
	if _, ok := findByName(out, "PersistentVolumeClaim", "pvc-b"); ok {
		t.Fatalf("expected covered PVC pvc-b dropped")
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
	if _, ok := findByName(out, "ConfigMap", "vm-cm"); !ok {
		t.Fatalf("expected the VM node's own object vm-cm in output")
	}
	// Post-order: the parent VM's own object is emitted after its disk children.
	if got := out[len(out)-1]; got.GetKind() != "ConfigMap" || got.GetName() != "vm-cm" {
		t.Fatalf("expected post-order with vm-cm last, got %s/%s", got.GetKind(), got.GetName())
	}
}

func TestManifestsWithDataRestoration_ChildNotReadyFailsClosed(t *testing.T) {
	// The VM is Ready but a disk child is not: the per-CR recursion must fail closed on the child rather
	// than compile a partial subtree.
	reader := fake.NewClientBuilder().WithScheme(domainScheme(t)).WithObjects(
		vmSnapReady("vm-snap", "dsnap-a"),
		diskSnapWithReady(false),
	).Build()
	fetcher := keyedFetcher{byKey: map[string][]unstructured.Unstructured{
		ResourceDemoVirtualMachineSnapshot + "/vm-snap": {configMapObj("vm-cm")},
		ResourceDemoVirtualDiskSnapshot + "/dsnap-a":    {demoDiskObj("disk-a", "pvc-a"), pvcObj("pvc-a")},
	}}
	svc := NewRestoreService(reader, fetcher, nil)
	if _, err := svc.ManifestsWithDataRestoration(context.Background(), ResourceDemoVirtualMachineSnapshot, "ns1", "vm-snap", ""); err == nil {
		t.Fatalf("expected fail-closed error when a child disk snapshot is not Ready")
	}
}

func TestMarshalObjects_EmptyAndFailClosedOnDuplicate(t *testing.T) {
	empty, err := marshalObjects(nil)
	if err != nil {
		t.Fatalf("marshalObjects(nil): %v", err)
	}
	if string(empty) != "[]" {
		t.Fatalf("expected [] for empty, got %s", empty)
	}

	// Two objects with the same identity must fail closed (mirroring the core compiler), not silently
	// collapse to one — restore never silently picks one of two same-identity objects.
	dup := []unstructured.Unstructured{demoDiskObj("disk-a", ""), demoDiskObj("disk-a", "")}
	if _, err := marshalObjects(dup); err == nil {
		t.Fatalf("expected fail-closed error on duplicate object identity")
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
