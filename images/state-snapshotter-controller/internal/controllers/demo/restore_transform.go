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

package demo

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/domainsdk"
)

// RestoreTransformer is the demo domain implementation of domainsdk.Transformer
// (ADR 2026-06-10). It keeps demo-specific restore knowledge next to the demo types, so the generic
// restore compiler in internal/usecase/restore stays domain-free.
//
// Behaviour:
//   - A DemoVirtualDisk that owns a PVC data leg (spec.persistentVolumeClaimName) covers that PVC:
//     the restored disk recreates it, so the compiler must not emit a standalone PVC for it.
//   - A DemoVirtualDisk captured under a DemoVirtualDiskSnapshot node is rewritten to restore from
//     that snapshot via spec.dataSource (mirroring PVC.spec.dataSourceRef -> VolumeSnapshot).
//   - DemoVirtualMachine and everything else is left untouched (the generic compiler sanitizes it).
type RestoreTransformer struct{}

// NewRestoreTransformer returns the demo domain restore transformer.
func NewRestoreTransformer() *RestoreTransformer { return &RestoreTransformer{} }

var _ domainsdk.Transformer = (*RestoreTransformer)(nil)

func (RestoreTransformer) CoveredPVCNames(_ *domainsdk.RestoreNode, objects []unstructured.Unstructured) map[string]struct{} {
	covered := map[string]struct{}{}
	for i := range objects {
		if !isDemoVirtualDisk(objects[i]) {
			continue
		}
		pvcName, _, _ := unstructured.NestedString(objects[i].Object, "spec", "persistentVolumeClaimName")
		if pvcName != "" {
			covered[pvcName] = struct{}{}
		}
	}
	return covered
}

func (RestoreTransformer) TransformObject(node *domainsdk.RestoreNode, obj *unstructured.Unstructured, _ []domainsdk.NodeResult) (bool, error) {
	if !isDemoVirtualDisk(*obj) {
		return false, nil
	}
	// Only a disk captured under its own DemoVirtualDiskSnapshot node has a restore source to point at.
	if node == nil || node.SnapshotRef.Kind != controllercommon.KindDemoVirtualDiskSnapshot {
		return false, nil
	}
	dataSource := map[string]interface{}{
		"apiGroup": demov1alpha1.APIGroup,
		"kind":     controllercommon.KindDemoVirtualDiskSnapshot,
		"name":     node.SnapshotRef.Name,
	}
	if err := unstructured.SetNestedMap(obj.Object, dataSource, "spec", "dataSource"); err != nil {
		return false, fmt.Errorf("set DemoVirtualDisk %s spec.dataSource: %w", obj.GetName(), err)
	}
	return true, nil
}

func isDemoVirtualDisk(obj unstructured.Unstructured) bool {
	return obj.GetKind() == controllercommon.KindDemoVirtualDisk &&
		obj.GetAPIVersion() == demov1alpha1.SchemeGroupVersion.String()
}
