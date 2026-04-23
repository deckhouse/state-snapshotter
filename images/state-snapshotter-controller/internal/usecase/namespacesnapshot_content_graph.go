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
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// ErrNamespaceSnapshotContentCycle is returned when childrenSnapshotContentRefs form a cycle.
var ErrNamespaceSnapshotContentCycle = errors.New("NamespaceSnapshotContent graph cycle")

// NamespaceSnapshotContentVisit is invoked once per NamespaceSnapshotContent in DFS order
// (parent before descendants; children sorted lexicographically by ref Name).
type NamespaceSnapshotContentVisit func(ctx context.Context, nsc *storagev1alpha1.NamespaceSnapshotContent) error

// DedicatedContentVisitHooks optional callbacks for non-NamespaceSnapshotContent nodes under
// status.childrenSnapshotContentRefs (domain snapshot content registered via DSC/bootstrap).
// Leaf is true when the node has no (or empty) status.childrenSnapshotContentRefs.
type DedicatedContentVisitHooks struct {
	Visit func(ctx context.Context, gvk schema.GroupVersionKind, contentName string, u *unstructured.Unstructured, leaf bool) error
}

func nscVisitedKey(name string) string { return "nsc:" + name }

func dedicatedContentVisitedKey(gvk schema.GroupVersionKind, name string) string {
	return fmt.Sprintf("content:%s/%s/%s", gvk.Group, gvk.Kind, name)
}

// WalkNamespaceSnapshotContentSubtree visits every NamespaceSnapshotContent reachable from rootNSCName
// following only status.childrenSnapshotContentRefs (see PR4 spec §2.2; system-spec §3.4 INV-REF-C1).
// It does not list NamespaceSnapshotContent or NamespaceSnapshot to discover children.
// Child refs must name other NamespaceSnapshotContent objects only; dedicated domain content requires
// WalkNamespaceSnapshotContentSubtreeWithRegistry.
func WalkNamespaceSnapshotContentSubtree(
	ctx context.Context,
	c client.Reader,
	rootNSCName string,
	visit NamespaceSnapshotContentVisit,
) error {
	visited := make(map[string]struct{})
	return walkNamespaceSnapshotContentSubtree(ctx, c, rootNSCName, visited, visit, nil, nil)
}

// WalkNamespaceSnapshotContentSubtreeWithRegistry is like WalkNamespaceSnapshotContentSubtree but resolves
// child refs to any SnapshotContent GVK registered in reg (DSC/bootstrap), using unstructured reads for
// status.childrenSnapshotContentRefs. Generic code does not import domain CRD packages.
func WalkNamespaceSnapshotContentSubtreeWithRegistry(
	ctx context.Context,
	c client.Reader,
	rootNSCName string,
	visit NamespaceSnapshotContentVisit,
	reg *snapshot.GVKRegistry,
	hooks *DedicatedContentVisitHooks,
) error {
	if reg == nil {
		return fmt.Errorf("WalkNamespaceSnapshotContentSubtreeWithRegistry requires non-nil GVK registry")
	}
	visited := make(map[string]struct{})
	return walkNamespaceSnapshotContentSubtree(ctx, c, rootNSCName, visited, visit, reg, hooks)
}

func walkNamespaceSnapshotContentSubtree(
	ctx context.Context,
	c client.Reader,
	nscName string,
	visited map[string]struct{},
	visit NamespaceSnapshotContentVisit,
	reg *snapshot.GVKRegistry,
	hooks *DedicatedContentVisitHooks,
) error {
	key := nscVisitedKey(nscName)
	if _, ok := visited[key]; ok {
		return fmt.Errorf("%w at NamespaceSnapshotContent %q", ErrNamespaceSnapshotContentCycle, nscName)
	}
	visited[key] = struct{}{}

	nsc := &storagev1alpha1.NamespaceSnapshotContent{}
	if err := c.Get(ctx, client.ObjectKey{Name: nscName}, nsc); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("NamespaceSnapshotContent %q not found: %w", nscName, err)
		}
		return fmt.Errorf("get NamespaceSnapshotContent %q: %w", nscName, err)
	}

	if err := visit(ctx, nsc); err != nil {
		return err
	}

	children := append([]storagev1alpha1.NamespaceSnapshotContentChildRef(nil), nsc.Status.ChildrenSnapshotContentRefs...)
	sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })
	for i := range children {
		if children[i].Name == "" {
			continue
		}
		if err := walkChildSnapshotContentRef(ctx, c, children[i].Name, visited, visit, reg, hooks); err != nil {
			return err
		}
	}
	return nil
}

