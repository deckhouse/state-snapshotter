package domain_rbac

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// stubResolver returns a GVR per apiVersion/kind from the table, or an error if the
// ref is listed in failKinds. It lets us exercise resolveEligibleGVRs without a live
// RESTMapper.
func stubResolver(table map[string]schema.GroupVersionResource, failKinds map[string]struct{}) gvrResolver {
	return func(ref v1alpha1.SnapshotGVKRef) (schema.GroupVersionResource, error) {
		key := ref.APIVersion + "/" + ref.Kind
		if _, fail := failKinds[key]; fail {
			return schema.GroupVersionResource{}, fmt.Errorf("no mapping for %s", key)
		}
		gvr, ok := table[key]
		if !ok {
			return schema.GroupVersionResource{}, fmt.Errorf("unexpected ref %s", key)
		}
		return gvr, nil
	}
}

func csd(name string, entries ...v1alpha1.SnapshotResourceMappingEntry) v1alpha1.CustomSnapshotDefinition {
	c := v1alpha1.CustomSnapshotDefinition{}
	c.Name = name
	c.Spec.SnapshotResourceMapping = entries
	return c
}

func entry(srcAPI, srcKind, snapAPI, snapKind string) v1alpha1.SnapshotResourceMappingEntry {
	return v1alpha1.SnapshotResourceMappingEntry{
		Source:   v1alpha1.SnapshotGVKRef{APIVersion: srcAPI, Kind: srcKind},
		Snapshot: v1alpha1.SnapshotGVKRef{APIVersion: snapAPI, Kind: snapKind},
	}
}

func TestResolveEligibleGVRs(t *testing.T) {
	const (
		demoAPI = "demo.state-snapshotter.deckhouse.io/v1alpha1"
	)
	diskGVR := schema.GroupVersionResource{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualdisks"}
	diskSnapGVR := schema.GroupVersionResource{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualdisksnapshots"}
	vmGVR := schema.GroupVersionResource{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualmachines"}
	vmSnapGVR := schema.GroupVersionResource{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualmachinesnapshots"}

	table := map[string]schema.GroupVersionResource{
		demoAPI + "/DemoVirtualDisk":            diskGVR,
		demoAPI + "/DemoVirtualDiskSnapshot":    diskSnapGVR,
		demoAPI + "/DemoVirtualMachine":         vmGVR,
		demoAPI + "/DemoVirtualMachineSnapshot": vmSnapGVR,
	}

	t.Run("all resolve: ordered, no pending", func(t *testing.T) {
		eligible := []v1alpha1.CustomSnapshotDefinition{
			csd("demo",
				entry(demoAPI, "DemoVirtualMachine", demoAPI, "DemoVirtualMachineSnapshot"),
				entry(demoAPI, "DemoVirtualDisk", demoAPI, "DemoVirtualDiskSnapshot"),
			),
		}
		src, snap, pending := resolveEligibleGVRs(eligible, stubResolver(table, nil))

		wantSrc := []schema.GroupVersionResource{vmGVR, diskGVR}
		wantSnap := []schema.GroupVersionResource{vmSnapGVR, diskSnapGVR}
		if !reflect.DeepEqual(src, wantSrc) {
			t.Errorf("source GVRs = %v, want %v", src, wantSrc)
		}
		if !reflect.DeepEqual(snap, wantSnap) {
			t.Errorf("snapshot GVRs = %v, want %v", snap, wantSnap)
		}
		if len(pending) != 0 {
			t.Errorf("pending = %v, want empty", pending)
		}
	})

	t.Run("resolve error never yields zero-value GVR", func(t *testing.T) {
		// Source fails to resolve; the other entry resolves fully. The failed source must NOT
		// leak a zero-value GVR into the output (which would become an empty ClusterRole rule),
		// but its CSD must be recorded as pending.
		fail := map[string]struct{}{demoAPI + "/DemoVirtualDisk": {}}
		eligible := []v1alpha1.CustomSnapshotDefinition{
			csd("partial",
				entry(demoAPI, "DemoVirtualDisk", demoAPI, "DemoVirtualDiskSnapshot"),
				entry(demoAPI, "DemoVirtualMachine", demoAPI, "DemoVirtualMachineSnapshot"),
			),
		}
		src, snap, pending := resolveEligibleGVRs(eligible, stubResolver(table, fail))

		for _, g := range src {
			if g == (schema.GroupVersionResource{}) {
				t.Fatalf("source GVRs contain a zero-value GVR: %v", src)
			}
		}
		for _, g := range snap {
			if g == (schema.GroupVersionResource{}) {
				t.Fatalf("snapshot GVRs contain a zero-value GVR: %v", snap)
			}
		}
		// The failed source is dropped; the successful VM source and both snapshots survive.
		wantSrc := []schema.GroupVersionResource{vmGVR}
		wantSnap := []schema.GroupVersionResource{diskSnapGVR, vmSnapGVR}
		if !reflect.DeepEqual(src, wantSrc) {
			t.Errorf("source GVRs = %v, want %v", src, wantSrc)
		}
		if !reflect.DeepEqual(snap, wantSnap) {
			t.Errorf("snapshot GVRs = %v, want %v", snap, wantSnap)
		}
		if _, ok := pending["partial"]; !ok {
			t.Errorf("CSD %q must be pending after a resolve error, pending = %v", "partial", pending)
		}
	})

	t.Run("dedup across CSDs", func(t *testing.T) {
		eligible := []v1alpha1.CustomSnapshotDefinition{
			csd("a", entry(demoAPI, "DemoVirtualDisk", demoAPI, "DemoVirtualDiskSnapshot")),
			csd("b", entry(demoAPI, "DemoVirtualDisk", demoAPI, "DemoVirtualDiskSnapshot")),
		}
		src, snap, pending := resolveEligibleGVRs(eligible, stubResolver(table, nil))

		if want := []schema.GroupVersionResource{diskGVR}; !reflect.DeepEqual(src, want) {
			t.Errorf("source GVRs = %v, want %v (deduped)", src, want)
		}
		if want := []schema.GroupVersionResource{diskSnapGVR}; !reflect.DeepEqual(snap, want) {
			t.Errorf("snapshot GVRs = %v, want %v (deduped)", snap, want)
		}
		if len(pending) != 0 {
			t.Errorf("pending = %v, want empty", pending)
		}
	})
}
