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

package volumecapture

import (
	"context"
	"errors"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	vcctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// ErrDuplicateCoveredPVCUID is returned when the same PVC UID is claimed in more than one descendant SnapshotContent.
var ErrDuplicateCoveredPVCUID = errors.New("duplicate covered PVC UID in snapshot subtree")

// ErrSubtreeDataRefsPending is returned when descendant volume coverage is not yet observable (no dataRefs and no pending VCR targets).
var ErrSubtreeDataRefsPending = errors.New("subtree data volume coverage pending")

// CollectSubtreeCoveredPVCUIDs returns PVC UIDs already covered by descendant SnapshotContent nodes
// (status.dataRefs[] and in-flight VCR spec.targets[]). The root content itself is not included.
func CollectSubtreeCoveredPVCUIDs(
	ctx context.Context,
	c client.Reader,
	namespace string,
	rootContent *storagev1alpha1.SnapshotContent,
) (map[types.UID]struct{}, error) {
	if rootContent == nil {
		return nil, fmt.Errorf("root SnapshotContent is required")
	}
	if namespace == "" {
		return nil, fmt.Errorf("namespace is required for subtree PVC coverage")
	}
	covered := make(map[types.UID]struct{})
	uidOwner := make(map[types.UID]string)

	visit := func(_ context.Context, content *storagev1alpha1.SnapshotContent) error {
		if content.Name == rootContent.Name {
			return nil
		}
		// Orphan/residual-PVC VolumeSnapshot children are ordinary domain content now (content-single-writer
		// design §11.6): the aggregator projects their status.data from the bound VSC, so they cover their own
		// PVC UID here like every other data-bearing node — there is no visibility-leaf carve-out. (The full
		// coverage rewrite — CSD RequiresDataArtifact + native-CSI snapshotSource fallback — lands in Block 5.)
		uids, err := coveredPVCUIDsForContent(ctx, c, namespace, content)
		if err != nil {
			return err
		}
		for _, uid := range uids {
			if uid == "" {
				continue
			}
			parsed := types.UID(uid)
			if prev, dup := uidOwner[parsed]; dup {
				return fmt.Errorf("%w: %s (SnapshotContent %q and %q)", ErrDuplicateCoveredPVCUID, uid, prev, content.Name)
			}
			uidOwner[parsed] = content.Name
			covered[parsed] = struct{}{}
		}
		return nil
	}

	if err := walkSnapshotContentSubtree(ctx, c, rootContent.Name, visit); err != nil {
		return nil, err
	}
	return covered, nil
}

type snapshotContentVisit func(ctx context.Context, content *storagev1alpha1.SnapshotContent) error

// walkSnapshotContentSubtree mirrors usecase.WalkSnapshotContentSubtree without an import cycle.
func walkSnapshotContentSubtree(
	ctx context.Context,
	c client.Reader,
	rootContentName string,
	visit snapshotContentVisit,
) error {
	visited := make(map[string]struct{})
	return walkSnapshotContentSubtreeRec(ctx, c, rootContentName, visited, visit)
}

func walkSnapshotContentSubtreeRec(
	ctx context.Context,
	c client.Reader,
	contentName string,
	visited map[string]struct{},
	visit snapshotContentVisit,
) error {
	if _, ok := visited[contentName]; ok {
		return fmt.Errorf("SnapshotContent graph cycle at %q", contentName)
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
		if err := walkSnapshotContentSubtreeRec(ctx, c, children[i].Name, visited, visit); err != nil {
			return err
		}
	}
	return nil
}

func coveredPVCUIDsForContent(
	ctx context.Context,
	c client.Reader,
	namespace string,
	content *storagev1alpha1.SnapshotContent,
) ([]string, error) {
	fromDataRefs, err := pvcUIDsFromSnapshotContentDataRefs(content)
	if err != nil {
		return nil, err
	}
	if len(fromDataRefs) > 0 {
		return fromDataRefs, nil
	}
	hasChildren := len(content.Status.ChildrenSnapshotContentRefs) > 0
	if hasChildren {
		return nil, nil
	}
	fromVCR, err := pvcUIDsFromPendingVCR(ctx, c, namespace, content.UID)
	if err != nil {
		return nil, err
	}
	if len(fromVCR) > 0 {
		return fromVCR, nil
	}
	// Manifest-only leaf (no volume leg yet): contributes no covered PVC UIDs.
	return nil, nil
}

func pvcUIDsFromSnapshotContentDataRefs(content *storagev1alpha1.SnapshotContent) ([]string, error) {
	refs := content.DataList()
	if len(refs) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(refs))
	for i := range refs {
		b := refs[i]
		uid := string(b.Source.UID)
		if uid == "" {
			return nil, fmt.Errorf("SnapshotContent %q data: empty source uid", content.Name)
		}
		out = append(out, uid)
	}
	return out, nil
}

func pvcUIDsFromPendingVCR(ctx context.Context, c client.Reader, namespace string, contentUID types.UID) ([]string, error) {
	if contentUID == "" {
		return nil, nil
	}
	name := vcpkg.SnapshotContentVCRName(contentUID)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get VolumeCaptureRequest %s/%s: %w", namespace, name, err)
	}
	targets, err := vcctrl.ParseVolumeCaptureTargets(obj)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.UID != "" {
			out = append(out, t.UID)
		}
	}
	return out, nil
}
