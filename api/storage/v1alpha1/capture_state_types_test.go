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

import (
	"encoding/json"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// ChildrenSettled uses omitempty: a nil latch (no children / not computed) must not appear in JSON, so a
// leaf never carries the field. A non-nil latch round-trips under the canonical json tag childrenSettled.
func TestCommonControllerCaptureState_ChildrenSettledJSON(t *testing.T) {
	t.Run("nil is omitted", func(t *testing.T) {
		data, err := json.Marshal(CommonControllerCaptureState{})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := m["childrenSettled"]; ok {
			t.Fatalf("nil ChildrenSettled must be omitted from JSON, got %s", data)
		}
	})

	for _, val := range []bool{true, false} {
		t.Run("round-trips value", func(t *testing.T) {
			in := CommonControllerCaptureState{ChildrenSettled: boolPtr(val)}
			data, err := json.Marshal(in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var m map[string]interface{}
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("unmarshal to map: %v", err)
			}
			got, ok := m["childrenSettled"]
			if !ok {
				t.Fatalf("childrenSettled must be present for a non-nil latch, got %s", data)
			}
			if got != val {
				t.Fatalf("childrenSettled JSON value = %v, want %v", got, val)
			}
			var out CommonControllerCaptureState
			if err := json.Unmarshal(data, &out); err != nil {
				t.Fatalf("unmarshal to struct: %v", err)
			}
			if out.ChildrenSettled == nil || *out.ChildrenSettled != val {
				t.Fatalf("round-trip ChildrenSettled = %v, want %v", out.ChildrenSettled, val)
			}
		})
	}
}

// DeepCopy must copy the ChildrenSettled pointer by value so mutating the source never leaks into the copy
// (the field is a *bool latch, like SubtreePlanned / DataCaptured).
func TestCommonControllerCaptureState_ChildrenSettledDeepCopy(t *testing.T) {
	src := &CommonControllerCaptureState{ChildrenSettled: boolPtr(true)}
	cp := src.DeepCopy()
	if cp.ChildrenSettled == nil || !*cp.ChildrenSettled {
		t.Fatalf("deepcopy must preserve ChildrenSettled=true, got %v", cp.ChildrenSettled)
	}
	if cp.ChildrenSettled == src.ChildrenSettled {
		t.Fatalf("deepcopy must allocate a distinct *bool, not alias the source pointer")
	}
	*src.ChildrenSettled = false
	if cp.ChildrenSettled == nil || !*cp.ChildrenSettled {
		t.Fatalf("mutating the source must not affect the copy, got %v", cp.ChildrenSettled)
	}
}
