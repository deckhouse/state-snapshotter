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

package crdtests

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

const (
	guardTemplate     = "../../templates/customsnapshotdefinition-guard.yaml"
	guardWriterRole   = "crdtests-csd-writer"
	guardAllRole      = "crdtests-everything"
	deckhouseSA       = "system:serviceaccount:d8-system:deckhouse"
	clusterAdminsName = "kubeadm:cluster-admins"
	guardForbidden    = "may be created, changed or deleted only by Deckhouse"
	guardBuiltinGroup = "which a CustomSnapshotDefinition may not name"
)

type guardActor struct {
	name   string
	user   string
	groups []string
}

var (
	actorDeckhouse     = guardActor{"deckhouse", deckhouseSA, []string{"system:serviceaccounts", "system:serviceaccounts:d8-system"}}
	actorClusterAdmins = guardActor{"kubeadm cluster-admins", "kubernetes-admin", []string{clusterAdminsName}}
	actorSuperAdmin    = guardActor{"superadmin", "crdtests-superadmin", nil}
	actorClusterAdmin  = guardActor{"cluster admin", "crdtests-cluster-admin", nil}
)

func TestCustomSnapshotDefinitionGuard(t *testing.T) {
	installGuard(t)
	ctx := context.Background()

	t.Run("create by", func(t *testing.T) {
		for _, tc := range []struct {
			actor   *guardActor
			allowed bool
		}{
			{&actorDeckhouse, true},
			{&actorClusterAdmins, true},
			{&actorSuperAdmin, true},
			{nil, true},
			{&actorClusterAdmin, false},
		} {
			name := "system-masters"
			if tc.actor != nil {
				name = tc.actor.name
			}
			t.Run(name, func(t *testing.T) {
				csd := guardCSD(objectName("guard-create", name), "virtualization.deckhouse.io/v1alpha1", "virtualization.deckhouse.io/v1alpha1")
				err := clientAs(t, tc.actor).Create(ctx, csd)
				if !tc.allowed {
					expectForbidden(t, err)
					return
				}
				if err != nil {
					t.Fatalf("want the definition admitted, got %v", err)
				}
				t.Cleanup(func() { _ = k8sClient.Delete(ctx, csd) })
			})
		}
	})

	t.Run("change by a subject without the register permission", func(t *testing.T) {
		csd := guardCSD("guard-change", "virtualization.deckhouse.io/v1alpha1", "virtualization.deckhouse.io/v1alpha1")
		create(t, csd)
		denied := clientAs(t, &actorClusterAdmin)

		spec := csd.DeepCopy()
		spec.Spec.Source.Kind = "Other"
		expectForbidden(t, denied.Update(ctx, spec))

		labels := csd.DeepCopy()
		labels.Labels = map[string]string{"example": "true"}
		expectForbidden(t, denied.Update(ctx, labels))

		status := csd.DeepCopy()
		status.Status.Conditions = []metav1.Condition{{
			Type: "Accepted", Status: metav1.ConditionFalse, Reason: "Probe", Message: "probe", LastTransitionTime: metav1.Now(),
		}}
		if err := denied.Status().Update(ctx, status); err != nil {
			t.Fatalf("want the status subresource left to RBAC, got %v", err)
		}

		expectForbidden(t, denied.Delete(ctx, csd.DeepCopy()))
	})

	t.Run("built-in group in", func(t *testing.T) {
		const allowed = "virtualization.deckhouse.io/v1alpha1"
		for _, apiVersion := range []string{
			"v1",
			"/v1",
			"apps/v1",
			"batch/v1",
			"rbac.authorization.k8s.io/v1",
			"admissionregistration.k8s.io/v1",
		} {
			for _, field := range []string{"spec.source.apiVersion", "spec.apiVersion"} {
				source, snapshot := apiVersion, allowed
				if field == "spec.apiVersion" {
					source, snapshot = allowed, apiVersion
				}
				for _, actor := range []*guardActor{&actorDeckhouse, &actorSuperAdmin, nil} {
					who := "system-masters"
					if actor != nil {
						who = actor.name
					}
					t.Run(field+" "+apiVersion+" by "+who, func(t *testing.T) {
						csd := guardCSD(objectName("guard-builtin", field+"-"+apiVersion+"-"+who), source, snapshot)
						expectAdmission(t, clientAs(t, actor).Create(ctx, csd), field+" "+apiVersion+" ", guardBuiltinGroup)
					})
				}
			}
		}
	})

	t.Run("group that only starts like a built-in one", func(t *testing.T) {
		for _, apiVersion := range []string{"apps.example.io/v1", "batch.deckhouse.io/v1alpha1", "rbac.example.io/v1"} {
			t.Run(apiVersion, func(t *testing.T) {
				csd := guardCSD(objectName("guard-lookalike", apiVersion), apiVersion, apiVersion)
				if err := clientAs(t, &actorDeckhouse).Create(ctx, csd); err != nil {
					t.Fatalf("want the definition admitted, got %v", err)
				}
				t.Cleanup(func() { _ = k8sClient.Delete(ctx, csd) })
			})
		}
	})

	t.Run("change into a built-in group", func(t *testing.T) {
		csd := guardCSD("guard-change-builtin", "virtualization.deckhouse.io/v1alpha1", "virtualization.deckhouse.io/v1alpha1")
		create(t, csd)
		changed := csd.DeepCopy()
		changed.Spec.Source = ssv1alpha1.SnapshotGVKRef{APIVersion: "v1", Kind: "Secret"}
		expectAdmission(t, clientAs(t, &actorDeckhouse).Update(ctx, changed), "spec.source.apiVersion v1 ", guardBuiltinGroup)
		if err := clientAs(t, &actorDeckhouse).Delete(ctx, csd.DeepCopy()); err != nil {
			t.Fatalf("want the definition deleted by Deckhouse, got %v", err)
		}
	})
}

