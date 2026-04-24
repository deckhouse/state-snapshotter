/*
Copyright 2025 Flant JSC

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

package usecase

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func TestCollectRunSubtreeManifestExcludeKeys_GraphRegistryNotReady(t *testing.T) {
	ctx := context.Background()
	scheme := rootCaptureTestScheme(t)
	log, _ := logger.NewLogger("error")

	nscRoot := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-nsc"},
		Status: storagev1alpha1.NamespaceSnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: "child-nsc"}},
		},
	}
	nscChild := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "child-nsc"},
		Status:     storagev1alpha1.NamespaceSnapshotContentStatus{},
	}
	childSnap := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "ch1", Namespace: "ns1"},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			BoundSnapshotContentName: "child-nsc",
		},
	}
	rootNS := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.NamespaceSnapshotChildRef{
				{
					APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
					Kind:       "NamespaceSnapshot",
					Namespace:  "ns1",
					Name:       "ch1",
				},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nscRoot, nscChild, childSnap, rootNS).Build()
	arch := NewArchiveService(cl, cl, log)

	_, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, cl, snapshotgraphregistry.NewStatic(nil), rootNS, "root-nsc")
	if err == nil {
		t.Fatal("expected error when GVK registry is nil with non-empty childrenSnapshotRefs")
	}
	if !errors.Is(err, snapshotgraphregistry.ErrGraphRegistryNotReady) {
		t.Fatalf("expected ErrGraphRegistryNotReady, got %v", err)
	}
}

func TestCollectRunSubtreeManifestExcludeKeys_DescendantNSCWithoutMCPPends(t *testing.T) {
	ctx := context.Background()
	scheme := rootCaptureTestScheme(t)
	log, _ := logger.NewLogger("error")
	reg := graphRegistryForRootCapture(t)

	nscRoot := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-nsc"},
		Status: storagev1alpha1.NamespaceSnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: "child-nsc"}},
		},
	}
	nscChild := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "child-nsc"},
		Status:     storagev1alpha1.NamespaceSnapshotContentStatus{},
	}
	childSnap := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "ch1", Namespace: "ns1"},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			BoundSnapshotContentName: "child-nsc",
		},
	}
	rootNS := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.NamespaceSnapshotChildRef{
				{
					APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
					Kind:       "NamespaceSnapshot",
					Namespace:  "ns1",
					Name:       "ch1",
				},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nscRoot, nscChild, childSnap, rootNS).Build()
	arch := NewArchiveService(cl, cl, log)

	_, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, cl, snapshotgraphregistry.NewStatic(reg), rootNS, "root-nsc")
	if err == nil {
		t.Fatal("expected pending error when descendant NSC has no manifestCheckpointName")
	}
	if !errors.Is(err, ErrSubtreeManifestCapturePending) {
		t.Fatalf("expected ErrSubtreeManifestCapturePending, got %v", err)
	}
}

func TestCollectRunSubtreeManifestExcludeKeys_ChildNotBoundNoExclude(t *testing.T) {
	ctx := context.Background()
	scheme := rootCaptureTestScheme(t)
	log, _ := logger.NewLogger("error")
	reg := graphRegistryForRootCapture(t)

	nscRoot := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-nsc"},
		Status:     storagev1alpha1.NamespaceSnapshotContentStatus{},
	}
	childSnap := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "ch1", Namespace: "ns1"},
		Status:     storagev1alpha1.NamespaceSnapshotStatus{},
	}
	rootNS := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.NamespaceSnapshotChildRef{
				{
					APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
					Kind:       "NamespaceSnapshot",
					Namespace:  "ns1",
					Name:       "ch1",
				},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nscRoot, childSnap, rootNS).Build()
	arch := NewArchiveService(cl, cl, log)

	_, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, cl, snapshotgraphregistry.NewStatic(reg), rootNS, "root-nsc")
	if err == nil {
		t.Fatal("expected error when child snapshot is not bound")
	}
	if !errors.Is(err, ErrRunGraphChildNotBound) {
		t.Fatalf("expected ErrRunGraphChildNotBound, got %v", err)
	}
}
