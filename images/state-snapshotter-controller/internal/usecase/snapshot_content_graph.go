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

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// ErrSnapshotContentCycle is returned when childrenSnapshotContentRefs form a cycle.
var ErrSnapshotContentCycle = errors.New("SnapshotContent graph cycle")

// SnapshotContentVisit is invoked once per SnapshotContent in DFS order
// (parent before descendants; children sorted lexicographically by ref Name).
type SnapshotContentVisit func(ctx context.Context, content *storagev1alpha1.SnapshotContent) error

// WalkSnapshotContentSubtree visits every SnapshotContent reachable from rootContentName following
// only status.childrenSnapshotContentRefs. It never lists SnapshotContent to infer children.
func WalkSnapshotContentSubtree(
	ctx context.Context,
	c client.Reader,
	rootContentName string,
	visit SnapshotContentVisit,
) error {
	visited := make(map[string]struct{})
	return walkSnapshotContentSubtree(ctx, c, rootContentName, visited, visit)
}

func walkSnapshotContentSubtree(
	ctx context.Context,
	c client.Reader,
	contentName string,
	visited map[string]struct{},
	visit SnapshotContentVisit,
) error {
	if _, ok := visited[contentName]; ok {
		return fmt.Errorf("%w at SnapshotContent %q", ErrSnapshotContentCycle, contentName)
	}
	visited[contentName] = struct{}{}

	content := &storagev1alpha1.SnapshotContent{}
	if err := c.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("SnapshotContent %q not found: %w", contentName, err)
		}
		return fmt.Errorf("get SnapshotContent %q: %w", contentName, err)
	}

	if err := visit(ctx, content); err != nil {
		return err
	}

	children := append([]storagev1alpha1.SnapshotContentChildRef(nil), content.Status.ChildrenSnapshotContentRefs...)
	sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })
	for i := range children {
		if children[i].Name == "" {
			continue
		}
		if err := walkSnapshotContentSubtree(ctx, c, children[i].Name, visited, visit); err != nil {
			return err
		}
	}
	return nil
}
