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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// ResolveChildSnapshotToBoundContentName resolves status.childrenSnapshotRefs (namespace+name only) to
// status.boundSnapshotContentName using only snapshot kinds registered in reg (DSC/bootstrap). Generic code
// does not hardcode domain snapshot CRDs.
func ResolveChildSnapshotToBoundContentName(ctx context.Context, c client.Reader, reg *snapshot.GVKRegistry, childNS, childName string) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("GVK registry is required to resolve child snapshot %s/%s", childNS, childName)
	}
	key := client.ObjectKey{Namespace: childNS, Name: childName}

	var (
		match    *unstructured.Unstructured
		matchGVK schema.GroupVersionKind
	)

	for _, sk := range reg.RegisteredSnapshotKinds() {
		snapGVK, err := reg.ResolveSnapshotGVK(sk)
		if err != nil {
			continue
		}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(snapGVK)
		if err := c.Get(ctx, key, u); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", fmt.Errorf("get %s %s/%s: %w", snapGVK.String(), childNS, childName, err)
		}
		if match != nil {
			return "", fmt.Errorf("%s/%s: %w: multiple registered snapshot kinds match (first %s, also %s)",
				childNS, childName, ErrRunGraphAmbiguousChildSnapshotRef, matchGVK.String(), snapGVK.String())
		}
		match = u
		matchGVK = snapGVK
	}
	if match == nil {
		return "", fmt.Errorf("%s/%s: %w: no registered snapshot kind matches this name in namespace (registered kinds: %v)",
			childNS, childName, ErrRunGraphChildSnapshotNotFound, reg.RegisteredSnapshotKinds())
	}

	bound, found, err := unstructured.NestedString(match.Object, "status", "boundSnapshotContentName")
	if err != nil {
		return "", fmt.Errorf("read status.boundSnapshotContentName from %s %s/%s: %w", matchGVK.String(), childNS, childName, err)
	}
	if !found || bound == "" {
		return "", fmt.Errorf("%s/%s: %w", childNS, childName, ErrRunGraphChildNotBound)
	}
	return bound, nil
}

// NamespaceSnapshotContentGVK returns the GVK for NamespaceSnapshotContent (storage API).
func NamespaceSnapshotContentGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   storagev1alpha1.APIGroup,
		Version: storagev1alpha1.APIVersion,
		Kind:    "NamespaceSnapshotContent",
	}
}
