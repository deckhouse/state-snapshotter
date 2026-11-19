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

package csd

import (
	"context"
	"strings"
	"testing"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

var (
	domainSourceGVK   = schema.GroupVersionKind{Group: "example.deckhouse.io", Version: "v1alpha1", Kind: "Disk"}
	domainSnapshotGVK = schema.GroupVersionKind{Group: "example.deckhouse.io", Version: "v1alpha1", Kind: "DiskSnapshot"}
	configMapGVK      = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	daemonSetGVK      = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DaemonSet"}
)

func definition(name string, source, snapshot schema.GroupVersionKind) *storagev1alpha1.CustomSnapshotDefinition {
	return &storagev1alpha1.CustomSnapshotDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec: storagev1alpha1.CustomSnapshotDefinitionSpec{
			APIVersion: snapshot.GroupVersion().String(),
			Kind:       snapshot.Kind,
			Source:     storagev1alpha1.SnapshotGVKRef{APIVersion: source.GroupVersion().String(), Kind: source.Kind},
		},
	}
}

func domainSnapshotCRD() *extv1.CustomResourceDefinition {
	crd := crdWithVersionSchema(domainSnapshotGVK.Version, &extv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]extv1.JSONSchemaProps{
			"status": *statusSchema("boundSnapshotContentName", "conditions"),
		},
	})
	crd.Name = "disksnapshots." + domainSnapshotGVK.Group
	return crd
}

func reconcileDefinitions(t *testing.T, objs ...client.Object) map[string]*metav1.Condition {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, extv1.AddToScheme, storagev1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{domainSourceGVK, domainSnapshotGVK, configMapGVK, daemonSetGVK} {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&storagev1alpha1.CustomSnapshotDefinition{}).Build()
	r := &CustomSnapshotDefinitionReconciler{Client: c, Scheme: scheme, RESTMapper: mapper}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var list storagev1alpha1.CustomSnapshotDefinitionList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	accepted := map[string]*metav1.Condition{}
	for i := range list.Items {
		d := &list.Items[i]
		accepted[d.Name] = meta.FindStatusCondition(d.Status.Conditions, CSDConditionAccepted)
		if ready := meta.FindStatusCondition(d.Status.Conditions, CSDConditionReady); ready == nil || ready.Status != metav1.ConditionFalse {
			t.Errorf("%s: Ready is %v, want False without AccessGranted", d.Name, ready)
		}
	}
	return accepted
}

func TestSnapshotKindMustBeCustomResource(t *testing.T) {
	accepted := reconcileDefinitions(t,
		domainSnapshotCRD(),
		definition("domain", domainSourceGVK, domainSnapshotGVK),
		definition("configmap", domainSourceGVK, configMapGVK),
		definition("daemonset", domainSourceGVK, daemonSetGVK),
		definition("daemonset-again", configMapGVK, daemonSetGVK),
	)

	if got := accepted["domain"]; got == nil || got.Status != metav1.ConditionTrue {
		t.Fatalf("domain: Accepted is %v, want True for a kind served by a CRD with the status contract", got)
	}
	for name, crd := range map[string]string{
		"configmap":       `"configmaps."`,
		"daemonset":       `"daemonsets.apps"`,
		"daemonset-again": `"daemonsets.apps"`,
	} {
		got := accepted[name]
		if got == nil || got.Status != metav1.ConditionFalse || got.Reason != CSDReasonSnapshotKindNotCustomResource {
			t.Errorf("%s: Accepted is %v, want False with reason %s", name, got, CSDReasonSnapshotKindNotCustomResource)
			continue
		}
		if !strings.Contains(got.Message, crd) || got.ObservedGeneration != 1 {
			t.Errorf("%s: Accepted message %q (generation %d), want it to name the CustomResourceDefinition %s at generation 1", name, got.Message, got.ObservedGeneration, crd)
		}
	}
}
