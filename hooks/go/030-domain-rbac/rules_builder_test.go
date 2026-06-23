package domain_rbac

import (
	"reflect"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestBuildRulesIncludesSourceStatus(t *testing.T) {
	diskGVR := schema.GroupVersionResource{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualdisks"}
	vmGVR := schema.GroupVersionResource{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualmachines"}
	diskSnapGVR := schema.GroupVersionResource{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualdisksnapshots"}

	rules := buildRules(
		[]schema.GroupVersionResource{vmGVR, diskGVR},
		[]schema.GroupVersionResource{diskSnapGVR},
	)

	want := []rbacv1.PolicyRule{
		{
			APIGroups: []string{"demo.state-snapshotter.deckhouse.io"},
			Resources: []string{"demovirtualdisks", "demovirtualmachines"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{"demo.state-snapshotter.deckhouse.io"},
			Resources: []string{"demovirtualdisks/status", "demovirtualmachines/status"},
			Verbs:     []string{"get", "update", "patch"},
		},
		{
			APIGroups: []string{"demo.state-snapshotter.deckhouse.io"},
			Resources: []string{"demovirtualdisksnapshots"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
		},
		{
			APIGroups: []string{"demo.state-snapshotter.deckhouse.io"},
			Resources: []string{"demovirtualdisksnapshots/status"},
			Verbs:     []string{"get", "update", "patch"},
		},
		{
			APIGroups: []string{"demo.state-snapshotter.deckhouse.io"},
			Resources: []string{"demovirtualdisksnapshots/finalizers"},
			Verbs:     []string{"update", "patch"},
		},
	}

	if len(rules) != len(want) {
		t.Fatalf("rule count = %d, want %d: %#v", len(rules), len(want), rules)
	}
	for i := range want {
		if !reflect.DeepEqual(rules[i], want[i]) {
			t.Fatalf("rule[%d] = %#v, want %#v", i, rules[i], want[i])
		}
	}
}