func guardCSD(name, sourceAPIVersion, snapshotAPIVersion string) *ssv1alpha1.CustomSnapshotDefinition {
	return &ssv1alpha1.CustomSnapshotDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ssv1alpha1.CustomSnapshotDefinitionSpec{
			APIVersion: snapshotAPIVersion,
			Kind:       "GuardSnapshot",
			Source:     ssv1alpha1.SnapshotGVKRef{APIVersion: sourceAPIVersion, Kind: "GuardSource"},
		},
	}
}

func clientAs(t *testing.T, actor *guardActor) client.Client {
	t.Helper()
	if actor == nil {
		return k8sClient
	}
	cfg := rest.CopyConfig(restConfig)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: actor.user, Groups: actor.groups}
	c, err := client.New(cfg, client.Options{Scheme: k8sClient.Scheme()})
	if err != nil {
		t.Fatalf("client as %s: %v", actor.user, err)
	}
	return c
}

func expectForbidden(t *testing.T, err error) {
	t.Helper()
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), guardForbidden) {
		t.Fatalf("want a Forbidden error with %q, got %v", guardForbidden, err)
	}
}

func installGuard(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	data, err := os.ReadFile(guardTemplate)
	if err != nil {
		t.Fatalf("read %s: %v", guardTemplate, err)
	}
	rendered := strings.ReplaceAll(string(data), "{{ .Chart.Name }}", "state-snapshotter")
	if strings.Contains(rendered, "{{") {
		t.Fatalf("%s holds a Helm action other than {{ .Chart.Name }}, which this test does not render", guardTemplate)
	}
	var policy *admissionregistrationv1.ValidatingAdmissionPolicy
	var binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
	for _, doc := range strings.Split(rendered, "\n---") {
		if strings.TrimSpace(strings.TrimPrefix(doc, "---")) == "" {
			continue
		}
		var meta metav1.TypeMeta
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatalf("parse %s: %v", guardTemplate, err)
		}
		switch meta.Kind {
		case "ValidatingAdmissionPolicy":
			policy = &admissionregistrationv1.ValidatingAdmissionPolicy{}
			err = yaml.UnmarshalStrict([]byte(doc), policy)
		case "ValidatingAdmissionPolicyBinding":
			binding = &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
			err = yaml.UnmarshalStrict([]byte(doc), binding)
		default:
			t.Fatalf("%s holds a %q, want only the policy and its binding", guardTemplate, meta.Kind)
		}
		if err != nil {
			t.Fatalf("parse %s %s: %v", guardTemplate, meta.Kind, err)
		}
	}
	if policy == nil || binding == nil {
		t.Fatalf("%s must hold a ValidatingAdmissionPolicy and its binding", guardTemplate)
	}

	writer := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: guardWriterRole},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{ssv1alpha1.SchemeGroupVersion.Group},
			Resources: []string{"customsnapshotdefinitions", "customsnapshotdefinitions/status"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
		}},
	}
	everything := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: guardAllRole},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}},
			{NonResourceURLs: []string{"*"}, Verbs: []string{"*"}},
		},
	}
	writerBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: guardWriterRole},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: guardWriterRole},
		Subjects: []rbacv1.Subject{
			{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: deckhouseSA},
			{APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: clusterAdminsName},
			{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: actorClusterAdmin.user},
		},
	}
	everythingBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: guardAllRole},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: guardAllRole},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: actorSuperAdmin.user}},
	}
	for _, obj := range []client.Object{writer, everything, writerBinding, everythingBinding, policy, binding} {
		create(t, obj)
	}

	probe := guardCSD("guard-probe", "virtualization.deckhouse.io/v1alpha1", "virtualization.deckhouse.io/v1alpha1")
	denied := clientAs(t, &actorClusterAdmin)
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := denied.Create(ctx, probe.DeepCopy())
		if apierrors.IsForbidden(err) && strings.Contains(err.Error(), guardForbidden) {
			return
		}
		if err == nil {
			_ = k8sClient.Delete(ctx, probe.DeepCopy())
		}
		if time.Now().After(deadline) {
			t.Fatalf("the policy is not enforced after 30s, the last create answered %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