func walkChildSnapshotContentRef(
	ctx context.Context,
	c client.Reader,
	childName string,
	visited map[string]struct{},
	visit NamespaceSnapshotContentVisit,
	reg *snapshot.GVKRegistry,
	hooks *DedicatedContentVisitHooks,
) error {
	childNSC := &storagev1alpha1.NamespaceSnapshotContent{}
	err := c.Get(ctx, client.ObjectKey{Name: childName}, childNSC)
	if err == nil {
		return walkNamespaceSnapshotContentSubtree(ctx, c, childName, visited, visit, reg, hooks)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get NamespaceSnapshotContent %q: %w", childName, err)
	}

	if reg == nil {
		return fmt.Errorf("child ref %q: not NamespaceSnapshotContent; heterogeneous graph requires GVK registry (DSC/bootstrap pairs)", childName)
	}

	nscGVK := NamespaceSnapshotContentGVK()
	for _, contentGVK := range reg.RegisteredContentGVKs() {
		if contentGVK.Group == nscGVK.Group && contentGVK.Version == nscGVK.Version && contentGVK.Kind == nscGVK.Kind {
			continue
		}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(contentGVK)
		if err2 := c.Get(ctx, client.ObjectKey{Name: childName}, u); err2 != nil {
			if apierrors.IsNotFound(err2) {
				continue
			}
			return fmt.Errorf("get %s %q: %w", contentGVK.String(), childName, err2)
		}
		return walkDedicatedSnapshotContentSubtree(ctx, c, childName, contentGVK, u, visited, visit, reg, hooks)
	}
	return fmt.Errorf("child ref %q: not a registered NamespaceSnapshotContent or SnapshotContent type", childName)
}

func walkDedicatedSnapshotContentSubtree(
	ctx context.Context,
	c client.Reader,
	contentName string,
	gvk schema.GroupVersionKind,
	u *unstructured.Unstructured,
	visited map[string]struct{},
	visit NamespaceSnapshotContentVisit,
	reg *snapshot.GVKRegistry,
	hooks *DedicatedContentVisitHooks,
) error {
	key := dedicatedContentVisitedKey(gvk, contentName)
	if _, ok := visited[key]; ok {
		return fmt.Errorf("%w at %s %q", ErrNamespaceSnapshotContentCycle, gvk.String(), contentName)
	}
	visited[key] = struct{}{}

	childNames, err := unstructuredChildrenSnapshotContentRefNames(u)
	if err != nil {
		return fmt.Errorf("%s %q: %w", gvk.String(), contentName, err)
	}
	leaf := len(childNames) == 0
	if hooks != nil && hooks.Visit != nil {
		if err := hooks.Visit(ctx, gvk, contentName, u, leaf); err != nil {
			return err
		}
	}
	if leaf {
		return nil
	}
	for _, ch := range childNames {
		if err := walkChildSnapshotContentRef(ctx, c, ch, visited, visit, reg, hooks); err != nil {
			return err
		}
	}
	return nil
}

func unstructuredChildrenSnapshotContentRefNames(u *unstructured.Unstructured) ([]string, error) {
	refs, found, err := unstructured.NestedSlice(u.Object, "status", "childrenSnapshotContentRefs")
	if err != nil {
		return nil, fmt.Errorf("read status.childrenSnapshotContentRefs: %w", err)
	}
	if !found || refs == nil {
		return nil, nil
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		m, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}
