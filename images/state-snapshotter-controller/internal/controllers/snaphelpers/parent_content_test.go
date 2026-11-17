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

package snaphelpers

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// The parent content resolved for a child is both its owner and the source of its deletionPolicy, so it
// must be the very content the parent Snapshot is bound to, and the owner reference must be built from it.
func TestResolveParentSnapshotContent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	root := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1", UID: types.UID("root-uid")},
		Status:     storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: "root-content"},
	}
	unbound := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "unbound", Namespace: "ns1", UID: types.UID("unbound-uid")},
	}
	rootContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content", UID: types.UID("root-content-uid")},
		Spec:       storagev1alpha1.SnapshotContentSpec{DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyDelete},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, unbound, rootContent).Build()

	childOf := func(parent *storagev1alpha1.Snapshot) *storagev1alpha1.Snapshot {
		child := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "ns1"}}
		if parent != nil {
			controller := true
			child.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       KindSnapshot,
				Name:       parent.Name,
				UID:        parent.UID,
				Controller: &controller,
			}}
		}
		return child
	}
	ctx := context.Background()

	got, pending, err := ResolveParentSnapshotContent(ctx, c, childOf(root))
	if err != nil || pending || got == nil {
		t.Fatalf("bound parent: got=%v pending=%v err=%v", got, pending, err)
	}
	if got.Name != "root-content" || got.Spec.DeletionPolicy != storagev1alpha1.SnapshotContentDeletionPolicyDelete {
		t.Fatalf("bound parent: resolved %q with deletionPolicy %q", got.Name, got.Spec.DeletionPolicy)
	}
	ref, pending, err := ResolveParentSnapshotContentOwnerRef(ctx, c, childOf(root))
	if err != nil || pending || ref == nil || ref.Name != "root-content" || ref.UID != rootContent.UID {
		t.Fatalf("owner ref of a bound parent: ref=%v pending=%v err=%v", ref, pending, err)
	}

	if got, pending, err := ResolveParentSnapshotContent(ctx, c, childOf(unbound)); err != nil || !pending || got != nil {
		t.Fatalf("unbound parent must be pending: got=%v pending=%v err=%v", got, pending, err)
	}
	if got, pending, err := ResolveParentSnapshotContent(ctx, c, childOf(nil)); err != nil || pending || got != nil {
		t.Fatalf("no parent: got=%v pending=%v err=%v", got, pending, err)
	}
}
