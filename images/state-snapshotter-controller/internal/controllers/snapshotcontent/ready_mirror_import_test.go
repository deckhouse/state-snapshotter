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

package snapshotcontent

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// TestMirrorReadyToOwnerSnapshot_DomainImportCRAfterBind pins that main (= content-ctrl) mirrors
// content.Ready onto a DOMAIN import-CR the SAME way it does for a core Snapshot: the writer switch is keyed
// off the owner's status.boundSnapshotContentName (creator -> main), and it fires for ANY owner GVK
// resolved from content.spec.snapshotRef. Pre-bind the mirror is a no-op (the domain import-CR holds no
// pre-bind condition — interim void, decision 2026-07-17); once bound, content.Ready is projected verbatim
// (an import owner has no domain capture phase, so barrier 2 does not gate it).
func TestMirrorReadyToOwnerSnapshot_DomainImportCRAfterBind(t *testing.T) {
	ctx := context.Background()

	domainGVK := schema.GroupVersionKind{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualMachineSnapshot"}

	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	scheme.AddKnownTypeWithName(domainGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(domainGVK.GroupVersion().WithKind("DemoVirtualMachineSnapshotList"), &unstructured.UnstructuredList{})

	// The domain import-CR: import mode, no capture phase, initially UNBOUND.
	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(domainGVK)
	owner.SetNamespace("ns1")
	owner.SetName("dvm-import")
	if err := unstructured.SetNestedField(owner.Object, "Import", "spec", "mode"); err != nil {
		t.Fatalf("set spec.mode: %v", err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(owner).
		WithStatusSubresource(owner).
		Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	// The Ready import SnapshotContent whose spec.snapshotRef points back at the domain import-CR.
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "dvm-content"},
		Spec: storagev1alpha1.SnapshotContentSpec{
			DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyDelete,
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: domainGVK.GroupVersion().String(),
				Kind:       domainGVK.Kind,
				Namespace:  "ns1",
				Name:       "dvm-import",
			},
		},
	}
	content.Status.Conditions = append(content.Status.Conditions, metav1.Condition{
		Type:               snapshot.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             snapshot.ReasonCompleted,
		Message:            "ready",
		LastTransitionTime: metav1.Now(),
	})
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(content)
	if err != nil {
		t.Fatalf("to unstructured: %v", err)
	}
	contentObj := &unstructured.Unstructured{Object: raw}
	contentObj.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())

	ownerReady := func() *metav1.Condition {
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(domainGVK)
		if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns1", Name: "dvm-import"}, fresh); err != nil {
			t.Fatalf("get owner: %v", err)
		}
		freshLike, err := snapshot.ExtractSnapshotLike(fresh)
		if err != nil {
			t.Fatalf("extract snapshot like: %v", err)
		}
		return snapshot.GetCondition(freshLike, snapshot.ConditionReady)
	}

	// Pre-bind: the mirror is a no-op (writer switch not yet handed to main).
	if err := r.mirrorReadyToOwnerSnapshot(ctx, contentObj); err != nil {
		t.Fatalf("pre-bind mirror: %v", err)
	}
	if cond := ownerReady(); cond != nil {
		t.Fatalf("pre-bind domain import-CR must have NO Ready condition, got %s/%s", cond.Status, cond.Reason)
	}

	// Bind the domain import-CR to the content, then re-run the mirror.
	bound := &unstructured.Unstructured{}
	bound.SetGroupVersionKind(domainGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns1", Name: "dvm-import"}, bound); err != nil {
		t.Fatalf("get owner to bind: %v", err)
	}
	if err := unstructured.SetNestedField(bound.Object, "dvm-content", "status", "boundSnapshotContentName"); err != nil {
		t.Fatalf("set boundSnapshotContentName: %v", err)
	}
	if err := cl.Status().Update(ctx, bound); err != nil {
		t.Fatalf("bind owner: %v", err)
	}

	if err := r.mirrorReadyToOwnerSnapshot(ctx, contentObj); err != nil {
		t.Fatalf("post-bind mirror: %v", err)
	}
	cond := ownerReady()
	if cond == nil {
		t.Fatalf("post-bind domain import-CR must carry the mirrored Ready condition")
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != snapshot.ReasonCompleted {
		t.Fatalf("post-bind Ready = %s/%s, want %s/%s", cond.Status, cond.Reason, metav1.ConditionTrue, snapshot.ReasonCompleted)
	}
}
