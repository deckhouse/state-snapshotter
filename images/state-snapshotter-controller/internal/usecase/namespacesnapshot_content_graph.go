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
// via status.childrenSnapshotContentRefs (PR5a heterogeneous graph).
type DemoVirtualDiskSnapshotContentLeafVisit func(ctx context.Context, d *demov1alpha1.DemoVirtualDiskSnapshotContent) error

// DemoVirtualMachineSnapshotContentLeafVisit is invoked when entering a DemoVirtualMachineSnapshotContent node
// before recursing into its status.childrenSnapshotContentRefs (PR5b).
type DemoVirtualMachineSnapshotContentLeafVisit func(ctx context.Context, m *demov1alpha1.DemoVirtualMachineSnapshotContent) error

// DemoSnapshotContentLeaves optional callbacks for non-NSC content nodes under childrenSnapshotContentRefs.
type DemoSnapshotContentLeaves struct {
	DiskContent    DemoVirtualDiskSnapshotContentLeafVisit
	MachineContent DemoVirtualMachineSnapshotContentLeafVisit
}

func nscVisitedKey(name string) string { return "nsc:" + name }

func demovmVisitedKey(name string) string { return "demovm:" + name }

// WalkNamespaceSnapshotContentSubtree visits every NamespaceSnapshotContent reachable from rootNSCName
// following only status.childrenSnapshotContentRefs (see PR4 spec §2.2; system-spec §3.4 INV-REF-C1).
// It does not list NamespaceSnapshotContent or NamespaceSnapshot to discover children.
// The same visited set is used for the whole walk (cycle detection).
//
// Child refs that name Demo*SnapshotContent instead of a NamespaceSnapshotContent are skipped for MCP collection
// (no error) unless optional callbacks are supplied via WalkNamespaceSnapshotContentSubtreeWithAllDemoLeaves.
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
// diskContentVisit for each DemoVirtualDiskSnapshotContent child (sorted with NSC children by ref Name).
func WalkNamespaceSnapshotContentSubtreeWithDemoLeaves(
	ctx context.Context,
	c client.Reader,
	rootNSCName string,
	visit NamespaceSnapshotContentVisit,
	diskContentVisit DemoVirtualDiskSnapshotContentLeafVisit,
) error {
	visited := make(map[string]struct{})
	var leaves *DemoSnapshotContentLeaves
	if diskContentVisit != nil {
		leaves = &DemoSnapshotContentLeaves{DiskContent: diskContentVisit}
	}
	return walkNamespaceSnapshotContentSubtree(ctx, c, rootNSCName, visited, visit, leaves)
}

// WalkNamespaceSnapshotContentSubtreeWithAllDemoLeaves is like WalkNamespaceSnapshotContentSubtreeWithDemoLeaves
// but allows both disk and DemoVirtualMachineSnapshotContent callbacks (PR5b).
func WalkNamespaceSnapshotContentSubtreeWithAllDemoLeaves(
	ctx context.Context,
	c client.Reader,
	rootNSCName string,
	visit NamespaceSnapshotContentVisit,
	leaves *DemoSnapshotContentLeaves,
) error {
	visited := make(map[string]struct{})
	return walkNamespaceSnapshotContentSubtree(ctx, c, rootNSCName, visited, visit, leaves)
}

func walkNamespaceSnapshotContentSubtree(
	ctx context.Context,
	c client.Reader,
	nscName string,
	visited map[string]struct{},
	visit NamespaceSnapshotContentVisit,
	leaves *DemoSnapshotContentLeaves,
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
		if err := walkChildSnapshotContentRef(ctx, c, children[i].Name, visited, visit, leaves); err != nil {
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
	leaves *DemoSnapshotContentLeaves,
) error {
	childNSC := &storagev1alpha1.NamespaceSnapshotContent{}
	err := c.Get(ctx, client.ObjectKey{Name: childName}, childNSC)
	if err == nil {
		return walkNamespaceSnapshotContentSubtree(ctx, c, childName, visited, visit, leaves)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get NamespaceSnapshotContent %q: %w", childName, err)
	}

	demoDisk := &demov1alpha1.DemoVirtualDiskSnapshotContent{}
	if err2 := c.Get(ctx, client.ObjectKey{Name: childName}, demoDisk); err2 == nil {
		if leaves != nil && leaves.DiskContent != nil {
			if err := leaves.DiskContent(ctx, demoDisk); err != nil {
				return err
			}
		}
		return nil
	} else if !apierrors.IsNotFound(err2) {
		return fmt.Errorf("get DemoVirtualDiskSnapshotContent %q: %w", childName, err2)
	}

	demoVM := &demov1alpha1.DemoVirtualMachineSnapshotContent{}
	err3 := c.Get(ctx, client.ObjectKey{Name: childName}, demoVM)
	if err3 == nil {
		return walkDemoVirtualMachineSnapshotContentSubtree(ctx, c, childName, demoVM, visited, visit, leaves)
	}
	if apierrors.IsNotFound(err3) {
		return fmt.Errorf("child ref %q: not NamespaceSnapshotContent, DemoVirtualDiskSnapshotContent, or DemoVirtualMachineSnapshotContent", childName)
	}
	return fmt.Errorf("get DemoVirtualMachineSnapshotContent %q: %w", childName, err3)
}

func walkDemoVirtualMachineSnapshotContentSubtree(
	ctx context.Context,
	c client.Reader,
	vmContentName string,
	vm *demov1alpha1.DemoVirtualMachineSnapshotContent,
	visited map[string]struct{},
	visit NamespaceSnapshotContentVisit,
	leaves *DemoSnapshotContentLeaves,
) error {
	key := demovmVisitedKey(vmContentName)
	if _, ok := visited[key]; ok {
		return fmt.Errorf("%w at DemoVirtualMachineSnapshotContent %q", ErrNamespaceSnapshotContentCycle, vmContentName)
	}
	visited[key] = struct{}{}

	if leaves != nil && leaves.MachineContent != nil {
		if err := leaves.MachineContent(ctx, vm); err != nil {
			return err
		}
	}

	children := append([]storagev1alpha1.NamespaceSnapshotContentChildRef(nil), vm.Status.ChildrenSnapshotContentRefs...)
	sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })
	for i := range children {
		if children[i].Name == "" {
			continue
		}
		if err := walkChildSnapshotContentRef(ctx, c, children[i].Name, visited, visit, leaves); err != nil {
			return err
		}
	}
	return nil
}
