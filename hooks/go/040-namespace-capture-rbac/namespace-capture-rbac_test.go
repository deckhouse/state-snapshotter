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

package namespace_capture_rbac

import (
	"testing"

	"github.com/deckhouse/state-snapshotter/hooks/go/consts"
)

// TestCaptureSubjectsIncludesControllerAndWebhooks pins the transient capture grant to both the controller
// SA (live-namespace read for capture) and the webhooks SA (so the MCR-validation webhook can resolve
// arbitrary namespaced CR targets via a dynamic Get during the capture window).
func TestCaptureSubjectsIncludesControllerAndWebhooks(t *testing.T) {
	subjects := captureSubjects()
	if len(subjects) != 2 {
		t.Fatalf("captureSubjects len = %d, want 2 (controller + webhooks)", len(subjects))
	}

	want := map[string]bool{consts.ControllerSAName: false, consts.WebhooksSAName: false}
	for _, s := range subjects {
		if s.Kind != "ServiceAccount" {
			t.Errorf("subject %q kind = %q, want ServiceAccount", s.Name, s.Kind)
		}
		if s.Namespace != consts.ModuleNamespace {
			t.Errorf("subject %q namespace = %q, want %q", s.Name, s.Namespace, consts.ModuleNamespace)
		}
		seen, expected := want[s.Name]
		if !expected {
			t.Errorf("unexpected subject %q", s.Name)
			continue
		}
		if seen {
			t.Errorf("duplicate subject %q", s.Name)
		}
		want[s.Name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("missing expected subject %q", name)
		}
	}
}

// TestDesiredCaptureRoleBindingShape verifies the managed binding targets the wildcard capture ClusterRole
// and carries the both-SA subject set plus the hook-managed marker label.
func TestDesiredCaptureRoleBindingShape(t *testing.T) {
	rb := desiredCaptureRoleBinding("some-ns")

	if rb.Namespace != "some-ns" {
		t.Errorf("namespace = %q, want some-ns", rb.Namespace)
	}
	if rb.Name != captureRoleBindingName {
		t.Errorf("name = %q, want %q", rb.Name, captureRoleBindingName)
	}
	if rb.RoleRef.Kind != "ClusterRole" || rb.RoleRef.Name != captureClusterRoleName {
		t.Errorf("roleRef = %s/%s, want ClusterRole/%s", rb.RoleRef.Kind, rb.RoleRef.Name, captureClusterRoleName)
	}
	if len(rb.Subjects) != 2 {
		t.Errorf("subjects len = %d, want 2", len(rb.Subjects))
	}
	if rb.Labels[captureRBACManagedLabelKey] != captureRBACManagedLabelValue {
		t.Errorf("managed label = %q, want %q", rb.Labels[captureRBACManagedLabelKey], captureRBACManagedLabelValue)
	}
}
