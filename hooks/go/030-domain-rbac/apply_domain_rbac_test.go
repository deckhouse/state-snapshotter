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
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/state-snapshotter/hooks/go/consts"
)

// The split model has three managed ClusterRole+binding pairs, one per consumer SA. The webhooks pair is
// what lets MCR target validation resolve domain CRs at admission time: without it the webhook's dynamic Get
// is denied outside the transient capture window, and a denied Get is not evidence that the target is absent.
func TestApplyDomainRBACBindsEachSAToItsOwnRole(t *testing.T) {
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(cleanupTestScheme(t)).Build()

	coreRules := []rbacv1.PolicyRule{{
		APIGroups: []string{"acme.deckhouse.io"},
		Resources: []string{"widgets"},
		Verbs:     []string{"get", "list"},
	}}
	webhookRules := []rbacv1.PolicyRule{{
		APIGroups: []string{"acme.deckhouse.io"},
		Resources: []string{"widgets"},
		Verbs:     []string{"get"},
	}}
	dataExportRules := []rbacv1.PolicyRule{{
		APIGroups: []string{"acme.deckhouse.io"},
		Resources: []string{"widgetsnapshots"},
		Verbs:     []string{"get"},
	}}

	if err := applyDomainRBAC(ctx, cl, coreRules, webhookRules, dataExportRules); err != nil {
		t.Fatalf("applyDomainRBAC: %v", err)
	}

	for _, tc := range []struct {
		role        string
		saName      string
		saNamespace string
		wantRules   []rbacv1.PolicyRule
	}{
		{consts.DomainCoreReadClusterRoleName, consts.ControllerSAName, consts.ModuleNamespace, coreRules},
		{consts.DomainWebhookReadClusterRoleName, consts.WebhooksSAName, consts.ModuleNamespace, webhookRules},
		{consts.DomainDataExportReadClusterRoleName, consts.DataExportControllerSAName, consts.DataExportModuleNamespace, dataExportRules},
	} {
		cr := new(rbacv1.ClusterRole)
		if err := cl.Get(ctx, ctrlclient.ObjectKey{Name: tc.role}, cr); err != nil {
			t.Fatalf("get ClusterRole %q: %v", tc.role, err)
		}
		if len(cr.Rules) != len(tc.wantRules) {
			t.Fatalf("ClusterRole %q rules = %#v, want %#v", tc.role, cr.Rules, tc.wantRules)
		}
		for i := range cr.Rules {
			if got, want := cr.Rules[i].String(), tc.wantRules[i].String(); got != want {
				t.Errorf("ClusterRole %q rule %d = %s, want %s", tc.role, i, got, want)
			}
		}

		crb := new(rbacv1.ClusterRoleBinding)
		if err := cl.Get(ctx, ctrlclient.ObjectKey{Name: tc.role}, crb); err != nil {
			t.Fatalf("get ClusterRoleBinding %q: %v", tc.role, err)
		}
		if crb.RoleRef.Name != tc.role || crb.RoleRef.Kind != "ClusterRole" {
			t.Errorf("ClusterRoleBinding %q roleRef = %+v, want ClusterRole/%s", tc.role, crb.RoleRef, tc.role)
		}
		if len(crb.Subjects) != 1 {
			t.Fatalf("ClusterRoleBinding %q subjects = %#v, want exactly one SA", tc.role, crb.Subjects)
		}
		got := crb.Subjects[0]
		if got.Kind != "ServiceAccount" || got.Name != tc.saName || got.Namespace != tc.saNamespace {
			t.Errorf("ClusterRoleBinding %q subject = %+v, want ServiceAccount %s/%s", tc.role, got, tc.saNamespace, tc.saName)
		}
	}
}

// The webhooks role must never be bound to the core controller SA (or vice versa): the two carry different
// verb sets on purpose (core lists sources during planning, the webhook only ever does a named Get).
func TestApplyDomainRBACKeepsWebhookRoleDistinctFromCore(t *testing.T) {
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(cleanupTestScheme(t)).Build()

	if err := applyDomainRBAC(ctx, cl, nil, nil, nil); err != nil {
		t.Fatalf("applyDomainRBAC: %v", err)
	}

	if consts.DomainWebhookReadClusterRoleName == consts.DomainCoreReadClusterRoleName {
		t.Fatal("webhook and core domain-read ClusterRole names must differ")
	}

	crb := new(rbacv1.ClusterRoleBinding)
	if err := cl.Get(ctx, ctrlclient.ObjectKey{Name: consts.DomainWebhookReadClusterRoleName}, crb); err != nil {
		t.Fatalf("get webhook ClusterRoleBinding: %v", err)
	}
	if crb.Subjects[0].Name == consts.ControllerSAName {
		t.Error("webhook domain-read binding must not target the core controller SA")
	}
}
