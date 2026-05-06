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

package demo

import (
	"context"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func TestEnsureDemoSnapshotContentCreatesContentWithOwnerRef(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t)
	ownerRef := metav1.OwnerReference{
		APIVersion: controllercommon.DeckhouseAPIVersion,
		Kind:       controllercommon.KindObjectKeeper,
		Name:       "ret-snap-demo",
		UID:        "ok-uid",
		Controller: boolPtr(true),
	}

	if err := ensureDemoSnapshotContent(context.Background(), cl, "demo-content", ownerRef); err != nil {
		t.Fatalf("ensureDemoSnapshotContent failed: %v", err)
	}

	content := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "demo-content"}, content); err != nil {
		t.Fatalf("get created SnapshotContent: %v", err)
	}
	if !controllercommon.OwnerReferencesEqual(content.OwnerReferences, []metav1.OwnerReference{ownerRef}) {
		t.Fatalf("expected ownerRef %#v, got %#v", ownerRef, content.OwnerReferences)
	}
}

func TestEnsureDemoSnapshotContentAddsLifecycleOwnerToExistingContent(t *testing.T) {
	unrelated := metav1.OwnerReference{APIVersion: "example.io/v1", Kind: "AuditAnchor", Name: "audit"}
	existing := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "demo-content",
			OwnerReferences: []metav1.OwnerReference{unrelated},
		},
	}
	cl := newDemoSourceRefFakeClient(t, existing)
	ownerRef := metav1.OwnerReference{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "SnapshotContent",
		Name:       "parent-content",
		UID:        "parent-uid",
		Controller: boolPtr(true),
	}

	if err := ensureDemoSnapshotContent(context.Background(), cl, "demo-content", ownerRef); err != nil {
		t.Fatalf("ensureDemoSnapshotContent failed: %v", err)
	}

	content := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "demo-content"}, content); err != nil {
		t.Fatalf("get updated SnapshotContent: %v", err)
	}
	assertDemoOwnerRef(t, content.OwnerReferences, unrelated.APIVersion, unrelated.Kind, unrelated.Name, false)
	assertDemoOwnerRef(t, content.OwnerReferences, ownerRef.APIVersion, ownerRef.Kind, ownerRef.Name, true)
}

func boolPtr(v bool) *bool {
	return &v
}
