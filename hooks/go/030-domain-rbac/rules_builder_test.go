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

package domain_rbac

import (
	"reflect"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestBuildDataExportReadRulesIsReadOnlyOnSnapshotGVRs(t *testing.T) {
	diskSnapGVR := schema.GroupVersionResource{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualdisksnapshots"}
	vmSnapGVR := schema.GroupVersionResource{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualmachinesnapshots"}

	rules := buildDataExportReadRules([]schema.GroupVersionResource{vmSnapGVR, diskSnapGVR})

	want := []rbacv1.PolicyRule{
		{
			APIGroups: []string{"demo.state-snapshotter.deckhouse.io"},
			Resources: []string{"demovirtualdisksnapshots", "demovirtualmachinesnapshots"},
			Verbs:     []string{"get", "list", "watch"},
		},
	}

	if !reflect.DeepEqual(rules, want) {
		t.Fatalf("rules = %#v, want %#v", rules, want)
	}
}

func TestBuildDataExportReadRulesEmpty(t *testing.T) {
	if rules := buildDataExportReadRules(nil); rules != nil {
		t.Fatalf("expected nil rules for no snapshot GVRs, got %#v", rules)
	}
}
