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

package v1alpha1

import "testing"

// The policy a Snapshot's tree is created with. Retain is what every capture tree got before the field
// existed, so an omitted field must keep meaning exactly that; an import is Delete whatever the spec says
// (admission refuses the field on Import, the helper does not depend on that).
func TestSnapshotContentDeletionPolicy(t *testing.T) {
	tests := []struct {
		name string
		spec SnapshotSpec
		want string
	}{
		{name: "capture, field omitted", spec: SnapshotSpec{Mode: SnapshotModeCapture}, want: SnapshotContentDeletionPolicyRetain},
		{name: "mode omitted, field omitted", spec: SnapshotSpec{}, want: SnapshotContentDeletionPolicyRetain},
		{name: "capture, Retain", spec: SnapshotSpec{Mode: SnapshotModeCapture, DeletionPolicy: SnapshotContentDeletionPolicyRetain}, want: SnapshotContentDeletionPolicyRetain},
		{name: "capture, Delete", spec: SnapshotSpec{Mode: SnapshotModeCapture, DeletionPolicy: SnapshotContentDeletionPolicyDelete}, want: SnapshotContentDeletionPolicyDelete},
		{name: "mode omitted, Delete", spec: SnapshotSpec{DeletionPolicy: SnapshotContentDeletionPolicyDelete}, want: SnapshotContentDeletionPolicyDelete},
		{name: "import, field omitted", spec: SnapshotSpec{Mode: SnapshotModeImport}, want: SnapshotContentDeletionPolicyDelete},
		{name: "import, Retain", spec: SnapshotSpec{Mode: SnapshotModeImport, DeletionPolicy: SnapshotContentDeletionPolicyRetain}, want: SnapshotContentDeletionPolicyDelete},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Snapshot{Spec: tt.spec}
			if got := s.ContentDeletionPolicy(); got != tt.want {
				t.Fatalf("ContentDeletionPolicy() = %q, want %q", got, tt.want)
			}
		})
	}
}
