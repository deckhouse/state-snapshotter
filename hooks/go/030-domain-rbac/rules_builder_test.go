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
	diskSnapGVR := schema.GroupVersionResource{Group: "sds-unified-snapshots-poc.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualdisksnapshots"}
	vmSnapGVR := schema.GroupVersionResource{Group: "sds-unified-snapshots-poc.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualmachinesnapshots"}

	rules := buildDataExportReadRules([]schema.GroupVersionResource{vmSnapGVR, diskSnapGVR})

	want := []rbacv1.PolicyRule{
		{
			APIGroups: []string{"sds-unified-snapshots-poc.deckhouse.io"},
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

// The CORE SA grant on CSD snapshot GVRs: planner verbs (create+patch, no delete, no /finalizers) on the
// resource, status-write on /status. Groups are emitted in sorted order; duplicate GVRs (two CSDs mapping
// to the same kind) collapse.
func TestBuildCoreReadRulesGrantsPlannerAndStatusWrite(t *testing.T) {
	diskSnapGVR := schema.GroupVersionResource{Group: "sds-unified-snapshots-poc.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualdisksnapshots"}
	vmSnapGVR := schema.GroupVersionResource{Group: "sds-unified-snapshots-poc.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualmachinesnapshots"}
	otherSnapGVR := schema.GroupVersionResource{Group: "a.example.com", Version: "v1", Resource: "widgetsnapshots"}

	rules := buildCoreReadRules([]schema.GroupVersionResource{vmSnapGVR, diskSnapGVR, otherSnapGVR, diskSnapGVR})

	want := []rbacv1.PolicyRule{
		{
			APIGroups: []string{"a.example.com"},
			Resources: []string{"widgetsnapshots"},
			Verbs:     []string{"get", "list", "watch", "create", "patch"},
		},
		{
			APIGroups: []string{"a.example.com"},
			Resources: []string{"widgetsnapshots/status"},
			Verbs:     []string{"get", "update", "patch"},
		},
		{
			APIGroups: []string{"sds-unified-snapshots-poc.deckhouse.io"},
			Resources: []string{"demovirtualdisksnapshots", "demovirtualmachinesnapshots"},
			Verbs:     []string{"get", "list", "watch", "create", "patch"},
		},
		{
			APIGroups: []string{"sds-unified-snapshots-poc.deckhouse.io"},
			Resources: []string{"demovirtualdisksnapshots/status", "demovirtualmachinesnapshots/status"},
			Verbs:     []string{"get", "update", "patch"},
		},
	}

	if !reflect.DeepEqual(rules, want) {
		t.Fatalf("rules = %#v, want %#v", rules, want)
	}
}

func TestBuildCoreReadRulesEmpty(t *testing.T) {
	if rules := buildCoreReadRules(nil); rules != nil {
		t.Fatalf("expected nil rules for no snapshot GVRs, got %#v", rules)
	}
}

// The CORE SA grant on CSD source GVRs is strictly read-only and informer-free: get (per-target manifest
// capture) + list (parent-graph planning), no watch, no mutation verbs.
func TestBuildCoreSourceReadRulesReadOnlyNoWatch(t *testing.T) {
	diskGVR := schema.GroupVersionResource{Group: "sds-unified-snapshots-poc.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualdisks"}
	vmGVR := schema.GroupVersionResource{Group: "sds-unified-snapshots-poc.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualmachines"}

	rules := buildCoreSourceReadRules([]schema.GroupVersionResource{vmGVR, diskGVR, vmGVR})

	want := []rbacv1.PolicyRule{
		{
			APIGroups: []string{"sds-unified-snapshots-poc.deckhouse.io"},
			Resources: []string{"demovirtualdisks", "demovirtualmachines"},
			Verbs:     []string{"get", "list"},
		},
	}

	if !reflect.DeepEqual(rules, want) {
		t.Fatalf("rules = %#v, want %#v", rules, want)
	}

	if rules := buildCoreSourceReadRules(nil); rules != nil {
		t.Fatalf("expected nil rules for no source GVRs, got %#v", rules)
	}
}

// The restore-delegation grant: get on <resource>/manifests-with-data-restoration in the domain's
// "subresources."-prefixed API group, so core can call the out-of-process domain apiserver when the
// restore compiler delegates a domain subtree.
func TestDomainRestoreSubresourceRulesPrefixedGetOnly(t *testing.T) {
	diskSnapGVR := schema.GroupVersionResource{Group: "sds-unified-snapshots-poc.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualdisksnapshots"}
	vmSnapGVR := schema.GroupVersionResource{Group: "sds-unified-snapshots-poc.deckhouse.io", Version: "v1alpha1", Resource: "demovirtualmachinesnapshots"}

	rules := domainRestoreSubresourceRules([]schema.GroupVersionResource{vmSnapGVR, diskSnapGVR})

	want := []rbacv1.PolicyRule{
		{
			APIGroups: []string{"subresources.sds-unified-snapshots-poc.deckhouse.io"},
			Resources: []string{
				"demovirtualdisksnapshots/manifests-with-data-restoration",
				"demovirtualmachinesnapshots/manifests-with-data-restoration",
			},
			Verbs: []string{"get"},
		},
	}

	if !reflect.DeepEqual(rules, want) {
		t.Fatalf("rules = %#v, want %#v", rules, want)
	}

	if rules := domainRestoreSubresourceRules(nil); rules != nil {
		t.Fatalf("expected nil rules for no snapshot GVRs, got %#v", rules)
	}
}
