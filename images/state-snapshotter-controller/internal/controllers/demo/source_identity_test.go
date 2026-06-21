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
	"testing"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
)

func TestResolveDemoSnapshotSource_Valid(t *testing.T) {
	spec := demov1alpha1.SnapshotSourceRef{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDisk,
		Name:       "disk-a",
	}
	res := resolveDemoSnapshotSource(controllercommon.KindDemoVirtualDisk, spec)
	if res.Reason != "" {
		t.Fatalf("unexpected failure: %s/%s", res.Reason, res.Message)
	}
	if res.Name != "disk-a" {
		t.Fatalf("unexpected source name: %q", res.Name)
	}
}

func TestResolveDemoSnapshotSource_Invalid(t *testing.T) {
	cases := map[string]demov1alpha1.SnapshotSourceRef{
		"empty": {},
		"wrong apiVersion": {
			APIVersion: "other.example.com/v1",
			Kind:       controllercommon.KindDemoVirtualDisk,
			Name:       "disk-a",
		},
		"wrong kind": {
			APIVersion: demov1alpha1.SchemeGroupVersion.String(),
			Kind:       controllercommon.KindDemoVirtualMachine,
			Name:       "disk-a",
		},
		"missing name": {
			APIVersion: demov1alpha1.SchemeGroupVersion.String(),
			Kind:       controllercommon.KindDemoVirtualDisk,
		},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			res := resolveDemoSnapshotSource(controllercommon.KindDemoVirtualDisk, spec)
			if res.Reason != demoReasonInvalidSourceRef {
				t.Fatalf("expected InvalidSourceRef, got %q (%s)", res.Reason, res.Message)
			}
		})
	}
}
