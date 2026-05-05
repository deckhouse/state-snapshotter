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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
)

const SnapshotContentKind = "SnapshotContent"

// RefGVK parses apiVersion/kind from a SnapshotChildRef (strict; no registry).
func RefGVK(ref storagev1alpha1.SnapshotChildRef) (schema.GroupVersionKind, error) {
	if ref.APIVersion == "" || ref.Kind == "" || ref.Name == "" {
		return schema.GroupVersionKind{}, fmt.Errorf("childrenSnapshotRefs entry must set apiVersion, kind, and name")
	}
	gvk := schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind)
	if gvk.Kind == "" || gvk.Version == "" {
		return schema.GroupVersionKind{}, fmt.Errorf("childrenSnapshotRefs apiVersion/kind: unresolved GVK from apiVersion=%q kind=%q", ref.APIVersion, ref.Kind)
	}
	return gvk, nil
}

// GetChildSnapshot resolves one status.childrenSnapshotRefs entry with a single Get (strict GVK).
// parentSnapshotNamespace is always used for child namespace (namespace-local snapshot run).
func GetChildSnapshot(ctx context.Context, c client.Reader, ref storagev1alpha1.SnapshotChildRef, parentSnapshotNamespace string) (*unstructured.Unstructured, schema.GroupVersionKind, error) {
	gvk, err := RefGVK(ref)
	if err != nil {
		return nil, schema.GroupVersionKind{}, err
	}
	ns := parentSnapshotNamespace
	key := client.ObjectKey{Namespace: ns, Name: ref.Name}
	if ns == "" {
		key = client.ObjectKey{Name: ref.Name}
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, key, u); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, gvk, fmt.Errorf("%w: get %s %s", ErrRunGraphChildSnapshotNotFound, gvk.String(), key.String())
		}
		return nil, gvk, fmt.Errorf("get child snapshot %s: %w", key.String(), err)
	}
	return u, gvk, nil
}

// ResolveChildSnapshotRefToBoundContentName returns status.boundSnapshotContentName for a strict child ref.
func ResolveChildSnapshotRefToBoundContentName(ctx context.Context, c client.Reader, ref storagev1alpha1.SnapshotChildRef, parentSnapshotNamespace string) (string, error) {
	u, gvk, err := GetChildSnapshot(ctx, c, ref, parentSnapshotNamespace)
	if err != nil {
		return "", err
	}
	bound, found, err := unstructured.NestedString(u.Object, "status", "boundSnapshotContentName")
	if err != nil {
		return "", fmt.Errorf("read status.boundSnapshotContentName from %s %s/%s: %w", gvk.String(), u.GetNamespace(), u.GetName(), err)
	}
	if !found || bound == "" {
		return "", fmt.Errorf("%s/%s: %w", parentSnapshotNamespace, ref.Name, ErrRunGraphChildNotBound)
	}
	return bound, nil
}

// EnsureGVKRegistryFromLive returns live.Current(), attempting TryRefresh once when nil (same contract as
// ResolveChildSnapshotToBoundContentNameLive).
func EnsureGVKRegistryFromLive(ctx context.Context, live snapshotgraphregistry.LiveReader) (*snapshot.GVKRegistry, error) {
	if live == nil {
		return nil, fmt.Errorf("snapshot graph registry reader is nil")
	}
	reg := live.Current()
	if reg == nil {
		if err := live.TryRefresh(ctx); err != nil && !errors.Is(err, snapshotgraphregistry.ErrRefreshNotConfigured) {
			return nil, err
		}
		reg = live.Current()
	}
	if reg == nil {
		return nil, snapshotgraphregistry.ErrGraphRegistryNotReady
	}
	return reg, nil
}

// SnapshotContentGVK returns the GVK for the common target SnapshotContent (storage API).
func SnapshotContentGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   storagev1alpha1.APIGroup,
		Version: storagev1alpha1.APIVersion,
		Kind:    SnapshotContentKind,
	}
}

// SnapshotContentGVR returns the GVR for the common target SnapshotContent (storage API).
func SnapshotContentGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    storagev1alpha1.APIGroup,
		Version:  storagev1alpha1.APIVersion,
		Resource: "snapshotcontents",
	}
}
