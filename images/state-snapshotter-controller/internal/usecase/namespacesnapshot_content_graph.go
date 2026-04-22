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
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// ErrNamespaceSnapshotContentCycle is returned when childrenSnapshotContentRefs form a cycle.
var ErrNamespaceSnapshotContentCycle = errors.New("NamespaceSnapshotContent graph cycle")

// NamespaceSnapshotContentVisit is invoked once per NamespaceSnapshotContent in DFS order
// (parent before descendants; children sorted lexicographically by ref Name).
type NamespaceSnapshotContentVisit func(ctx context.Context, nsc *storagev1alpha1.NamespaceSnapshotContent) error

// DemoVirtualDiskSnapshotContentLeafVisit is invoked for each DemoVirtualDiskSnapshotContent reached as a leaf
// via status.childrenSnapshotContentRefs on a NamespaceSnapshotContent (PR5a heterogeneous graph).
type DemoVirtualDiskSnapshotContentLeafVisit func(ctx context.Context, d *demov1alpha1.DemoVirtualDiskSnapshotContent) error

// WalkNamespaceSnapshotContentSubtree visits every NamespaceSnapshotContent reachable from rootNSCName
// following only status.childrenSnapshotContentRefs (see PR4 spec §2.2; system-spec §3.4 INV-REF-C1).
// It does not list NamespaceSnapshotContent or NamespaceSnapshot to discover children.
// The same visited set is used for the whole walk (cycle detection).
//
// Child refs that name a DemoVirtualDiskSnapshotContent instead of a NamespaceSnapshotContent are skipped
// (no error) so ref-only walks used for aggregated manifests can coexist with PR5a domain leaves that have no MCP.
func WalkNamespaceSnapshotContentSubtree(
	ctx context.Context,
	c client.Reader,
	rootNSCName string,
	visit NamespaceSnapshotContentVisit,
) error {
	visited := make(map[string]struct{})
	return walkNamespaceSnapshotContentSubtree(ctx, c, rootNSCName, visited, visit, nil)
}

// WalkNamespaceSnapshotContentSubtreeWithDemoLeaves is like WalkNamespaceSnapshotContentSubtree but invokes
// demoLeafVisit for each DemoVirtualDiskSnapshotContent child (sorted with NSC children by ref Name).
// Domain leaves do not recurse further (no childrenSnapshotContentRefs on demo content in PR5a).
func WalkNamespaceSnapshotContentSubtreeWithDemoLeaves(
	ctx context.Context,
	c client.Reader,
	rootNSCName string,
	visit NamespaceSnapshotContentVisit,
	demoLeafVisit DemoVirtualDiskSnapshotContentLeafVisit,
) error {
	visited := make(map[string]struct{})
	return walkNamespaceSnapshotContentSubtree(ctx, c, rootNSCName, visited, visit, demoLeafVisit)
}

func walkNamespaceSnapshotContentSubtree(
	ctx context.Context,
	c client.Reader,
	nscName string,
	visited map[string]struct{},
	visit NamespaceSnapshotContentVisit,
	demoLeafVisit DemoVirtualDiskSnapshotContentLeafVisit,
) error {
	if _, ok := visited[nscName]; ok {
		return fmt.Errorf("%w at NamespaceSnapshotContent %q", ErrNamespaceSnapshotContentCycle, nscName)
	}
	visited[nscName] = struct{}{}

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
		if err := walkChildSnapshotContentRef(ctx, c, children[i].Name, visited, visit, demoLeafVisit); err != nil {
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
	demoLeafVisit DemoVirtualDiskSnapshotContentLeafVisit,
) error {
	childNSC := &storagev1alpha1.NamespaceSnapshotContent{}
	err := c.Get(ctx, client.ObjectKey{Name: childName}, childNSC)
	if err == nil {
		return walkNamespaceSnapshotContentSubtree(ctx, c, childName, visited, visit, demoLeafVisit)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get NamespaceSnapshotContent %q: %w", childName, err)
	}

	demo := &demov1alpha1.DemoVirtualDiskSnapshotContent{}
	if err2 := c.Get(ctx, client.ObjectKey{Name: childName}, demo); err2 == nil {
		if demoLeafVisit != nil {
			if err := demoLeafVisit(ctx, demo); err != nil {
				return err
			}
		}
		return nil
	} else if apierrors.IsNotFound(err2) {
		return fmt.Errorf("child ref %q: neither NamespaceSnapshotContent nor DemoVirtualDiskSnapshotContent", childName)
	} else {
		return fmt.Errorf("get DemoVirtualDiskSnapshotContent %q: %w", childName, err2)
	}
}
